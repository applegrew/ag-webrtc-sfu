# ag-webrtc-sfu
This is a many-to-many websocket based SFU. This has the following features.

* Trickle ICE
* Re-negotiation
* Basic RTCP
* Multiple inbound/outbound tracks per PeerConnection
* No codec restriction per call. You can have H264 and VP8 in the same conference.
* Support for multiple browsers
* Concept of rooms. The conference tracks are forwarded within a given room.
* Supports JWT based authentication to connect to websocket to initiate Webrtc peer connection.

It has a npm module to easily get the `ag-webrtc-sfu.js` file from here for use on client-sides.


## Instructions
### Download
This code can be run in dev mode which requires you to clone the repo since it will be serving static HTML and a JS file. If you do not use the `-dev` option then the HTML & JS files won't be served.

```
git clone git@github.com:applegrew/ag-webrtc-sfu.git
cd ag-webrtc-sfu
```

### Run sfu-ws
Execute `go build` then `./ag-webrtc-sfu -dev -addr=:9900`

### Open the Web UI
Open [http://localhost:9900](http://localhost:9900). This will automatically connect and send your video. Now join from other tabs and browsers!

