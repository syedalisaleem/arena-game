package server

import (
	"math/rand"
	"sync/atomic"
	"time"
)

// Room is one arena instance. It owns the authoritative simulation and runs
// it on a single goroutine at a fixed 60 Hz timestep; all state mutations
// happen on that goroutine (joins, leaves and inputs arrive via channels).
type Room struct {
	ID      string
	hub     *Hub
	players map[int]*Player
	conns   map[int]*Conn
	bullets map[int]*Bullet

	tick         int
	nextBulletID int
	rng          *rand.Rand
	hist         *history
	enc          *deltaEncoder

	joinCh  chan *Conn
	leaveCh chan int
	inputCh chan connInput

	stopCh   chan struct{}
	stopped  atomic.Bool
	lastSeen atomic.Int64 // unix millis of last player activity (for reaping)

	// Observability mirrors, written by the room goroutine at the end of each
	// tick. Readers on other goroutines (tests, metrics) must use Tick and
	// HistoryLen instead of the room-owned fields above, which would race the
	// simulation loop.
	tickObs atomic.Int64
	histObs atomic.Int64
}

type connInput struct {
	playerID int
	msg      InputMsg
	recvMs   int64
}

// newRoom creates a room with a deterministic RNG seed.
func newRoom(id string, hub *Hub, seed int64) *Room {
	return &Room{
		ID:      id,
		hub:     hub,
		players: make(map[int]*Player),
		conns:   make(map[int]*Conn),
		bullets: make(map[int]*Bullet),
		rng:     rand.New(rand.NewSource(seed)),
		hist:    newHistory(),
		enc:     newDeltaEncoder(),
		joinCh:  make(chan *Conn, 16),
		leaveCh: make(chan int, 16),
		inputCh: make(chan connInput, 1024),
		stopCh:  make(chan struct{}),
	}
}

func (r *Room) playerCount() int { return len(r.players) }

// Tick returns the most recently completed simulation tick. Safe to call from
// any goroutine.
func (r *Room) Tick() int { return int(r.tickObs.Load()) }

// HistoryLen returns how many ticks of lag-compensation history are stored.
// Safe to call from any goroutine.
func (r *Room) HistoryLen() int { return int(r.histObs.Load()) }

// run is the fixed-timestep simulation loop.
func (r *Room) run() {
	ticker := time.NewTicker(time.Second / TickRate)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.tickSim()
		case c := <-r.joinCh:
			r.addPlayer(c)
		case id := <-r.leaveCh:
			r.removePlayer(id)
		case in := <-r.inputCh:
			r.applyInput(in)
		case <-r.stopCh:
			return
		}
	}
}

// addPlayer registers a new player, sends the welcome message and marks
// the connection as needing a full snapshot.
func (r *Room) addPlayer(c *Conn) {
	p := &Player{
		ID:    c.playerID,
		Name:  c.name,
		Alive: true,
		HP:    PlayerHP,
	}
	sp := SpawnPoints[(r.tick/30+c.playerID)%len(SpawnPoints)]
	p.Pos = Vec2{X: sp[0], Y: sp[1]}
	r.players[p.ID] = p
	r.conns[p.ID] = c
	c.needFull = true
	r.lastSeen.Store(time.Now().UnixMilli())

	ob := make([][]float64, 0, len(Obstacles))
	for _, o := range Obstacles {
		ob = append(ob, []float64{o.X, o.Y, o.W, o.H})
	}
	roster := r.roster()
	c.send(WelcomeMsg{
		T: "welcome", ID: p.ID, Room: r.ID, Tick: r.tick,
		Arena:   ArenaDesc{W: ArenaW, H: ArenaH, Obstacles: ob},
		Players: roster,
	})
	r.broadcastRoster()
}

// removePlayer drops a player and its connection.
func (r *Room) removePlayer(id int) {
	if c, ok := r.conns[id]; ok {
		c.close()
		delete(r.conns, id)
	}
	delete(r.players, id)
	r.broadcastRoster()
}

// applyInput validates and stores a player's latest input.
func (r *Room) applyInput(in connInput) {
	p, ok := r.players[in.playerID]
	if !ok {
		return
	}
	validated := validateInput(p, &in.msg, in.recvMs)
	if !validated {
		return
	}
	// Latency estimate from the client's observed tick (one-way, in ticks).
	lag := r.tick - p.lastInput.CTick
	if lag < 0 {
		lag = 0
	}
	if lag > HistoryTicks-1 {
		lag = HistoryTicks - 1
	}
	p.LatencyT = lag
	r.lastSeen.Store(in.recvMs)
}

