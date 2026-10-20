# Arena — Real-Time Distributed Multiplayer Game Server

A 2D multiplayer arena shooter with an **authoritative Go game server**
(fixed-timestep 60 Hz simulation) and a **PixiJS web client** featuring
client-side prediction + reconciliation, entity interpolation,
delta-compressed snapshots, and server-side lag compensation.

One binary runs everything: the client is embedded via `go:embed` and
served on the same port as the WebSocket endpoint.

## Architecture

```
                    ┌────────────────────────────────────────────┐
                    │              arena-server (Go)             │
                    │                                            │
  ┌──────────┐      │  ┌─────────┐   ┌────────────────────────┐  │
  │ PixiJS   │◄─────┼──►│  Hub    │──►│ Room (goroutine)       │  │
  │ client   │  WS  │  │ match-  │   │  60 Hz fixed timestep  │  │
  │          │ JSON │  │ making  │   │  ┌──────┐ ┌──────────┐  │  │
  │ predict  ├──────┼──►│ 8/room  │   │  │ sim  │ │netcode   │  │  │
  │ inter-   │ snap │  │         │   │  │physics│ │delta enc │  │  │
  │ polate   │ 20Hz │  │         │   │  └──────┘ │lag-comp  │  │  │
  └──────────┘      │  └─────────┘   │         │history   │  │  │
                    │                └────────────────────────┘  │
                    │  static files ──► go:embed web/           │
                    └────────────────────────────────────────────┘
```

* **Hub** — owns rooms, matchmakes joiners into the first non-full room
  (8 players max), reaps idle rooms.
* **Room** — one goroutine per arena. Owns *all* mutable simulation
  state; joins, leaves and inputs arrive over channels, so the sim is
  single-threaded and race-free by construction.
* **Sim** — custom circle/AABB physics, movement, projectiles,
  hitpoints, respawn, scoring.
* **Netcode** — 20 Hz snapshots, bitmask delta compression, per-shooter
  lag-compensated hit tests.
* **Client** — prediction, reconciliation, interpolation, debug HUD.

### Why custom physics instead of Matter.js / a Go physics lib?

1. **Determinism.** The sim must be bit-identical across runs for
   replays, server-side verification and (future) rollback. External
   engines prioritize features over determinism; a small custom
   integrator with a fixed timestep and seeded RNG is trivially
   deterministic (`TestStepDeterminism` proves it).
2. **Delta-friendly state.** The netcode diffs entity fields every
   broadcast; a minimal state representation (pos/vel/hp/…) makes the
   bitmask encoder simple and the snapshots small.
3. **Authority.** Only the server runs physics. The client re-implements
   just the movement integrator for prediction — a 40-line function,
   not a whole engine.

## How to run

Prerequisites: Go 1.24+ (or use the vendored toolchain note below).

```bash
# 1. Sync the client into the embed directory (web/ is source of truth)
./sync-web.sh

# 2. Build & run (serves game at http://localhost:8080, WS at /ws)
go build -o arena-server ./cmd/arena-server
./arena-server -addr :8080
```

Then open `http://localhost:8080`, enter a name, and play.
Open a second tab for multiplayer. Move with **WASD**, aim with the
mouse, click/hold to fire, **H** toggles the debug HUD.

```bash
go vet ./...        # static analysis
go test ./...       # unit + integration tests
```

> The Go toolchain used to build this was installed locally at
> `~/workspace/tools/go` (no system-wide install).

## Protocol spec (v1, JSON over WebSocket text frames)

World: 1600×900 units, x right, y down. Angles in radians, 0 = facing right.

### Client → server

| Message | Shape |
|---|---|
| `join` (must be first) | `{"t":"join","name":"Ali"}` |
| `input` (30 Hz) | `{"t":"input","seq":N,"ctick":T,"keys":K,"aim":A,"fire":B}` |
| `ping` (2 s) | `{"t":"ping","ct":<client unix ms>}` |

`keys` bitmask: `1`=up, `2`=down, `4`=left, `8`=right.
`ctick` = newest server snapshot tick the client has rendered —
used for lag compensation (see below).

