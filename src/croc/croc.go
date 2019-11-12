package croc

import (
	"crypto/elliptic"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v2"
	"github.com/schollz/croc/v7/src/box"
	"github.com/schollz/croc/v7/src/crypt"
	log "github.com/schollz/logger"
	"github.com/schollz/pake/v2"
)

// Debug toggles debug mode
func Debug(debug bool) {
	if debug {
		log.SetLevel("debug")
	} else {
		log.SetLevel("warn")
	}
}

// Options specifies user specific options
type Options struct {
	IsSender     bool
	SharedSecret string
	Debug        bool
	RelayAddress string
	Stdout       bool
	NoPrompt     bool
	DisableLocal bool
	Ask          bool
}

// Client holds the state of the croc transfer
type Client struct {
	// connections
	ws  *websocket.Conn
	rtc *webrtc.PeerConnection

	// options
	Options Options

	// security
	Pake *pake.Pake
	Key  []byte

	// steps involved in forming relationship
	Step1ChannelSecured bool
	IsOfferer           bool
}

// TransferOptions for sending
type TransferOptions struct {
	PathToFiles      []string
	KeepPathInRemote bool
}

type WebsocketMessage struct {
	Message string
	Payload string
}

// New establishes a new connection for transferring files between two instances.
func New(ops Options) (c *Client, err error) {
	c = new(Client)

	// setup basic info
	c.Options = ops
	if c.Options.Debug {
		log.SetLevel("debug")
	} else {
		log.SetLevel("info")
	}

	// connect to relay and exchange info
	err = c.connectToRelay()
	return
}

// Send will send the specified file
func (c *Client) Send(options TransferOptions) (err error) {
	return
}

// Receive will receive the file
func (c *Client) Receive() (err error) {
	return
}

func (c *Client) connectToRelay() (err error) {
	// connect to relay
	websocketURL := c.Options.RelayAddress + "/test1"
	log.Debugf("dialing %s", websocketURL)
	c.ws, _, err = websocket.DefaultDialer.Dial(websocketURL, nil)
	if err != nil {
		log.Error("dial:", err)
		return
	}

	// // create webrtc connection
	finished := make(chan error, 1)
	c.rtc, err = c.CreateOfferer(finished)
	if err != nil {
		log.Error(err)
	}

	log.Debugf("connected and sending first message")
	bundled, err := box.Bundle(WebsocketMessage{
		Message: "[1] you are offerer",
	}, c.Key)
	if err != nil {
		log.Error(err)
		return
	}
	err = c.ws.WriteMessage(1, []byte(bundled))
	if err != nil {
		log.Error(err)
		return
	}

	var setKey []byte
	setKey = nil
	for {
		if setKey != nil {
			c.Key = setKey
		}
		var wsmsg, wsreply WebsocketMessage
		var msg []byte
		_, msg, err = c.ws.ReadMessage()
		if err != nil {
			log.Debug("read:", err)
			return
		}
		err = box.Unbundle(string(msg), c.Key, &wsmsg)
		log.Debugf("recv: %s", wsmsg.Message)
		if wsmsg.Message == "[1] you are offerer" {
			c.IsOfferer = true
			c.Pake, err = pake.Init([]byte(c.Options.SharedSecret), 0, elliptic.P521(), 1*time.Microsecond)
			if err != nil {
				log.Error(err)
				return
			}
			wsreply.Message = "[2] you are answerer"
		} else if wsmsg.Message == "[2] you are answerer" {
			c.IsOfferer = false
			c.Pake, err = pake.Init([]byte(c.Options.SharedSecret), 1, elliptic.P521(), 1*time.Microsecond)
			if err != nil {
				log.Error(err)
				return
			}
			wsreply.Message = "[3] pake1"
			wsreply.Payload = base64.StdEncoding.EncodeToString(c.Pake.Bytes())
		} else if wsmsg.Message == "[3] pake1" || wsmsg.Message == "[4] pake2" || wsmsg.Message == "[5] pake3" {
			var pakeBytes []byte
			pakeBytes, err = base64.StdEncoding.DecodeString(wsmsg.Payload)
			if err != nil {
				log.Error(err)
				return
			}
			err = c.Pake.Update(pakeBytes)
			if err != nil {
				log.Debugf("pakeBytes: %s", pakeBytes)
				log.Error(err)
				return
			}
			if wsmsg.Message == "[3] pake1" {
				wsreply.Message = "[4] pake2"
				wsreply.Payload = base64.StdEncoding.EncodeToString(c.Pake.Bytes())
			} else if wsmsg.Message == "[4] pake2" {
				log.Debug(c.Pake.SessionKey())
				wsreply.Message = "[5] pake3"
				wsreply.Payload = base64.StdEncoding.EncodeToString(c.Pake.Bytes())
			} else if wsmsg.Message == "[5] pake3" {
				var sessionKey, salt []byte
				sessionKey, err = c.Pake.SessionKey()
				if err != nil {
					log.Error(err)
					return
				}
				// setting setKey will ensure that this transfer is not encrypted, but future ones are
				setKey, salt, err = crypt.New(sessionKey, nil)
				if err != nil {
					log.Error(err)
					return
				}
				log.Debugf("key: %x", setKey)
				wsreply.Message = "[6] salt"
				wsreply.Payload = base64.StdEncoding.EncodeToString(salt)
			}
		} else if wsmsg.Message == "[6] salt" {
			var sessionKey, salt []byte
			salt, err = base64.StdEncoding.DecodeString(wsmsg.Payload)
			if err != nil {
				log.Error(err)
				return
			}
			sessionKey, err = c.Pake.SessionKey()
			if err != nil {
				log.Error(err)
				return
			}
			c.Key, _, err = crypt.New(sessionKey, salt)
			if err != nil {
				log.Error(err)
				return
			}
			log.Debugf("key: %x", c.Key)

			// create offer and send it over
			var offer webrtc.SessionDescription
			offer, err = c.rtc.CreateOffer(nil)
			if err != nil {
				log.Error(err)
				return
			}
			err = c.rtc.SetLocalDescription(offer)
			if err != nil {
				log.Error(err)
				return
			}
			var offerJSON []byte
			offerJSON, err = json.Marshal(offer)
			if err != nil {
				log.Error(err)
				return
			}
			wsreply.Message = "[7] offer"
			wsreply.Payload = base64.StdEncoding.EncodeToString(offerJSON)
			if err != nil {
				log.Error(err)
				return
			}
		} else if wsmsg.Message == "[7] offer" {
			// create webrtc answer and send it over
			var payload []byte
			payload, err = base64.StdEncoding.DecodeString(wsmsg.Payload)
			err = setRemoteDescription(c.rtc, payload)
			if err != nil {
				log.Error(err)
				return
			}

			var answer webrtc.SessionDescription
			answer, err = c.rtc.CreateAnswer(nil)
			if err != nil {
				log.Error(err)
				return
			}
			err = c.rtc.SetLocalDescription(answer)
			if err != nil {
				log.Error(err)
				return
			}

			// bundle it and send it over
			var answerJSON []byte
			answerJSON, err = json.Marshal(answer)
			if err != nil {
				log.Error(err)
			}
			wsreply.Message = "[8] answer"
			wsreply.Payload = base64.StdEncoding.EncodeToString(answerJSON)
		} else if wsmsg.Message == "[8] answer" {
			var payload []byte
			payload, err = base64.StdEncoding.DecodeString(wsmsg.Payload)
			err = setRemoteDescription(c.rtc, payload)
			if err != nil {
				log.Error(err)
				return
			}
		} else {
			log.Debug("unknown: %s", wsmsg)
		}
		if wsreply.Message != "" {
			log.Debugf("sending: %s", wsreply.Message)
			var bundled string
			bundled, err = box.Bundle(wsreply, c.Key)
			err = c.ws.WriteMessage(1, []byte(bundled))
			if err != nil {
				log.Error(err)
				return
			}
		}
	}
	err = <-finished
	return
}

