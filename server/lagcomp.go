package server

// Lag-compensation position history.
//
// The server records every player's position at the end of every sim tick
// into a ring buffer (HistoryTicks entries = 1 second at 60 Hz). When a
// bullet is tested for hits, the victims' positions are "rewound" to the
// tick the shooter had actually seen, compensating for the shooter's
// latency. This is the classic "server rewinds time for hit tests" model
// (as in Source engine lag compensation).
//
// Each player reports ctick (the newest server tick it has rendered) in
// every input packet. The server estimates one-way latency as
// serverTick - ctick, clamped to the history length, and rewinds the
// world by that many ticks for that shooter's hit tests only. Other
// players' own simulations are unaffected.

// histEntry is one recorded tick of player positions.
type histEntry struct {
	tick int
	pos  map[int]Vec2 // playerID -> position (alive players only)
}

// history is a fixed-size ring buffer of position snapshots.
type history struct {
	buf   []histEntry
	head  int // index of newest entry
	count int
}

func newHistory() *history {
	return &history{buf: make([]histEntry, HistoryTicks)}
}

// record stores the current alive-player positions for tick.
func (h *history) record(tick int, players map[int]*Player) {
	e := histEntry{tick: tick, pos: make(map[int]Vec2, len(players))}
	for id, p := range players {
		if p.Alive {
			e.pos[id] = p.Pos
		}
	}
	h.buf[h.head] = e
	h.head = (h.head + 1) % len(h.buf)
	if h.count < len(h.buf) {
		h.count++
	}
}

// len returns how many ticks of history are stored.
func (h *history) len() int { return h.count }

// rewind returns player positions as of ticksBack ticks ago.
// ticksBack is clamped to the available history. The returned map must
// not be mutated by the caller.
func (h *history) rewind(ticksBack int) map[int]Vec2 {
	if h.count == 0 {
		return nil
	}
	if ticksBack < 0 {
		ticksBack = 0
	}
	if ticksBack >= h.count {
		ticksBack = h.count - 1
	}
	idx := h.head - 1 - ticksBack
	for idx < 0 {
		idx += len(h.buf)
	}
	return h.buf[idx].pos
}
