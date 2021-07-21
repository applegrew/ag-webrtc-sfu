package main

import (
	"encoding/json"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt"
)

func setupDevMode() {

	// index.html handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Let's fetch this everytime so that we don't have to
		// restart this service after every change to index.html
		indexHTML, err := ioutil.ReadFile("index.html")
		if err != nil {
			panic(err)
		}
		indexTemplate := &template.Template{}
		indexTemplate = template.Must(template.New("").Parse(string(indexHTML)))
		if err := indexTemplate.Execute(w, "ws://"+r.Host+"/websocket"); err != nil {
			log.Fatal(err)
		}
	})

	http.HandleFunc("/get.token", func(w http.ResponseWriter, r *http.Request) {
		roomId, present := r.URL.Query()["roomId"]
		if present {
			claims := jwt.StandardClaims{ExpiresAt: time.Now().Unix() + 2*60, Subject: roomId[0]}
			token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			ss, err := token.SignedString([]byte(os.Getenv("AG_WEBRTC_SFU_KEY")))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				log.Println("Got key: '" + os.Getenv("AG_WEBRTC_SFU_KEY") + "'")
				log.Fatal(err)
				return
			}
			js, err := json.Marshal(struct {
				Token string `json:"token"`
			}{ss})
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
	})

	http.HandleFunc("/ag-webrtc-sfu.js", func(writer http.ResponseWriter, request *http.Request) {
		http.ServeFile(writer, request, "ag-webrtc-sfu.js")
	})

	log.Println("Dev mode is on")
}
