package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/golang-jwt/jwt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/satori/go.uuid"
)

var (
	addr      = flag.String("addr", ":9000", "http service address")
	isDevMode = flag.Bool("dev", false, "is dev mode enabled")
	isVerbose = flag.Bool("verbose", false, "is verbose logging enabled")
	upgrader  = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	roomCollectionsLock sync.RWMutex
	roomCollections     map[string]*roomCollection
	totalRooms          uint
	totalPeers          uint
)

type roomCollection struct {
	// lock for peerConnections and trackLocals
	id              string
	listLock        sync.RWMutex
	peerConnections []peerConnectionState
	trackLocals     map[string]*localTrackData
}

type localTrackData struct {
	track        *webrtc.TrackLocalStaticRTP
	remotePeerId string
}

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type peerConnectionState struct {
	peerConnection *webrtc.PeerConnection
	websocket      *threadSafeWriter
	peerId         string
}

func debugLog(v ...interface{}) {
	if *isDevMode || *isVerbose {
		log.Println(v)
	}
}

func verboseLog(v ...interface{}) {
	if *isVerbose {
		log.Println(v)
	}
}

func main() {
	// Parse the flags passed to program
	flag.Parse()

	// Init other state
	log.SetFlags(0)

	roomCollections = map[string]*roomCollection{}

	if *isDevMode {
		setupDevMode()
	}

	// websocket handler
	http.HandleFunc("/websocket", websocketHandler)

	http.HandleFunc("/get.stats", statsHandler)

	// request a keyframe every 3 seconds
	go func() {
		for range time.NewTicker(time.Second * 3).C {
			var roomsSnapshot []*roomCollection
			roomCollectionsLock.Lock()
			for _, room := range roomCollections {
				roomsSnapshot = append(roomsSnapshot, room)
			}
			roomCollectionsLock.Unlock()
			for _, room := range roomsSnapshot {
				dispatchKeyFrame(room)
			}
		}
	}()

	// start HTTP server
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	stats := struct {
		TotalRooms uint     `json:"total-rooms"`
		TotalPeers uint     `json:"total-peers"`
		RoomIds    []string `json:"room-ids,omitempty"`
	}{totalRooms, totalPeers, nil}

	details, present := r.URL.Query()["details"]
	if present && len(details) > 0 && details[0] == "true" {
		roomCollectionsLock.Lock()
		rooms := make([]string, 0, len(roomCollections))
		for r := range roomCollections {
			rooms = append(rooms, r)
		}
		roomCollectionsLock.Unlock()
		stats.RoomIds = rooms
	}

	js, err := json.Marshal(stats)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Fatal(err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(js)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Fatal(err)
		return
	}
}

// Add to list of tracks and fire renegotiation for all PeerConnections
func addTrack(room *roomCollection, t *webrtc.TrackRemote, peerId string) *webrtc.TrackLocalStaticRTP {
	room.listLock.Lock()
	defer func() {
		room.listLock.Unlock()
		signalPeerConnections(room)
	}()

	// Create a new TrackLocal with the same codec as our incoming
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	room.trackLocals[t.ID()] = &localTrackData{trackLocal, peerId}
	return trackLocal
}

// Remove from list of tracks and fire renegotiation for all PeerConnections
func removeTrack(room *roomCollection, t *webrtc.TrackLocalStaticRTP) {
	room.listLock.Lock()
	defer func() {
		room.listLock.Unlock()
		signalPeerConnections(room)
	}()

	delete(room.trackLocals, t.ID())
}

