// Package server implements the authoritative simulation and networking
// for the arena game. This file defines the JSON wire protocol (v1).
package server

// Client -> server messages (t = message type).

// JoinMsg is the first message a client must send after connecting.
type JoinMsg struct {
	T    string `json:"t"` // "join"
	Name string `json:"name"`
}

// InputMsg carries one client input sample.
type InputMsg struct {
	T     string  `json:"t"` // "input"
	Seq   int     `json:"seq"`
	CTick int     `json:"ctick"` // client's latest seen server tick (lag comp)
	Keys  int     `json:"keys"`  // bitmask: 1=up 2=down 4=left 8=right
	Aim   float64 `json:"aim"`   // radians, 0 = facing right
	Fire  bool    `json:"fire"`
}

// PingMsg is echoed by the server for client-side RTT measurement.
type PingMsg struct {
	T  string `json:"t"` // "ping"
	CT int64  `json:"ct"` // client unix millis
}

// Server -> client messages.

// WelcomeMsg is sent once after a successful join.
type WelcomeMsg struct {
	T       string         `json:"t"` // "welcome"
	ID      int            `json:"id"`
	Room    string         `json:"room"`
	Tick    int            `json:"tick"`
	Arena   ArenaDesc      `json:"arena"`
	Players []RosterPlayer `json:"players"`
}

// ArenaDesc describes the static arena geometry.
type ArenaDesc struct {
	W         float64 `json:"w"`
	H         float64 `json:"h"`
	Obstacles [][]float64 `json:"obstacles"` // [x,y,w,h]
}

// SnapMsg is a world snapshot. Full=true carries complete entity state;
// otherwise p/b entries are [id, bitmask, changed-fields...] deltas.
type SnapMsg struct {
	T      string   `json:"t"` // "snap"
	Tick   int      `json:"tick"`
	Full   bool     `json:"full"`
	P      []any    `json:"p"`              // players
	B      []any    `json:"b"`              // bullets
	Gone   []int   `json:"gone,omitempty"` // removed bullet ids
	Events []any    `json:"events,omitempty"`
}

// RosterMsg carries the scoreboard.
type RosterMsg struct {
	T       string         `json:"t"` // "roster"
	Players []RosterPlayer `json:"players"`
}

// RosterPlayer is one scoreboard row.
type RosterPlayer struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Score int    `json:"score"`
	Kills int    `json:"kills"`
}

// PongMsg echoes a client ping for RTT measurement.
type PongMsg struct {
	T  string `json:"t"` // "pong"
	CT int64  `json:"ct"`
}

// ErrorMsg reports a protocol error to the client.
type ErrorMsg struct {
	T   string `json:"t"` // "error"
	Msg string `json:"msg"`
}

// Wire event kinds inside SnapMsg.Events.
const (
	EventKill    = "kill"    // ["kill", killerID, victimID]
	EventRespawn = "respawn" // ["respawn", playerID, x, y]
)
