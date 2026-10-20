// Package server implements the authoritative simulation and networking
// for the arena game. All tuning knobs live here.
package server

// Rect is an axis-aligned bounding box obstacle.
type Rect struct {
	X, Y, W, H float64
}

const (
	// TickRate is the fixed simulation timestep frequency (Hz).
	TickRate = 60
	// SnapshotRate is how often snapshots are broadcast to clients (Hz).
	SnapshotRate = 20
	// MaxPlayersPerRoom caps room size; matchmaking fills rooms up to this.
	MaxPlayersPerRoom = 8

	// Arena dimensions in world units (pixels).
	ArenaW = 1600.0
	ArenaH = 900.0

	// Player physics.
	PlayerRadius   = 18.0
	PlayerMaxSpeed = 320.0
	PlayerAccel    = 2400.0
	PlayerFriction = 8.0 // per-second velocity decay when no input

	// Projectiles.
	BulletRadius  = 4.0
	BulletSpeed   = 700.0
	BulletTTL     = 2.0 // seconds
	BulletDamage  = 25
	FireCooldown  = 0.25 // seconds between shots

	// Health / respawn.
	PlayerHP     = 100
	RespawnDelay = 3.0 // seconds

	// HistoryTicks is the lag-compensation ring buffer length (1s @ 60Hz).
	HistoryTicks = 60

	// InputRateLimit caps accepted inputs per player per second.
	InputRateLimit = 120

	// WriteBufSize is the per-connection outbound message buffer.
	WriteBufSize = 256
)

// SimDt is the fixed timestep in seconds.
const SimDt = 1.0 / TickRate

// SnapshotEvery is how many sim ticks pass between snapshot broadcasts.
const SnapshotEvery = TickRate / SnapshotRate

// Obstacles is the fixed, deterministic arena layout shared by all rooms.
var Obstacles = []Rect{
	{X: 380, Y: 180, W: 120, H: 120},
	{X: 1100, Y: 180, W: 120, H: 120},
	{X: 380, Y: 600, W: 120, H: 120},
	{X: 1100, Y: 600, W: 120, H: 120},
	{X: 740, Y: 380, W: 120, H: 140}, // center block
	{X: 180, Y: 400, W: 90, H: 100},
	{X: 1330, Y: 400, W: 90, H: 100},
}

// SpawnPoints are deterministic respawn locations (corners + mid-edges).
var SpawnPoints = [][2]float64{
	{120, 120}, {ArenaW - 120, 120},
	{120, ArenaH - 120}, {ArenaW - 120, ArenaH - 120},
	{ArenaW / 2, 90}, {ArenaW / 2, ArenaH - 90},
	{90, ArenaH / 2}, {ArenaW - 90, ArenaH / 2},
}

// Input key bitmask (client -> server).
const (
	KeyUp    = 1
	KeyDown  = 2
	KeyLeft  = 4
	KeyRight = 8
)

// Player snapshot field bitmask (server -> client delta encoding).
// Field order in full snapshots: x, y, vx, vy, aim, hp, alive, score, kills.
const (
	FPlayerX     = 1 << 0
	FPlayerY     = 1 << 1
	FPlayerVX    = 1 << 2
	FPlayerVY    = 1 << 3
	FPlayerAim   = 1 << 4
	FPlayerHP    = 1 << 5
	FPlayerAlive = 1 << 6
	FPlayerScore = 1 << 7
	FPlayerKills = 1 << 8
	FPlayerAll   = (1 << 9) - 1
)

// Bullet snapshot field bitmask. Field order: x, y, vx, vy, owner, ttl.
const (
	FBulletX     = 1 << 0
	FBulletY     = 1 << 1
	FBulletVX    = 1 << 2
	FBulletVY    = 1 << 3
	FBulletOwner = 1 << 4
	FBulletTTL   = 1 << 5
	FBulletAll   = (1 << 6) - 1
)