// signalPeerConnections updates each PeerConnection so that it is getting all the expected media tracks
func signalPeerConnections(room *roomCollection) {
	deleteRoom := false
	room.listLock.Lock()
	debugLog("signalPeerConnections for room: ", room.id)
	defer func() {
		room.listLock.Unlock()
		dispatchKeyFrame(room)
		if deleteRoom {
			roomCollectionsLock.Lock()
			if _, present := roomCollections[room.id]; present {
				delete(roomCollections, room.id)
				totalRooms--
			}
			roomCollectionsLock.Unlock()
		}
	}()

	attemptSync := func() (tryAgain bool) {
		for i := range room.peerConnections {
			if room.peerConnections[i].peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
				room.peerConnections = append(room.peerConnections[:i], room.peerConnections[i+1:]...)
				return true // We modified the slice, start from the beginning
			}

			// map of sender we already are sending, so we don't double send
			existingSenders := map[string]bool{}

			for _, sender := range room.peerConnections[i].peerConnection.GetSenders() {
				if sender.Track() == nil {
					continue
				}

				existingSenders[sender.Track().ID()] = true

				// If we have a RTPSender that doesn't map to a existing track remove and signal
				if _, ok := room.trackLocals[sender.Track().ID()]; !ok {
					if err := room.peerConnections[i].peerConnection.RemoveTrack(sender); err != nil {
						return true
					}
				}
			}

			// Don't receive videos we are sending, make sure we don't have loop-back
			for _, receiver := range room.peerConnections[i].peerConnection.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}

				existingSenders[receiver.Track().ID()] = true
			}

			// Add all track we aren't sending yet to the PeerConnection
			for trackID := range room.trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					if _, err := room.peerConnections[i].peerConnection.AddTrack(room.trackLocals[trackID].track); err != nil {
						return true
					}
					debugLog("Added local track to peer: ", room.peerConnections[i].peerId, " in room: ", room.id,
						" with stream id: ", room.trackLocals[trackID].track.StreamID(),
						" for remote peer: ", room.trackLocals[trackID].remotePeerId)

					trackMeta, err := json.Marshal(struct {
						Id     string `json:"id"`
						PeerId string `json:"peer_id"`
					}{room.trackLocals[trackID].track.StreamID(), room.trackLocals[trackID].remotePeerId})
					if err != nil {
						log.Println(err)
						return
					}

					if writeErr := room.peerConnections[i].websocket.WriteJSON(&websocketMessage{
						Event: "track-meta",
						Data:  string(trackMeta),
					}); writeErr != nil {
						log.Println(writeErr)
					}
				}
			}

			offer, err := room.peerConnections[i].peerConnection.CreateOffer(nil)
			if err != nil {
				return true
			}

			if err = room.peerConnections[i].peerConnection.SetLocalDescription(offer); err != nil {
				return true
			}

			offerString, err := json.Marshal(offer)
			if err != nil {
				return true
			}

			verboseLog("Offer: ", offer.SDP, " for peer: ", room.peerConnections[i].peerId)
			if err = room.peerConnections[i].websocket.WriteJSON(&websocketMessage{
				Event: "offer",
				Data:  string(offerString),
			}); err != nil {
				return true
			}
			debugLog("Sending offer to peer: ", room.peerConnections[i].peerId, " of room: ", room.id)
		}

		return
	}

	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 25 {
			// Release the lock and attempt a sync in 3 seconds. We might be blocking a RemoveTrack or AddTrack
			go func() {
				time.Sleep(time.Second * 3)
				signalPeerConnections(room)
			}()
			return
		}

		if !attemptSync() {
			break
		}
	}

	if len(room.peerConnections) == 0 {
		// No peers in the room. Let's cleanup this room.
		deleteRoom = true
	}
}

