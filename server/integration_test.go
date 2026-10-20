package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testClient is a scripted fake player driving the real WebSocket endpoint.
type testClient struct {
	conn      *websocket.Conn
	mu        sync.Mutex
	welcome   bool
	playerID  int
	snaps     int
	fullSnaps int
	lastTick  int
	maxTick   int
	seen      map[int]bool // player ids observed in snapshots
	events    int
	errors    int
}

func (c *testClient) readLoop(done chan struct{}) {
	defer close(done)
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var base struct {
			T string `json:"t"`
		}
		if err := json.Unmarshal(raw, &base); err != nil {
			continue
		}
		c.mu.Lock()
		switch base.T {
		case "welcome":
			var w WelcomeMsg
			_ = json.Unmarshal(raw, &w)
			c.welcome = true
			c.playerID = w.ID
			if len(w.Arena.Obstacles) == 0 || w.Arena.W != ArenaW {
				c.errors++
			}
		case "snap":
			var s struct {
				Tick   int             `json:"tick"`
				Full   bool            `json:"full"`
				P      []json.RawMessage `json:"p"`
				Events []json.RawMessage `json:"events"`
			}
			_ = json.Unmarshal(raw, &s)
			c.snaps++
			if s.Full {
				c.fullSnaps++
			}
			if s.Tick > c.maxTick {
				c.maxTick = s.Tick
			}
			c.lastTick = s.Tick
			for _, e := range s.P {
				var arr []any
				if err := json.Unmarshal(e, &arr); err == nil && len(arr) > 0 {
					if id, ok := arr[0].(float64); ok {
						c.seen[int(id)] = true
					}
				}
			}
			c.events += len(s.Events)
		case "error":
			c.errors++
		}
		c.mu.Unlock()
	}
}

func (c *testClient) sendInput(seq, ctick, keys int, aim float64, fire bool) error {
	return c.conn.WriteJSON(InputMsg{T: "input", Seq: seq, CTick: ctick, Keys: keys, Aim: aim, Fire: fire})
}

func (c *testClient) tick() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastTick
}

// TestMultiplayerIntegration spins up the real server, connects 6 scripted
// clients for ~10 simulated seconds, and asserts the netcode contract:
// snapshots at ~20 Hz, entity state visible to all clients, lag-compensation
// history populated, no panics or protocol errors.
func TestMultiplayerIntegration(t *testing.T) {
	hub := NewHub()
	stop := make(chan struct{})
	defer close(stop)
	go hub.Run(stop)

	srv := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	defer srv.Close()
	wsURL := "ws" + srv.URL[len("http"):] 

	const nClients = 6
	const simSeconds = 10

	clients := make([]*testClient, nClients)
	dones := make([]chan struct{}, nClients)
	for i := 0; i < nClients; i++ {
		ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("client %d dial: %v", i, err)
		}
		c := &testClient{conn: ws, seen: make(map[int]bool)}
		clients[i] = c
		if err := ws.WriteJSON(JoinMsg{T: "join", Name: fmt.Sprintf("bot-%d", i)}); err != nil {
			t.Fatalf("client %d join: %v", i, err)
		}
		dones[i] = make(chan struct{})
		go c.readLoop(dones[i])
	}

	// Drive scripted inputs at 30 Hz for ~10s.
	inputTicker := time.NewTicker(33 * time.Millisecond)
	defer inputTicker.Stop()
	deadline := time.Now().Add(simSeconds * time.Second)
	seqs := make([]int, nClients)
	phase := 0
	for time.Now().Before(deadline) {
		<-inputTicker.C
		phase++
		for i, c := range clients {
			seqs[i]++
			// Cycle movement direction every ~2s so bots roam the arena.
			var keys int
			switch (phase / 60) % 4 {
			case 0:
				keys = KeyUp | KeyRight
			case 1:
				keys = KeyDown | KeyRight
			case 2:
				keys = KeyDown | KeyLeft
			default:
				keys = KeyUp | KeyLeft
			}
			aim := float64(phase)*0.05 + float64(i)
			fire := phase%30 < 15 // burst fire: 0.5s on, 0.5s off
			if err := c.sendInput(seqs[i], c.tick(), keys, aim, fire); err != nil {
				t.Fatalf("client %d input: %v", i, err)
			}
		}
		// Periodic pings (RTT path).
		if phase%60 == 0 {
			for _, c := range clients {
				_ = c.conn.WriteJSON(PingMsg{T: "ping", CT: time.Now().UnixMilli()})
			}
		}
	}

	for i, c := range clients {
		_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = c.conn.Close()
		<-dones[i]
	}

	// Assertions.
	totalSnaps := 0
	for i, c := range clients {
		c.mu.Lock()
		t.Logf("client %d: welcome=%v snaps=%d (full=%d) maxTick=%d playersSeen=%d events=%d errors=%d",
			i, c.welcome, c.snaps, c.fullSnaps, c.maxTick, len(c.seen), c.events, c.errors)
		if !c.welcome {
			t.Errorf("client %d never received welcome", i)
		}
		if c.fullSnaps < 1 {
			t.Errorf("client %d never received a full snapshot", i)
		}
		// 20 Hz * 10 s = 200; allow generous jitter bounds.
		if c.snaps < 150 || c.snaps > 260 {
			t.Errorf("client %d snapshot count %d outside expected ~200 range", i, c.snaps)
		}
		if len(c.seen) < nClients {
			t.Errorf("client %d saw only %d players, want %d", i, len(c.seen), nClients)
		}
		if c.errors > 0 {
			t.Errorf("client %d got %d protocol errors", i, c.errors)
		}
		totalSnaps += c.snaps
		c.mu.Unlock()
	}
	t.Logf("total snapshots across %d clients: %d", nClients, totalSnaps)

	if got := hub.roomCount(); got != 1 {
		t.Errorf("roomCount = %d, want 1 (all clients should share a room)", got)
	}

	// Lag-compensation history must be populated on the room, and the sim must
	// keep up. Tick/HistoryLen are the goroutine-safe mirrors: reading r.tick or
	// r.hist directly here would race the room's tick loop.
	hub.mu.Lock()
	for _, r := range hub.rooms {
		if got := r.HistoryLen(); got == 0 {
			t.Errorf("room %s lag history empty", r.ID)
		} else {
			t.Logf("room %s: tick=%d history=%d/%d", r.ID, r.Tick(), got, HistoryTicks)
		}
		if r.Tick() < TickRate*simSeconds-30 {
			t.Errorf("room %s tick=%d, sim fell behind", r.ID, r.Tick())
		}
	}
	hub.mu.Unlock()
}

