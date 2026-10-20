package server

import "math"

// Vec2 is a 2D vector.
type Vec2 struct{ X, Y float64 }

// Player is the authoritative state of one connected player.
type Player struct {
	ID         int
	Name       string
	Pos        Vec2
	Vel        Vec2
	Aim        float64
	HP         int
	Alive      bool
	RespawnT   float64
	Cooldown   float64 // fire cooldown remaining
	Score      int
	Kills      int
	Deaths     int
	LatencyT   int // estimated one-way latency in ticks (for lag comp)
	lastInput  Input
	inputTimes []int64 // unix-millis of recent inputs, for rate limiting
	violations int
}

// Input is a validated client input sample.
type Input struct {
	Seq   int
	CTick int
	Keys  int
	Aim   float64
	Fire  bool
}

// Bullet is an authoritative projectile.
type Bullet struct {
	ID    int
	Pos   Vec2
	Vel   Vec2
	Owner int
	TTL   float64
}

// validateInput sanitizes a raw client input and, if acceptable, stores it
// as the player's latest input. It returns false for inputs that must be
// dropped (duplicate/out-of-order seq, rate limit exceeded).
func validateInput(p *Player, in *InputMsg, nowMillis int64) bool {
	out := Input{Seq: in.Seq, CTick: in.CTick, Fire: in.Fire}
	if in.Seq <= p.lastInput.Seq {
		return false // duplicate or out-of-order: drop
	}
	// Rate limit: sliding 1s window.
	kept := p.inputTimes[:0]
	for _, t := range p.inputTimes {
		if nowMillis-t < 1000 {
			kept = append(kept, t)
		}
	}
	p.inputTimes = kept
	if len(p.inputTimes) >= InputRateLimit {
		p.violations++
		return false
	}
	p.inputTimes = append(p.inputTimes, nowMillis)
	out.Keys = in.Keys & 0xF // only movement bits are legal
	// Normalize aim into [-pi, pi]; reject NaN/Inf.
	if math.IsNaN(in.Aim) || math.IsInf(in.Aim, 0) {
		out.Aim = p.lastInput.Aim
	} else {
		out.Aim = math.Mod(in.Aim+math.Pi, 2*math.Pi) - math.Pi
		if out.Aim < -math.Pi {
			out.Aim += 2 * math.Pi
		}
	}
	p.lastInput = out
	return true
}

// inputDir converts a key bitmask into a normalized direction vector.
func inputDir(keys int) Vec2 {
	var d Vec2
	if keys&KeyUp != 0 {
		d.Y -= 1
	}
	if keys&KeyDown != 0 {
		d.Y += 1
	}
	if keys&KeyLeft != 0 {
		d.X -= 1
	}
	if keys&KeyRight != 0 {
		d.X += 1
	}
	if l := math.Hypot(d.X, d.Y); l > 0 {
		d.X /= l
		d.Y /= l
	}
	return d
}

// stepPlayer advances one player by dt using its latest validated input.
// Returns a bullet if the player fired this tick, else nil.
func stepPlayer(p *Player, dt float64) *Bullet {
	if !p.Alive {
		p.RespawnT -= dt
		return nil
	}
	d := inputDir(p.lastInput.Keys)
	if d.X != 0 || d.Y != 0 {
		p.Vel.X += d.X * PlayerAccel * dt
		p.Vel.Y += d.Y * PlayerAccel * dt
	} else {
		decay := 1 - PlayerFriction*dt
		if decay < 0 {
			decay = 0
		}
		p.Vel.X *= decay
		p.Vel.Y *= decay
	}
	// Clamp speed.
	if sp := math.Hypot(p.Vel.X, p.Vel.Y); sp > PlayerMaxSpeed {
		p.Vel.X = p.Vel.X / sp * PlayerMaxSpeed
		p.Vel.Y = p.Vel.Y / sp * PlayerMaxSpeed
	}
	p.Pos.X += p.Vel.X * dt
	p.Pos.Y += p.Vel.Y * dt

	collideWalls(p)
	collideObstacles(p)

	p.Aim = p.lastInput.Aim
	p.Cooldown -= dt
	if p.lastInput.Fire && p.Cooldown <= 0 {
		p.Cooldown = FireCooldown
		return fireBullet(p)
	}
	return nil
}

