package main

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	webrtc "github.com/deepch/vdk/format/webrtcv3"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// WebRTCRequest is the JSON request body for the WebRTC endpoint.
// Clients may POST either form-encoded data (legacy) or JSON with optional ICE server overrides.
type WebRTCRequest struct {
	Data          string       `json:"data"`
	ICEServers    []string     `json:"ice_servers,omitempty"`
	ICEUsername   string       `json:"ice_username,omitempty"`
	ICECredential string       `json:"ice_credential,omitempty"`
}

// HTTPAPIServerStreamWebRTC stream video over WebRTC
func HTTPAPIServerStreamWebRTC(c *gin.Context) {
	safeContext := c.Copy()
	requestLogger := log.WithFields(logrus.Fields{
		"module":  "http_webrtc",
		"stream":  safeContext.Param("uuid"),
		"channel": safeContext.Param("channel"),
		"func":    "HTTPAPIServerStreamWebRTC",
	})

	if !Storage.StreamChannelExist(safeContext.Param("uuid"), safeContext.Param("channel")) {
		c.IndentedJSON(500, Message{Status: 0, Payload: ErrorStreamNotFound.Error()})
		requestLogger.WithFields(logrus.Fields{
			"call": "StreamChannelExist",
		}).Errorln(ErrorStreamNotFound.Error())
		return
	}

	if !RemoteAuthorization("WebRTC", safeContext.Param("uuid"), safeContext.Param("channel"), safeContext.Query("token"), safeContext.ClientIP()) {
		requestLogger.WithFields(logrus.Fields{
			"call": "RemoteAuthorization",
		}).Errorln(ErrorStreamUnauthorized.Error())
		return
	}

	Storage.StreamChannelRun(safeContext.Param("uuid"), safeContext.Param("channel"))
	codecs, err := Storage.StreamChannelCodecs(safeContext.Param("uuid"), safeContext.Param("channel"))
	if err != nil {
		c.IndentedJSON(500, Message{Status: 0, Payload: err.Error()})
		requestLogger.WithFields(logrus.Fields{
			"call": "StreamCodecs",
		}).Errorln(err.Error())
		return
	}

	// Parse request: try JSON first, fall back to form-encoded "data" field
	var req WebRTCRequest
	sdpData := ""
	contentType := c.GetHeader("Content-Type")
	if strings.Contains(contentType, "application/json") {
		body, err := io.ReadAll(c.Request.Body)
		if err == nil {
			json.Unmarshal(body, &req)
			sdpData = req.Data
		}
	}
	if sdpData == "" {
		sdpData = c.PostForm("data")
	}

	// Build WebRTC options: use client-provided ICE servers if present, otherwise fall back to server config
	options := webrtc.Options{
		ICEServers:    Storage.ServerICEServers(),
		ICEUsername:   Storage.ServerICEUsername(),
		ICECredential: Storage.ServerICECredential(),
		PortMin:       Storage.ServerWebRTCPortMin(),
		PortMax:       Storage.ServerWebRTCPortMax(),
	}
	if len(Storage.ServerICECandidates()) > 0 {
		options.ICECandidates = Storage.ServerICECandidates()
	}
	if len(req.ICEServers) > 0 {
		options.ICEServers = req.ICEServers
		options.ICEUsername = req.ICEUsername
		options.ICECredential = req.ICECredential
		requestLogger.WithFields(logrus.Fields{
			"call": "ClientICEServers",
		}).Debugln("Using client-provided ICE servers")
	}

	muxerWebRTC := webrtc.NewMuxer(options)

	answer, err := muxerWebRTC.WriteHeader(codecs, sdpData)
	if err != nil {
		c.IndentedJSON(400, Message{Status: 0, Payload: err.Error()})
		requestLogger.WithFields(logrus.Fields{
			"call": "WriteHeader",
		}).Errorln(err.Error())
		return
	}
	_, err = c.Writer.Write([]byte(answer))
	if err != nil {
		c.IndentedJSON(400, Message{Status: 0, Payload: err.Error()})
		requestLogger.WithFields(logrus.Fields{
			"call": "Write",
		}).Errorln(err.Error())
		return
	}

	go func() {
		cid, ch, _, err := Storage.ClientAdd(safeContext.Param("uuid"), safeContext.Param("channel"), WEBRTC)
		if err != nil {
			c.IndentedJSON(400, Message{Status: 0, Payload: err.Error()})
			requestLogger.WithFields(logrus.Fields{
				"call": "ClientAdd",
			}).Errorln(err.Error())
			return
		}
		defer Storage.ClientDelete(safeContext.Param("uuid"), cid, safeContext.Param("channel"))
		defer muxerWebRTC.Close() // Close the WebRTC session when done
		var videoStart bool
		noVideo := time.NewTimer(10 * time.Second)
		for {
			select {
			case <-noVideo.C:
				c.IndentedJSON(500, Message{Status: 0, Payload: ErrorStreamNoVideo.Error()})
				requestLogger.WithFields(logrus.Fields{
					"call": "ErrorStreamNoVideo",
				}).Errorln(ErrorStreamNoVideo.Error())
				return
			case pck, ok := <-ch:
				if !ok {
					// Channel closed, likely due to camera disconnection
					c.IndentedJSON(500, Message{Status: 0, Payload: "Camera disconnected"})
					requestLogger.WithFields(logrus.Fields{
						"call": "CameraDisconnected",
					}).Errorln("Camera disconnected")
					return
				}

				if pck.IsKeyFrame {
					noVideo.Reset(10 * time.Second)
					videoStart = true
				}
				if !videoStart {
					continue
				}
				err = muxerWebRTC.WritePacket(*pck)
				if err != nil {
					requestLogger.WithFields(logrus.Fields{
						"call": "WritePacket",
					}).Errorln(err.Error())
					return
				}
			}
		}
	}()
}