// TestStepDeterminism checks the simulation core is deterministic: two
// identical runs produce bit-identical state (required for future
// server-side reconciliation and replay).
func TestStepDeterminism(t *testing.T) {
	run := func() (Vec2, Vec2, int) {
		p := &Player{Alive: true, HP: PlayerHP}
		p.Pos = Vec2{X: 800, Y: 450}
		var bullets []*Bullet
		for i := 0; i < 600; i++ {
			p.lastInput = Input{
				Keys: []int{KeyUp | KeyRight, KeyDown, KeyLeft, 0}[i/150],
				Aim:  float64(i) * 0.01,
				Fire: i%20 < 10,
			}
			if b := stepPlayer(p, SimDt); b != nil {
				bullets = append(bullets, b)
			}
			for _, b := range bullets {
				stepBullet(b, SimDt)
			}
		}
		return p.Pos, p.Vel, p.HP
	}
	pos1, vel1, hp1 := run()
	pos2, vel2, hp2 := run()
	if pos1 != pos2 || vel1 != vel2 || hp1 != hp2 {
		t.Errorf("non-deterministic sim: %+v/%+v/%d vs %+v/%+v/%d", pos1, vel1, hp1, pos2, vel2, hp2)
	}
}

// TestValidateInput ensures the anti-cheat validation drops bad inputs.
func TestValidateInput(t *testing.T) {
	p := &Player{Alive: true}
	now := time.Now().UnixMilli()
	in := InputMsg{T: "input", Seq: 1, Keys: 0xFF, Aim: 3 * 3.14159, Fire: true}
	if !validateInput(p, &in, now) {
		t.Fatal("first valid input rejected")
	}
	if p.lastInput.Keys != 0xF {
		t.Errorf("keys not clamped: got %d", p.lastInput.Keys)
	}
	// Duplicate seq must be dropped.
	if validateInput(p, &in, now) {
		t.Error("duplicate seq accepted")
	}
	// Rate limit: 120 inputs in the same millisecond window.
	p2 := &Player{Alive: true}
	allowed := 0
	for i := 1; i <= 200; i++ {
		m := InputMsg{T: "input", Seq: i, Keys: 1}
		if validateInput(p2, &m, now) {
			allowed++
		}
	}
	if allowed != InputRateLimit {
		t.Errorf("rate limit allowed %d inputs, want %d", allowed, InputRateLimit)
	}
}