// tickSim advances the whole world by one fixed timestep.
func (r *Room) tickSim() {
	r.tick++

	// 1. Players (movement, firing).
	for _, id := range sortedKeys(r.players) {
		p := r.players[id]
		if b := stepPlayer(p, SimDt); b != nil {
			r.nextBulletID++
			b.ID = r.nextBulletID
			r.bullets[b.ID] = b
		}
		if !p.Alive && p.RespawnT <= 0 {
			r.respawn(p)
		}
	}

	// 2. Bullets (move + lag-compensated hit tests).
	for _, id := range sortedKeys(r.bullets) {
		b := r.bullets[id]
		prev := b.Pos
		if !stepBullet(b, SimDt) {
			delete(r.bullets, id)
			continue
		}
		if victim := r.hitTest(b, prev); victim != nil {
			delete(r.bullets, id)
			r.damage(victim, b.Owner)
		}
	}

	// 3. Record lag-compensation history.
	r.hist.record(r.tick, r.players)
	r.tickObs.Store(int64(r.tick))
	r.histObs.Store(int64(r.hist.len()))

	// 4. Broadcast.
	if r.tick%SnapshotEvery == 0 {
		r.broadcastSnap()
	}
	if r.tick%TickRate == 0 {
		r.broadcastRoster()
	}
}

// hitTest checks bullet b (travelling prev->b.Pos) against all alive
// players except its owner, using positions rewound by the shooter's
// observed latency. Returns the victim or nil.
func (r *Room) hitTest(b *Bullet, prev Vec2) *Player {
	shooter, ok := r.players[b.Owner]
	lag := 0
	if ok {
		lag = shooter.LatencyT
	}
	rewound := r.hist.rewind(lag)
	hitR := PlayerRadius + BulletRadius
	for _, id := range sortedKeys(r.players) {
		if id == b.Owner {
			continue
		}
		p := r.players[id]
		if !p.Alive {
			continue
		}
		pos, ok := rewound[id]
		if !ok {
			pos = p.Pos // no history yet (first second): use current
		}
		if segmentHitsCircle(prev, b.Pos, pos, hitR) {
			return p
		}
	}
	return nil
}

// damage applies a hit; kills grant score and queue events.
func (r *Room) damage(victim *Player, shooterID int) {
	victim.HP -= BulletDamage
	if victim.HP > 0 {
		return
	}
	victim.HP = 0
	victim.Alive = false
	victim.RespawnT = RespawnDelay
	victim.Deaths++
	if s, ok := r.players[shooterID]; ok && shooterID != victim.ID {
		s.Kills++
		s.Score++
	}
	r.enc.queueEvent([]any{EventKill, shooterID, victim.ID})
}

// respawn places a dead player at a deterministic spawn point.
func (r *Room) respawn(p *Player) {
	sp := SpawnPoints[(r.tick/180+p.ID)%len(SpawnPoints)]
	p.Pos = Vec2{X: sp[0], Y: sp[1]}
	p.Vel = Vec2{}
	p.HP = PlayerHP
	p.Alive = true
	p.Cooldown = 0
	p.lastInput = Input{} // clear stale input so we don't drift on respawn
	r.enc.queueEvent([]any{EventRespawn, p.ID, p.Pos.X, p.Pos.Y})
}

// broadcastSnap sends a snapshot to every connection; connections that
// joined since the last broadcast get a full snapshot.
func (r *Room) broadcastSnap() {
	full, delta := r.enc.encodeBoth(r.tick, r.players, r.bullets)
	for _, c := range r.conns {
		if c.needFull {
			c.send(full)
			c.needFull = false
		} else {
			c.send(delta)
		}
	}
}

// broadcastRoster sends the scoreboard to every connection.
func (r *Room) broadcastRoster() {
	msg := RosterMsg{T: "roster", Players: r.roster()}
	for _, c := range r.conns {
		c.send(msg)
	}
}

func (r *Room) roster() []RosterPlayer {
	out := make([]RosterPlayer, 0, len(r.players))
	for _, id := range sortedKeys(r.players) {
		p := r.players[id]
		out = append(out, RosterPlayer{ID: p.ID, Name: p.Name, Score: p.Score, Kills: p.Kills})
	}
	return out
}

// stop shuts the room down.
func (r *Room) stop() {
	if r.stopped.CompareAndSwap(false, true) {
		close(r.stopCh)
	}
}
