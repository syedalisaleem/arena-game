package server

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Conn is one player's WebSocket connection. The read pump runs on the
// HTTP handler goroutine; the write pump on its own goroutine. All
// simulation state lives on the room goroutine — the conn only forwards
// validated inputs through room.inputCh.
type Conn struct {
	hub      *Hub
	ws       *websocket.Conn
	playerID int
	name     string
	room     *Room
	needFull bool
	sendCh   chan any
	closeOnce sync.Once
	closed    chan struct{} // closed when the conn is fully torn down
}

func newConnFields(h *Hub, ws *websocket.Conn, pid int) *Conn {
	return &Conn{hub: h, ws: ws, playerID: pid, sendCh: make(chan any, WriteBufSize), closed: make(chan struct{})}
}

// readPump: first message must be join; then input/ping messages.
func (c *Conn) readPump() {
	defer c.teardown()
	c.ws.SetReadLimit(64 << 10)
	_ = c.ws.SetReadDeadline(time.Now().Add(15 * time.Second))

	// Join handshake.
	_, raw, err := c.ws.ReadMessage()
	if err != nil {
		return
	}
	var join JoinMsg
	if err := json.Unmarshal(raw, &join); err != nil || join.T != "join" {
		c.send(ErrorMsg{T: "error", Msg: "first message must be a join"})
		return
	}
	c.name = sanitizeName(join.Name)
	room := c.hub.matchmake()
	c.room = room // set before the room goroutine sees us via joinCh
	select {
	case room.joinCh <- c:
	case <-time.After(5 * time.Second):
		c.send(ErrorMsg{T: "error", Msg: "room busy, try again"})
		return
	}
	_ = c.ws.SetReadDeadline(time.Time{}) // no global deadline; pings keep alive

	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		c.handleMessage(raw)
	}
}

// handleMessage routes one client message.
func (c *Conn) handleMessage(raw []byte) {
	var base struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		return
	}
	switch base.T {
	case "input":
		var in InputMsg
		if err := json.Unmarshal(raw, &in); err != nil {
			return
		}
		select {
		case c.room.inputCh <- connInput{playerID: c.playerID, msg: in, recvMs: time.Now().UnixMilli()}:
		default:
			// Room overloaded: drop input rather than block the reader.
		}
	case "ping":
		var p PingMsg
		if err := json.Unmarshal(raw, &p); err != nil {
			return
		}
		c.send(PongMsg{T: "pong", CT: p.CT})
	}
}

// writePump serializes outbound messages.
func (c *Conn) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-c.sendCh:
			if !ok {
				return
			}
			_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.ws.WriteJSON(msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// send queues a message; if the buffer is full the message is dropped
// (backpressure protection: a slow client must not stall the room).
func (c *Conn) send(v any) {
	select {
	case c.sendCh <- v:
	default:
	}
}

// teardown removes the player from its room and closes the socket.
func (c *Conn) teardown() {
	c.closeOnce.Do(func() {
		if c.room != nil {
			// Best-effort; room may be stopped.
			select {
			case c.room.leaveCh <- c.playerID:
			case <-time.After(time.Second):
			}
		}
		_ = c.ws.Close()
		close(c.closed)
	})
}

func (c *Conn) close() { c.teardown() }

// sanitizeName trims and clamps player names.
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Player"
	}
	r := []rune(s)
	if len(r) > 16 {
		r = r[:16]
	}
	return string(r)
}
