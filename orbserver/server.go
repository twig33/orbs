// Copyright 2013 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package orbserver

import (
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

var (
	maxID    = 512
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	isOkName  = regexp.MustCompile("^[A-Za-z0-9]+$").MatchString
	delimstr  = "\uffff"
	delimrune = '\uffff'
)

// Hub maintains the set of active clients and broadcasts messages to the
// clients.
type Hub struct {
	// Registered clients.
	clients map[*Client]bool

	// True if the id is in use
	id map[int]bool

	// Inbound messages from the clients.
	processMsgCh chan *Message

	// Connection requests from the clients.
	connect chan *websocket.Conn

	// Unregister requests from clients.
	unregister chan *Client

	roomName string
	//list of valid game character sprite resource keys
	spriteNames []string
}

func NewHub(roomName string, spriteNames []string) *Hub {
	return &Hub{
		processMsgCh: make(chan *Message),
		connect:      make(chan *websocket.Conn),
		unregister:   make(chan *Client),
		clients:      make(map[*Client]bool),
		id:           make(map[int]bool),
		roomName:     roomName,
		spriteNames:  spriteNames,
	}
}

func (h *Hub) Run() {
	http.HandleFunc("/"+h.roomName, h.serveWs)
	for {
		select {
		case conn := <-h.connect:
			id := 0
			for i := 0; i <= maxID; i++ {
				if !h.id[i] {
					id = i
					break
				}
			}
			//sprite index < 0 means none
			client := &Client{hub: h, conn: conn, send: make(chan []byte, 256), id: id, x: 0, y: 0, name: "", spd: 3, spriteName: "none", spriteIndex: -1}
			go client.writePump()
			go client.readPump()

			client.send <- []byte("s" + delimstr + strconv.Itoa(id)) //"your id is %id%" message
			//send the new client info about the game state
			for other_client := range h.clients {
				client.send <- []byte("c" + delimstr + strconv.Itoa(other_client.id))
				client.send <- []byte("m" + delimstr + strconv.Itoa(other_client.id) + delimstr + strconv.Itoa(other_client.x) + delimstr + strconv.Itoa(other_client.y) + delimstr + strconv.Itoa(other_client.f))
				client.send <- []byte("spd" + delimstr + strconv.Itoa(other_client.id) + delimstr + strconv.Itoa(other_client.spd))
				if other_client.spriteIndex >= 0 { //if the other client sent us valid sprite and index before
					client.send <- []byte("spr" + delimstr + strconv.Itoa(other_client.id) + delimstr + other_client.spriteName + delimstr + strconv.Itoa(other_client.spriteIndex))
				}
			}
			//register client in the structures
			h.id[id] = true
			h.clients[client] = true
			//tell everyone that a new client has connected
			h.broadcast([]byte("c" + delimstr + strconv.Itoa(id))) //user %id% has connected
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				h.deleteClient(client)
			}
		case message := <-h.processMsgCh:
			err := h.processMsg(message)
			if err != nil {
				log.Println(err)
			}
		}
	}
}

// serveWs handles websocket requests from the peer.
func (hub *Hub) serveWs(w http.ResponseWriter, r *http.Request) {
	protocols := r.Header.Get("Sec-Websocket-Protocol")
	conn, err := upgrader.Upgrade(w, r, http.Header{"Sec-Websocket-Protocol": {protocols}})
	if err != nil {
		log.Println(err)
		return
	}
	hub.connect <- conn
}

func (h *Hub) broadcast(data []byte) {
	for client := range h.clients {
		select {
		case client.send <- data:
		default:
			h.deleteClient(client)
		}
	}
}

func (h *Hub) deleteClient(client *Client) {
	delete(h.id, client.id)
	close(client.send)
	delete(h.clients, client)
	h.broadcast([]byte("d" + delimstr + strconv.Itoa(client.id))) //user %id% has disconnected message
}

func (h *Hub) processMsg(msg *Message) error {
	msgStr := string(msg.data[:])
	err := errors.New("Invalid message: " + msgStr)
	msgFields := strings.FieldsFunc(msgStr, func(c rune) bool {
		return c == delimrune
	}) //split message string on delimiting character

	switch msgFields[0] {
	case "m": //"i moved to x y facing"
		if len(msgFields) != 4 {
			return err
		}
		//check if the coordinates are valid
		x, errconv := strconv.Atoi(msgFields[1])
		if errconv != nil {
			return err
		}
		y, errconv := strconv.Atoi(msgFields[2])
		if errconv != nil {
			return err
		}
		f, errconv := strconv.Atoi(msgFields[3])
		if errconv != nil {
			return err
		}
		msg.sender.x = x
		msg.sender.y = y
		msg.sender.f = f
		h.broadcast([]byte("m" + delimstr + strconv.Itoa(msg.sender.id) + delimstr + msgFields[1] + delimstr + msgFields[2] + delimstr + msgFields[3])) //user %id% moved to x y
	case "spd": //change my speed to spd
		if len(msgFields) != 2 {
			return err
		}
		spd, errconv := strconv.Atoi(msgFields[1])
		if errconv != nil {
			return err
		}
		if spd < 0 || spd > 10 { //something's not right
			return err
		}
		msg.sender.spd = spd
		h.broadcast([]byte("spd" + delimstr + strconv.Itoa(msg.sender.id) + delimstr + msgFields[1]))
	case "spr": //change my sprite
		if len(msgFields) != 3 {
			return err
		}
		if !h.isValidSpriteName(msgFields[1]) {
			return err
		}
		index, errconv := strconv.Atoi(msgFields[2])
		if errconv != nil || index < 0 {
			return err
		}
		msg.sender.spriteName = msgFields[1]
		msg.sender.spriteIndex = index
		h.broadcast([]byte("spr" + delimstr + strconv.Itoa(msg.sender.id) + delimstr + msgFields[1] + delimstr + msgFields[2]))
	case "say":
		if len(msgFields) != 2 {
			return err
		}
		if msg.sender.name == "" {
			return err
		}
		h.broadcast([]byte("say" + delimstr + "<" + msg.sender.name + "> " + msgFields[1]))
	case "name":
		if msg.sender.name != "" || len(msgFields) != 2 || !isOkName(msgFields[1]) || len(msgFields[1]) > 7 {
			return err
		}
		msg.sender.name = msgFields[1]
	default:
		return err
	}

	return nil
}

func (h *Hub) isValidSpriteName(name string) bool {
	for _, otherName := range h.spriteNames {
		if otherName == name {
			return true
		}
	}
	return false
}
