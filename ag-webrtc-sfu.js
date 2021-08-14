"use strict";

(function (g, console, mediaDevices) {
    const PARKING = 'PARKING';
    const VERSION = '1.0';

    function fireEvent(sfu, eventName, payload) {
        console.debug("fireEvent, eventName: ", eventName);
        let cb = sfu._listeners[eventName];
        if (cb)
            cb(payload);
    }

    function unexpectedWsConnectionTermination(sfu) {
        sfu._ws = undefined;
        if (sfu._pc)
            sfu._pc.close();
        sfu._pc = undefined;
        if (sfu._retryCount > sfu.MAX_RETRY_COUNT) {
            console.warn("Retried enough times. No more.");
            sfu.endSession(true);
            return;
        }
        sfu._retryCount++;
        sfu.startSession(sfu._roomId);
    }

    function onSocketMessage(sfu, event) {
        let msg = JSON.parse(event.data)
        if (!msg) {
            return console.warn('Failed to parse msg');
        }
        const ws = sfu._ws;
        const pc = sfu._pc;

        switch (msg.event) {
            case 'login':
                console.debug("onSocketMessage, login");
                if (!sfu._token) {
                    console.error("Cannot reply to login, as no token is available!");
                    return
                }
                sfu._peerId = msg.data;
                ws.send(JSON.stringify({
                    event: "login-reply",
                    data: JSON.stringify({
                        token: sfu._token,
                        peer_name: sfu._peerName,
                        token_hint: sfu._tokenHint})
                }));
                sfu._token = undefined; // Tokens are valid for limited time. So no use storing them once used.
                return

            case 'offer':
                console.debug("onSocketMessage, offer");
                let offer = JSON.parse(msg.data);
                if (!offer) {
                    return console.warn('Failed to parse offer')
                }
                pc.setRemoteDescription(offer);
                pc.createAnswer().then(answer => {
                    pc.setLocalDescription(answer);
                    // Before sending the answer let's send our audio state broadcast
                    ws.send(JSON.stringify({event: 'audio-muted', data: (!sfu._audioEnabled).toString()}));
                    ws.send(JSON.stringify({event: 'answer', data: JSON.stringify(answer)}));
                });
                return

            case 'candidate':
                console.debug("onSocketMessage, candidate");
                let candidate = JSON.parse(msg.data);
                if (!candidate) {
                    return console.warn('Failed to parse candidate');
                }

                pc.addIceCandidate(candidate);
                return

            case 'track-meta': {
                console.debug("onSocketMessage, track-meta");
                let {peer_id, id, peer_name} = JSON.parse(msg.data);
                if (!peer_id || !id) {
                    return console.warn('Failed to parse track-meta');
                }
                let peer = sfu._remotePeers[peer_id];
                let remoteTrackData;
                if (!peer) {
                    remoteTrackData = sfu._remotePeers[PARKING][id];
                    if (remoteTrackData) {
                        peer = {
                            peerName: peer_name,
                            tracks: {
                                [id]: remoteTrackData
                            }
                        };
                        delete sfu._remotePeers[PARKING][id];
                    } else {
                        // Before adding this peer-id pair lets remove this id from other peers, if present.
                        // In those cases this is the new peer id of the same peer.
                        const peerIds = Object.keys(sfu._remotePeers);
                        peerIds.forEach((peerId) => {
                            const peer = sfu._remotePeers[peerId];
                            if (peerId === PARKING)
                                return
                            if (peer.tracks !== true && peer.tracks[id]) {
                                delete peer.tracks[id];
                                if (Object.keys(peer.tracks).length === 0) {
                                    delete sfu._remotePeers[peerId];
                                }
                            }
                        });
                        peer = {
                            peerName: peer_name,
                            tracks: {}
                        };
                    }
                    sfu._remotePeers[peer_id] = peer;
                    fireEvent(sfu, "peer-add", {peerId: peer_id, peerName: peer_name});
                } else {
                    peer.peerName = peer_name;
                }
                remoteTrackData = peer.tracks[id];
                if (!remoteTrackData) {
                    peer.tracks[id] = true;
                    return
                } else if (!remoteTrackData.stream) {
                    console.warn("Remote track data for id " + id + " not set");
                    return
                }
                fireEvent(sfu, "add-peer-track", {
                    stream: remoteTrackData.stream,
                    track: remoteTrackData.track,
                    peerId: peer_id,
                    isMuted: remoteTrackData.track.kind === 'audio'? !remoteTrackData.track.enabled : undefined
                });
                return
            }
            case 'peer-gone':
                let peerId = msg.data;
                console.debug("onSocketMessage, peer-gone: ", peerId);
                if (!peerId) {
                    return console.warn('Invalid peer-gone');
                }
                let remotePeer = sfu._remotePeers[peerId];
                if (remotePeer) {
                    delete sfu._remotePeers[peerId];
                }
                fireEvent(sfu, "peer-gone", {peerId});
                return

            case 'audio-muted': {
                console.debug("onSocketMessage, audio-muted");
                let {peer_id, is_muted} = JSON.parse(msg.data);
                if (!peer_id || typeof is_muted === 'undefined') {
                    return console.warn('Invalid audio-muted');
                }
                if (sfu._remotePeers[peer_id])
                    fireEvent(sfu, "peer-muted", {peerId: peer_id, isMuted: is_muted});
                else {
                    // This peer has not joined yet
                    let state = sfu._remotePeersStateCahce[peer_id];
                    if (!state) {
                        state = {};
                        sfu._remotePeersStateCahce[peer_id] = state;
                    }
                    state.isMuted = is_muted;
                }
                return
            }
        }
    }

    function restartSession(sfu) {
        const roomId = sfu._roomId;
        sfu.endSession(false, true);
        sfu.startSession(roomId);
    }

    // ---- Sfu definition -----
    function Sfu(peerName, connectionStringResolver) {
        this._peerName = peerName;
        this._videoEnabled = true;
        this._audioEnabled = true;
        this._peerId = undefined;
        this._listeners = {};
        this._retryCount = 0;
        this._connectionStringResolver = connectionStringResolver;
        this._remotePeers = {
            [PARKING]: {}
        };
        this._remotePeersStateCahce = {};
    }
    Sfu.prototype = {
        MAX_RETRY_COUNT: 5,
        VERSION
    };
    Sfu.prototype.isInSession = function () {
        return !!this._pc;
    };
    Sfu.prototype.startSession = function (roomId) {
        if (this._start_session_in_progress)
            return
        if (this.isInSession())
            this.endSession();
        setTimeout(async () => { // This is needed so that the ws.close handler gets to run if this called is preceded by endSession (as in restartSession).
            console.debug("startSession for room: ", roomId);
            if (!roomId) {
                console.debug("No room id");
                debugger
            }
            this._start_session_in_progress = true;
            this._roomId = roomId;
            const {socketUrl, token, tokenHint=""} = await this._connectionStringResolver(roomId);
            this._token = token;
            this._tokenHint = tokenHint;
            const pc = new RTCPeerConnection();
            this._pc = pc;

            let disableAudio = false;
            if (!this._audioEnabled)
                disableAudio = true;
            mediaDevices.getUserMedia({ video: this._videoEnabled, audio: true }).then((stream) => {
                this._localStream = stream;
                let isDone = false;
                stream.getTracks().forEach(track => {
                    this._pc.addTrack(track, stream);
                    if (track.kind === 'audio') {
                        if (disableAudio)
                            track.enabled = false;
                    }
                    track.onended = () => {
                        if (track.kind === 'video')
                            isDone = true;
                        fireEvent(this, "remove-local-track", {stream, track});
                    };
                    fireEvent(this, "add-local-track", {stream, track, isMuted: track.kind === 'audio'? !track.enabled : undefined});
                });
            }).catch(error => {
                console.error(error);
                fireEvent(this, "user-device-error", {error});
            }).finally(() => {
                this._ws = new WebSocket(socketUrl);
                this._ws.onclose = (event) => {
                    if (event.code !== 1000) {
                        unexpectedWsConnectionTermination(this);
                    } else {
                        this._retryCount = 0;
                        if (this._end_session_was_trigger)
                            this.endSession(false);
                    }
                };
                this._ws.onerror = unexpectedWsConnectionTermination.bind(null, this);
                this._ws.onmessage = onSocketMessage.bind(null, this);
            });

            pc.ontrack = (event) => {
                console.debug("_pc.ontrack", event);
                event.streams[0].addEventListener("removetrack", (trackEvent) => {
                    fireEvent(this, "remove-peer-track", {stream:event.streams[0], track:trackEvent.track});
                    const id = event.streams[0].id + '';
                    const peerIds = Object.keys(this._remotePeers);
                    peerIds.forEach((peerId) => {
                        const peer = this._remotePeers[peerId];
                        if (peerId === PARKING) {
                            if (peer[id]) {
                                delete peer[id];
                            }
                        } else {
                            if (peer.tracks !== true && peer.tracks[id]) {
                                delete peer.tracks[id];
                                if (Object.keys(peer.tracks).length === 0) {
                                    delete this._remotePeers[peerId];
                                    // Peer Ids might change but Id may not so there could be multiple peers mapping to same Id in tracks{}. So we need to clean all of them.
                                }
                            }
                        }
                    });
                });
                const id = event.streams[0].id + '';
                const remoteTrackData = {
                    stream: event.streams[0],
                    track: event.track
                };
                let found = false;
                for (const peerId in this._remotePeers) {
                    if (peerId === PARKING)
                        continue
                    const peer = this._remotePeers[peerId];
                    if (peer.tracks[id]) {
                        peer.tracks[id] = remoteTrackData;
                        found = true;
                        let isMuted;
                        if (remoteTrackData.track.kind === 'audio' && this._remotePeersStateCahce[peerId]) {
                            isMuted = this._remotePeersStateCahce[peerId].isMuted;
                            delete this._remotePeersStateCahce[peerId];
                        }
                        fireEvent(this, "add-peer-track", {
                            stream:remoteTrackData.stream,
                            track:remoteTrackData.track,
                            peerId,
                            isMuted});
                        break;
                    }
                }
                if (!found) {
                    this._remotePeers[PARKING][id] = remoteTrackData;
                }
            };
            pc.onicecandidate = event => {
                if (!event.candidate) {
                    return
                }

                this._ws.send(JSON.stringify({event: 'candidate', data: JSON.stringify(event.candidate)}));
            };
            pc.onsignalingstatechange = () => {
                console.debug("_pc.onsignalingstatechange", pc.signalingState, pc.connectionState);
                if (pc.signalingState === "closed" || pc.connectionState === "closed") {
                    this.endSession(false);
                }
            };

            this._start_session_in_progress = false;
        }, 1);
    };
    Sfu.prototype.endSession = function (isAbnormal, skipEvent) {
        isAbnormal = !!isAbnormal;
        console.debug("endSession, isAbnormal: ", isAbnormal);
        if (this._ws) {
            this._end_session_was_trigger = true;
            this._ws.close(isAbnormal ? 1006 : 1000);
            this._end_session_was_trigger = false;
        }
        if (this._pc)
            this._pc.close();
        if (this._localStream)
            this._localStream.getTracks().forEach((track) => {
                track.stop();
            });
        this._localStream = undefined;
        this._roomId = undefined;
        this._pc = undefined;
        this._ws = undefined;
        this._token = undefined;
        this._retryCount = 0;
        if (!skipEvent)
            fireEvent(this, "end-session", {isAbnormal})
    };
    Sfu.prototype.setVideo = function ({enabled = true}) {
        if (enabled !== this._videoEnabled) {
            this._videoEnabled = enabled;
            if (this.isInSession()) {
                // Video switch does not happen often so we take this restartSession approach.

                // this._pc.getSenders().forEach((sender) => {
                //     if (sender.track && sender.track.kind === 'video')
                //         sender.track.enabled = enabled;
                // });
                restartSession(this);
            }
        }
    };
    Sfu.prototype.setAudio = function ({enabled = true}) {
        if (enabled !== this._audioEnabled) {
            this._audioEnabled = enabled;
            if (this.isInSession()) {
                // Audio toggle can happen many times a session so we chose to use this
                // approach here, which sends out zero volume audio frames on disable.
                this._pc.getSenders().forEach((sender) => {
                    if (sender.track && sender.track.kind === 'audio')
                        sender.track.enabled = enabled;
                });
                this._ws.send(JSON.stringify({event: 'audio-muted', data: (!enabled).toString()}));
                // restartSession(this);
            }
        }
    };
    Sfu.prototype.onEvent = function (eventName, callback) {
        if (this._listeners[eventName]) {
            console.warn(`callback for ${eventName} was already registered. That will now get overridden.`);
            debugger
        }
        this._listeners[eventName] = callback;
    };

    g.AgWebrtcSfu = Sfu;
}(window, window.console, window.navigator.mediaDevices));