// dispatchKeyFrame sends a keyframe to all PeerConnections, used everytime a new user joins the call
func dispatchKeyFrame(room *roomCollection) {
	room.listLock.Lock()
	defer room.listLock.Unlock()
	//debugLog("dispatchKeyFrame for room: ", room.id)

	for i := range room.peerConnections {
		for _, receiver := range room.peerConnections[i].peerConnection.GetReceivers() {
			if receiver.Track() == nil {
				continue
			}

			_ = room.peerConnections[i].peerConnection.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}

// Handle incoming websockets
func websocketHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade HTTP request to Websocket
	unsafeConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}

	c := &threadSafeWriter{unsafeConn, sync.Mutex{}}

	defer func() {
		totalPeers--
	}()

	// When this frame returns close the Websocket
	defer c.Close() //nolint

	peerId := uuid.NewV1().String()
	if writeErr := c.WriteJSON(&websocketMessage{
		Event: "login",
		Data:  peerId,
	}); writeErr != nil {
		log.Println(writeErr)
		return
	}

	message := &websocketMessage{}

	_, raw, err := c.ReadMessage()
	if err != nil {
		log.Println(err)
		return
	} else if err := json.Unmarshal(raw, &message); err != nil {
		log.Println(err)
		return
	}

	if message.Event != "login-reply" {
		log.Println("Invalid login-reply by remote: " + message.Event)
		return
	}

	loginData := struct {
		Token     string `json:"token"`
		TokenHint string `json:"token_hint"`
	}{}
	if err := json.Unmarshal([]byte(message.Data), &loginData); err != nil {
		log.Println(err)
		return
	}
	roomId, err := validateTokenAndGetRoomId(loginData.Token, loginData.TokenHint, getTokenKey)
	if err != nil {
		log.Println("Provided token: " + loginData.Token)
		log.Println(err)
		return
	}
	roomCollectionsLock.Lock()
	room, ok := roomCollections[roomId]
	if !ok {
		roomCollections[roomId] = &roomCollection{id: roomId, peerConnections: []peerConnectionState{}, trackLocals: map[string]*localTrackData{}}
		room, _ = roomCollections[roomId]
		totalRooms++
		debugLog("Added new room: ", room.id)
	} else {
		debugLog("Re-fetched room: ", room.id)
	}
	roomCollectionsLock.Unlock()

	// Create new PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Print(err)
		return
	}
	debugLog("New peer connection for room: ", room.id, " with peerId: ", peerId)

	// When this frame returns close the PeerConnection
	defer peerConnection.Close() //nolint

	defer broadcastToOtherPeersInRoom(room, peerId, &websocketMessage{
		Event: "peer-gone",
		Data:  peerId,
	})

	// Accept one audio and one video track incoming
	for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := peerConnection.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			log.Print(err)
			return
		}
	}

	// Add our new PeerConnection to global list
	room.listLock.Lock()
	room.peerConnections = append(room.peerConnections, peerConnectionState{peerConnection, c, peerId})
	totalPeers++
	room.listLock.Unlock()

	// Trickle ICE. Emit server candidate to client
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		debugLog("peerConnection.OnICECandidate for a member of room: ", room.id)

		candidateString, err := json.Marshal(i.ToJSON())
		if err != nil {
			log.Println(err)
			return
		}

		if writeErr := c.WriteJSON(&websocketMessage{
			Event: "candidate",
			Data:  string(candidateString),
		}); writeErr != nil {
			log.Println(writeErr)
		}
	})

	// If PeerConnection is closed remove it from global list
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		debugLog("peerConnection.OnConnectionStateChange for peer: ", peerId, " of room: ", room.id, " new state: ", p.String())
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				log.Print(err)
			}
		case webrtc.PeerConnectionStateClosed:
			signalPeerConnections(room)
		}
	})

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		debugLog("peerConnection.OnTrack for peer: ", peerId, " of room: ", room.id, " with track id: ", t.ID())
		// Create a track to fan out our incoming video to all peers
		trackLocal := addTrack(room, t, peerId)
		defer removeTrack(room, trackLocal)

		buf := make([]byte, 1500)
		for {
			i, _, err := t.Read(buf)
			if err != nil {
				return
			}

			if _, err = trackLocal.Write(buf[:i]); err != nil {
				return
			}
		}
	})

	// Signal for the new PeerConnection
	signalPeerConnections(room)

	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		} else if err := json.Unmarshal(raw, &message); err != nil {
			log.Println(err)
			return
		}

		switch message.Event {
		case "candidate":
			candidate := webrtc.ICECandidateInit{}
			if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
				log.Println(err)
				return
			}

			if err := peerConnection.AddICECandidate(candidate); err != nil {
				log.Println(err)
				return
			}
		case "answer":
			answer := webrtc.SessionDescription{}
			if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
				log.Println(err)
				return
			}
			debugLog("Got answer from peer: ", peerId, " of room: ", roomId)
			verboseLog("Answer: ", answer.SDP, " from peer: ", peerId)

			if err := peerConnection.SetRemoteDescription(answer); err != nil {
				log.Println(err)
				return
			}
		}
	}
}

func validateTokenAndGetRoomId(tokenString string, tokenHint string, tokenKeyFetcher func(tokenHint string) (string, error)) (string, error) {
	token, err := jwt.ParseWithClaims(tokenString, &jwt.StandardClaims{}, func(token *jwt.Token) (interface{}, error) {
		// Don't forget to validate the alg is what you expect:
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		var key string
		var err error
		if key, err = tokenKeyFetcher(tokenHint); err != nil {
			return nil, err
		}

		return []byte(key), nil
	})

	if err != nil {
		return "", err
	}

	if claims, ok := token.Claims.(*jwt.StandardClaims); ok && token.Valid {
		if err := claims.Valid(); err != nil {
			return "", err
		}
		return claims.Subject, nil
	} else {
		return "", fmt.Errorf("invalid token: %+v", token)
	}
}

func getTokenKey(tokenHint string) (string, error) {
	return os.Getenv("AG_WEBRTC_SFU_KEY"), nil
}

// Helper to make Gorilla Websockets thread-safe
type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}

func (t *threadSafeWriter) WriteJSON(v interface{}) error {
	t.Lock()
	defer t.Unlock()

	return t.Conn.WriteJSON(v)
}

func broadcastToOtherPeersInRoom(room *roomCollection, fromPeerId string, message *websocketMessage) {
	debugLog("broadcastToOtherPeersInRoom, fromPeerId: ", fromPeerId, " message: ", *message)
	room.listLock.Lock()
	for _, peerConn := range room.peerConnections {
		if peerConn.peerId != fromPeerId {
			if writeErr := peerConn.websocket.WriteJSON(message); writeErr != nil {
				log.Println(writeErr)
			}
		}
	}
	room.listLock.Unlock()
}