### Server → client

| Message | Shape |
|---|---|
| `welcome` | `{"t":"welcome","id":PID,"room":"room-1","tick":T,"arena":{"w":1600,"h":900,"obstacles":[[x,y,w,h],…]},"players":[{"id","name"}]}` |
| `snap` | `{"t":"snap","tick":T,"full":bool,"p":[…],"b":[…],"gone":[…],"events":[…]}` |
| `roster` | `{"t":"roster","players":[{"id","name","score","kills"}]}` |
| `pong` | `{"t":"pong","ct":<echo>}` |
| `error` | `{"t":"error","msg":"…"}` |

**Snapshot entity encoding.** `full=true` (first snapshot per client):
`p` entries are `[id,x,y,vx,vy,aim,hp,alive01,score,kills]`,
`b` entries are `[id,x,y,vx,vy,owner,ttl]`.
Otherwise each entry is a delta `[id, bitmask, changed-fields…]` where the
bitmask selects fields in ascending bit order:

```
player bits: 1=x 2=y 4=vx 8=vy 16=aim 32=hp 64=alive 128=score 256=kills
bullet bits: 1=x 2=y 4=vx 8=vy 16=owner 32=ttl
```

New entities inside a delta use mask = all-bits. Removed bullets (and
disconnected players) are listed in `gone`. `events` carries
`["kill",killerId,victimId]` and `["respawn",id,x,y]`.

### Server authority / anti-cheat

The server **never trusts client positions** — clients send only inputs.
Every input is validated: sequence must increase (drops duplicates),
key bits are masked to the 4 legal bits, aim is NaN/Inf-checked and
normalized, and a sliding-window rate limiter caps 120 inputs/sec
(excess is dropped; chronic violators are counted for future kicks).
Speeds are clamped by the integrator itself, so teleporting or
speed-hacking is impossible by construction.

## Netcode algorithms

### Client-side prediction + reconciliation

The client runs the same movement integrator as the server at 60 Hz.
Local input applies *immediately* (no round-trip wait), and every input
is stored with its `seq`. On each snapshot the client compares its
predicted position with the authoritative one; if the error exceeds 2px
it snaps to the server state and **re-simulates all unacknowledged
inputs** on top. The HUD counts these corrections — on a LAN it should
stay near zero; on lossy links you see exactly how hard reconciliation
is working.

### Entity interpolation

Remote entities are rendered from a snapshot buffer with a **100 ms
render delay**: `renderTime = newestSnapshotTime − 100ms`, linearly
interpolating between the two bracketing snapshots. This hides 20 Hz
snapshot quantization and jitter at the cost of a constant 100 ms of
added visual latency (tunable). Extrapolation is capped at one tick;
beyond that entities freeze rather than guess wrong.

### Delta compression

Snapshots are diffed against the last broadcast baseline per room.
Unchanged entities are omitted entirely; changed ones send only a
bitmask + the changed fields (see layout above). A moving player
typically sends 2–3 fields (x, y, maybe vx/vy) instead of 10 — roughly
a 3–4× bandwidth reduction on the hot path. Floats compare with
ε = 1e-3 so deterministic float noise never spams deltas. The design
is wire-compatible with a future binary encoding (varints + the same
bitmask layout) without changing the algorithm.

### Lag compensation (server rewind)

The server keeps a **1-second ring buffer (60 ticks)** of every alive
player's position per room. Hit tests for a bullet are performed
against positions **rewound by the shooter's observed latency**:

```
lagTicks = clamp(serverTick − shooter.ctick, 0, 59)
victimPos = history[now − lagTicks][victim]
hit = segmentHitsCircle(bulletPrev, bulletNow, victimPos, r)
```

Each shooter gets hit tests in *their own* past — the classic
Source-engine model — so a 150 ms player sees hits land where their
crosshair was, while the victim's own movement remains authoritative
and un-rewound. Continuous collision (segment-vs-circle) prevents
tunneling at 700 u/s bullet speeds.

## Tuning knobs (`server/config.go`)