const (
	bufferedAmountLowThreshold uint64 = 512 * 1024  // 512 KB
	maxBufferedAmount          uint64 = 1024 * 1024 // 1 MB
	maxPacketSize              uint64 = 65535
)

func setRemoteDescription(pc *webrtc.PeerConnection, sdp []byte) (err error) {
	log.Debug("setting remote description")
	var desc webrtc.SessionDescription
	err = json.Unmarshal(sdp, &desc)
	if err != nil {
		log.Error(err)
		return
	}

	log.Debug("applying remote description")
	// Apply the desc as the remote description
	err = pc.SetRemoteDescription(desc)
	if err != nil {
		log.Error(err)
	}
	return
}

func (c *Client) CreateOfferer(finished chan<- error) (pc *webrtc.PeerConnection, err error) {
	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	}

	// Create a new PeerConnection
	pc, err = webrtc.NewPeerConnection(config)
	if err != nil {
		log.Error(err)
		return
	}

	ordered := false
	maxRetransmits := uint16(0)
	options := &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &maxRetransmits,
	}

	sendMoreCh := make(chan struct{})

	// Create a datachannel with label 'data'
	dc, err := pc.CreateDataChannel("data", options)
	if err != nil {
		log.Error(err)
		return
	}

	// Register channel opening handling
	sendData := func(buf []byte) error {
		fmt.Printf("sent message: %x\n", md5.Sum(buf))
		err := dc.Send(buf)
		if err != nil {
			return err
		}
		if dc.BufferedAmount()+uint64(len(buf)) > maxBufferedAmount {
			// wait until the bufferedAmount becomes lower than the threshold
			<-sendMoreCh
		}
		return nil
	}

	dc.OnOpen(func() {
		fmt.Println(time.Now())
		log.Debugf("OnOpen: %s-%d. Start sending a series of 1024-byte packets as fast as it can\n", dc.Label(), dc.ID())
		its := 0
		for {
			its++

			msg, _ := box.Bundle(WebsocketMessage{
				Message: fmt.Sprintf("%d", its),
			}, c.Key)
			err2 := sendData([]byte(msg))
			if err2 != nil {
				finished <- err2
				return
			}
			time.Sleep(1 * time.Second)
			if its == 30000000 {
				break
			}
		}
	})

	// Set bufferedAmountLowThreshold so that we can get notified when
	// we can send more
	dc.SetBufferedAmountLowThreshold(bufferedAmountLowThreshold)

	// This callback is made when the current bufferedAmount becomes lower than the threadshold
	dc.OnBufferedAmountLow(func() {
		sendMoreCh <- struct{}{}
	})

	// Register the OnMessage to handle incoming messages
	dc.OnMessage(func(dcMsg webrtc.DataChannelMessage) {
		var wsmsg WebsocketMessage
		err = box.Unbundle(string(dcMsg.Data), c.Key, &wsmsg)
		if err == nil {
			log.Debugf("wsmsg: %+v", wsmsg)
		} else {
			log.Error(err)
		}
	})

	return pc, nil
}