// collideWalls clamps the player inside the arena and kills the velocity
// component that pushed through the wall.
func collideWalls(p *Player) {
	if p.Pos.X < PlayerRadius {
		p.Pos.X = PlayerRadius
		if p.Vel.X < 0 {
			p.Vel.X = 0
		}
	} else if p.Pos.X > ArenaW-PlayerRadius {
		p.Pos.X = ArenaW - PlayerRadius
		if p.Vel.X > 0 {
			p.Vel.X = 0
		}
	}
	if p.Pos.Y < PlayerRadius {
		p.Pos.Y = PlayerRadius
		if p.Vel.Y < 0 {
			p.Vel.Y = 0
		}
	} else if p.Pos.Y > ArenaH-PlayerRadius {
		p.Pos.Y = ArenaH - PlayerRadius
		if p.Vel.Y > 0 {
			p.Vel.Y = 0
		}
	}
}

// collideObstacles resolves circle-vs-AABB penetration by pushing the
// player out along the axis of least penetration.
func collideObstacles(p *Player) {
	for _, o := range Obstacles {
		// Closest point on the AABB to the circle center.
		cx := clamp(p.Pos.X, o.X, o.X+o.W)
		cy := clamp(p.Pos.Y, o.Y, o.Y+o.H)
		dx := p.Pos.X - cx
		dy := p.Pos.Y - cy
		distSq := dx*dx + dy*dy
		if distSq >= PlayerRadius*PlayerRadius {
			continue
		}
		if distSq > 1e-9 {
			// Push out along the penetration normal.
			dist := math.Sqrt(distSq)
			push := (PlayerRadius - dist) / dist
			p.Pos.X += dx * push
			p.Pos.Y += dy * push
			// Kill velocity into the surface.
			nx, ny := dx/dist, dy/dist
			vn := p.Vel.X*nx + p.Vel.Y*ny
			if vn < 0 {
				p.Vel.X -= vn * nx
				p.Vel.Y -= vn * ny
			}
		} else {
			// Center inside the box: push out along smallest-penetration axis.
			pl := p.Pos.X - o.X
			pr := o.X + o.W - p.Pos.X
			pt := p.Pos.Y - o.Y
			pb := o.Y + o.H - p.Pos.Y
			m := math.Min(math.Min(pl, pr), math.Min(pt, pb))
			switch m {
			case pl:
				p.Pos.X = o.X - PlayerRadius
				if p.Vel.X > 0 {
					p.Vel.X = 0
				}
			case pr:
				p.Pos.X = o.X + o.W + PlayerRadius
				if p.Vel.X < 0 {
					p.Vel.X = 0
				}
			case pt:
				p.Pos.Y = o.Y - PlayerRadius
				if p.Vel.Y > 0 {
					p.Vel.Y = 0
				}
			default:
				p.Pos.Y = o.Y + o.H + PlayerRadius
				if p.Vel.Y < 0 {
					p.Vel.Y = 0
				}
			}
		}
	}
}

// fireBullet spawns a projectile from the player's aim direction.
func fireBullet(p *Player) *Bullet {
	dx, dy := math.Cos(p.Aim), math.Sin(p.Aim)
	return &Bullet{
		Pos: Vec2{
			X: p.Pos.X + dx*(PlayerRadius+BulletRadius+2),
			Y: p.Pos.Y + dy*(PlayerRadius+BulletRadius+2),
		},
		Vel: Vec2{
			X: dx*BulletSpeed + p.Vel.X*0.5,
			Y: dy*BulletSpeed + p.Vel.Y*0.5,
		},
		Owner: p.ID,
		TTL:   BulletTTL,
	}
}

// stepBullet moves a bullet; it returns false when the bullet dies
// (TTL expired or hit a wall/obstacle).
func stepBullet(b *Bullet, dt float64) bool {
	b.TTL -= dt
	if b.TTL <= 0 {
		return false
	}
	b.Pos.X += b.Vel.X * dt
	b.Pos.Y += b.Vel.Y * dt
	if b.Pos.X < 0 || b.Pos.X > ArenaW || b.Pos.Y < 0 || b.Pos.Y > ArenaH {
		return false
	}
	for _, o := range Obstacles {
		if b.Pos.X >= o.X && b.Pos.X <= o.X+o.W &&
			b.Pos.Y >= o.Y && b.Pos.Y <= o.Y+o.H {
			return false
		}
	}
	return true
}

// segmentHitsCircle reports whether the segment a->b passes within r of c.
func segmentHitsCircle(a, b, c Vec2, r float64) bool {
	abx, aby := b.X-a.X, b.Y-a.Y
	lenSq := abx*abx + aby*aby
	t := 0.0
	if lenSq > 1e-12 {
		t = ((c.X-a.X)*abx + (c.Y-a.Y)*aby) / lenSq
		t = clamp(t, 0, 1)
	}
	px, py := a.X+abx*t, a.Y+aby*t
	dx, dy := c.X-px, c.Y-py
	return dx*dx+dy*dy <= r*r
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
