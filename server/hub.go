package server

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Hub owns all rooms and performs matchmaking: a joining player is placed
// into the first room with a free slot, otherwise a new room is created.
type Hub struct {
	mu        sync.Mutex
	rooms     map[string]*Room
	nextRoom  int
	nextPID   atomic.Int64
	upgrader  websocket.Upgrader
	idleSweep time.Duration
}

// NewHub creates a hub. idleSweep controls how often empty rooms are reaped.
func NewHub() *Hub {
	return &Hub{
		rooms: make(map[string]*Room),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		idleSweep: 30 * time.Second,
	}
}

// Run starts background maintenance (reaping empty rooms). It returns when
// stop is closed.
func (h *Hub) Run(stop <-chan struct{}) {
	t := time.NewTicker(h.idleSweep)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			h.reapIdle()
		case <-stop:
			return
		}
	}
}

// reapIdle closes rooms that have been empty for > 2 minutes.
func (h *Hub) reapIdle() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now().UnixMilli()
	for id, r := range h.rooms {
		if r.playerCount() == 0 && now-r.lastSeen.Load() > 2*60*1000 {
			r.stop()
			delete(h.rooms, id)
		}
	}
}

// roomCount is exposed for tests/metrics.
func (h *Hub) roomCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms)
}

// matchmake finds or creates a room for a new player.
func (h *Hub) matchmake() *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.rooms {
		if !r.stopped.Load() && r.playerCount() < MaxPlayersPerRoom {
			return r
		}
	}
	h.nextRoom++
	id := fmt.Sprintf("room-%d", h.nextRoom)
	r := newRoom(id, h, time.Now().UnixNano()+int64(h.nextRoom))
	h.rooms[id] = r
	go r.run()
	return r
}

// ServeWS upgrades an HTTP connection and attaches it to a room.
func (h *Hub) ServeWS(w http.ResponseWriter, req *http.Request) {
	ws, err := h.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	pid := int(h.nextPID.Add(1))
	c := newConnFields(h, ws, pid)
	go c.writePump()
	c.readPump() // blocks until disconnect; then cleans up
}
