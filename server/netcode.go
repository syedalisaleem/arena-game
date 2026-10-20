package server

import "math"

// Delta-compressed snapshot encoding.
//
// Every SnapshotRate tick the room broadcasts the world. The first
// snapshot a client receives is FULL: every entity with every field.
// Subsequent broadcasts are DELTAS against the last broadcast state:
// each entity becomes [id, bitmask, changed-fields...] where the bitmask
// selects which fields follow, in ascending bit order (see FPlayer* /
// FBullet* in config.go). Entities whose state did not change are omitted
// entirely; removed bullets are listed in `gone`.
//
// Floats are compared with an epsilon so network noise-free deterministic
// floats don't spam deltas. NaN can never appear (validated on input).

const deltaEpsilon = 1e-3

func feq(a, b float64) bool { return math.Abs(a-b) < deltaEpsilon }

// netPlayer / netBullet are the last-broadcast wire states, kept per room
// (all clients in a room share the same broadcast cadence, so one baseline
// per room is enough).
type netPlayer struct {
	x, y, vx, vy, aim, hp float64
	alive                 bool
	score, kills          int
}
type netBullet struct {
	x, y, vx, vy, ttl float64
	owner             int
}

// deltaEncoder tracks the last broadcast baseline and emitted events.
type deltaEncoder struct {
	players map[int]*netPlayer
	bullets map[int]*netBullet
	events  []any // queued since last broadcast
}

func newDeltaEncoder() *deltaEncoder {
	return &deltaEncoder{
		players: make(map[int]*netPlayer),
		bullets: make(map[int]*netBullet),
	}
}

func (e *deltaEncoder) queueEvent(ev ...any) { e.events = append(e.events, ev) }

// encodeBoth builds the full and delta SnapMsgs for the current room state
// in a single pass, updating the baseline once. Clients that joined since
// the last broadcast receive full; everyone else receives delta.
func (e *deltaEncoder) encodeBoth(tick int, players map[int]*Player, bullets map[int]*Bullet) (SnapMsg, SnapMsg) {
	full := SnapMsg{T: "snap", Tick: tick, Full: true}
	delta := SnapMsg{T: "snap", Tick: tick, Full: false}
	// Players: ids sorted for deterministic output.
	for _, id := range sortedKeys(players) {
		p := players[id]
		cur := &netPlayer{
			x: p.Pos.X, y: p.Pos.Y, vx: p.Vel.X, vy: p.Vel.Y,
			aim: p.Aim, hp: float64(p.HP),
			alive: p.Alive, score: p.Score, kills: p.Kills,
		}
		full.P = append(full.P, []any{id, cur.x, cur.y, cur.vx, cur.vy, cur.aim, cur.hp, bool01(cur.alive), cur.score, cur.kills})
		if prev := e.players[id]; prev != nil {
			mask := 0
			var vals []any
			add := func(bit int, v any) {
				mask |= bit
				vals = append(vals, v)
			}
			if !feq(cur.x, prev.x) {
				add(FPlayerX, cur.x)
			}
			if !feq(cur.y, prev.y) {
				add(FPlayerY, cur.y)
			}
			if !feq(cur.vx, prev.vx) {
				add(FPlayerVX, cur.vx)
			}
			if !feq(cur.vy, prev.vy) {
				add(FPlayerVY, cur.vy)
			}
			if !feq(cur.aim, prev.aim) {
				add(FPlayerAim, cur.aim)
			}
			if !feq(cur.hp, prev.hp) {
				add(FPlayerHP, cur.hp)
			}
			if cur.alive != prev.alive {
				add(FPlayerAlive, bool01(cur.alive))
			}
			if cur.score != prev.score {
				add(FPlayerScore, cur.score)
			}
			if cur.kills != prev.kills {
				add(FPlayerKills, cur.kills)
			}
			if mask != 0 {
				entry := []any{id, mask}
				delta.P = append(delta.P, append(entry, vals...))
			}
		} else {
			// New player for existing clients: send as a full entry
			// inside the delta (mask = all bits).
			entry := []any{id, FPlayerAll, cur.x, cur.y, cur.vx, cur.vy, cur.aim, cur.hp, bool01(cur.alive), cur.score, cur.kills}
			delta.P = append(delta.P, entry)
		}
		e.players[id] = cur
	}
	// Bullets.
	for _, id := range sortedKeys(bullets) {
		b := bullets[id]
		cur := &netBullet{x: b.Pos.X, y: b.Pos.Y, vx: b.Vel.X, vy: b.Vel.Y, owner: b.Owner, ttl: b.TTL}
		full.B = append(full.B, []any{id, cur.x, cur.y, cur.vx, cur.vy, cur.owner, cur.ttl})
		if prev := e.bullets[id]; prev != nil {
			mask := 0
			var vals []any
			add := func(bit int, v any) {
				mask |= bit
				vals = append(vals, v)
			}
			if !feq(cur.x, prev.x) {
				add(FBulletX, cur.x)
			}
			if !feq(cur.y, prev.y) {
				add(FBulletY, cur.y)
			}
			if !feq(cur.vx, prev.vx) {
				add(FBulletVX, cur.vx)
			}
			if !feq(cur.vy, prev.vy) {
				add(FBulletVY, cur.vy)
			}
			if cur.owner != prev.owner {
				add(FBulletOwner, cur.owner)
			}
			if !feq(cur.ttl, prev.ttl) {
				add(FBulletTTL, cur.ttl)
			}
			if mask != 0 {
				entry := []any{id, mask}
				delta.B = append(delta.B, append(entry, vals...))
			}
		} else {
			// New bullet for existing clients: full entry, all bits.
			delta.B = append(delta.B, []any{id, FBulletAll, cur.x, cur.y, cur.vx, cur.vy, cur.owner, cur.ttl})
		}
		e.bullets[id] = cur
	}
	// Removed bullets.
	for id := range e.bullets {
		if _, ok := bullets[id]; !ok {
			delta.Gone = append(delta.Gone, id)
			delete(e.bullets, id)
		}
	}
	// Removed players (disconnects).
	for id := range e.players {
		if _, ok := players[id]; !ok {
			delta.Gone = append(delta.Gone, id)
			delete(e.players, id)
		}
	}
	if len(e.events) > 0 {
		full.Events = e.events
		delta.Events = e.events
		e.events = nil
	}
	return full, delta
}

func bool01(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sortedKeys[V any](m map[int]V) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	// Insertion sort: maps are tiny.
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}