| Knob | Default | Effect |
|---|---|---|
| `TickRate` | 60 | Sim frequency; raise for tighter physics at higher CPU cost |
| `SnapshotRate` | 20 | Broadcast Hz; raise for smoother remotes at higher bandwidth |
| `MaxPlayersPerRoom` | 8 | Room capacity |
| `PlayerMaxSpeed` / `PlayerAccel` | 320 / 2400 | Movement feel (client must mirror!) |
| `BulletSpeed` / `FireCooldown` | 700 / 0.25s | Weapon feel |
| `HistoryTicks` | 60 | Lag-comp window (1 s); raise for high-latency regions |
| `InputRateLimit` | 120/s | Anti-cheat input cap |

Client knobs (`web/game.js`): `INTERP_DELAY` (100 ms), prediction
error threshold (2 px), input send rate (30 Hz).

## Verification

```bash
go build ./...   # clean
go vet ./...     # clean
go test ./...    # 3 tests, all pass
```

* `TestMultiplayerIntegration` — boots the real HTTP+WS server, connects
  **6 scripted WebSocket clients** sending inputs at 30 Hz for ~10 s,
  and asserts: every client got `welcome` + a full snapshot, ~200
  snapshots each (≈20 Hz), all 6 player entities visible to every
  client, zero protocol errors, one shared room, and a non-empty
  lag-compensation history buffer.
* `TestStepDeterminism` — two identical 600-tick sim runs produce
  bit-identical state.
* `TestValidateInput` — duplicate seq dropped, key bits clamped,
  120/s rate limit enforced.

## Project structure

```
arena-game/
├── cmd/arena-server/main.go   # entrypoint: HTTP+WS server, embedded client
├── server/
│   ├── config.go              # tuning knobs, bitmask layouts, arena geometry
│   ├── proto.go               # wire protocol message types
│   ├── sim.go                 # deterministic physics + game logic
│   ├── lagcomp.go             # 1s position history ring buffer
│   ├── netcode.go             # bitmask delta snapshot encoder
│   ├── room.go                # per-arena goroutine, fixed-timestep loop
│   ├── hub.go                 # rooms, matchmaking, WS upgrade
│   ├── conn.go                # per-connection read/write pumps
│   ├── static.go              # go:embed web client
│   └── integration_test.go    # 6-client live test + unit tests
├── web/                       # PixiJS client (source of truth)
│   ├── index.html | style.css | game.js
├── sync-web.sh                # syncs web/ -> server/web/ before build
└── README.md
```

## Roadmap

- [ ] **UDP/WebTransport transport** — unreliable sequenced channel for
      inputs/snapshots; keep TCP WS as fallback.
- [ ] Binary snapshot encoding (varints + the existing bitmask layout).
- [ ] Server regions / edge placement + latency-based matchmaking.
- [ ] Server-side replay recording (deterministic sim makes this cheap).
- [ ] Mobile touch controls (virtual joystick + fire button).
- [ ] Anti-cheat hardening: kick chronic rate-limit violators,
      statistical aim/move anomaly detection.
- [ ] Horizontal scaling: rooms sharded across processes with a
      Redis-backed lobby directory.

---

## CI, container image & releases

| What | Where |
| --- | --- |
| Build + tests | [`.github/workflows/ci.yml`](.github/workflows/ci.yml) — `go vet`, `go test -race ./...`, `go build ./...` |
| Container image | `ghcr.io/syedalisaleem/arena-game:latest` (published on every push to `main`) |
| Release binaries | [Releases](https://github.com/syedalisaleem/arena-game/releases) — `v*` tags cross-compile linux/amd64, linux/arm64, windows/amd64 and darwin/arm64 |

Run the published image (client + server on one port, exactly as in production):

```bash
docker run --rm -p 8080:8080 ghcr.io/syedalisaleem/arena-game:latest
# open http://localhost:8080
```

GitHub Pages cannot host this project on its own: the authoritative simulation is a Go
WebSocket server, so the static client would have nowhere to connect. The Docker image (or a
release binary) is the deployable artifact instead.