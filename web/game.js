'use strict';
/* =====================================================================
 * NEON ARENA - web client
 *
 * PixiJS v7, plain JS, no build step. Talks to the Go authoritative
 * server over JSON text frames on ws://<host>/ws.
 *
 * Netcode:
 *   - client-side prediction of the local player (60Hz fixed model)
 *   - reconciliation against server snapshots (server sends no seq
 *     acks; inputs sent since the last snapshot arrival are replayed)
 *   - entity interpolation for remote players (100ms render delay)
 * ===================================================================== */

// ---------------- 1. Config & constants ----------------
const ARENA_W = 1600, ARENA_H = 900;   // world size (server authoritative)
const PLAYER_R = 18;                  // player collision radius
const MAX_SPEED = 320;                // px/s
const ACCEL = 2400;                   // px/s^2
const FRICTION = 8;                   // 1/s damping when no input held
const DT = 1 / 60;                    // fixed simulation step (must match server)
const INPUT_HZ = 30;                  // input send rate
const INPUT_MS = 1000 / INPUT_HZ;
const INTERP_DELAY = 100;             // ms of render delay for interpolation
const RECONCILE_EPS = 2;              // px; corrections counted above this
const SNAP_BUF_MAX = 40;              // interpolation history depth
const PING_MS = 2000;

// ---------------- 2. Small utilities ----------------
const clamp = (v, a, b) => v < a ? a : v > b ? b : v;
const lerp = (a, b, t) => a + (b - a) * t;

/** shortest-path angle lerp (radians) */
function lerpAngle(a, b, t) {
  let d = (b - a) % (Math.PI * 2);
  if (d > Math.PI) d -= Math.PI * 2;
  if (d < -Math.PI) d += Math.PI * 2;
  return a + d * t;
}

/** stable player color from numeric id */
function colorOf(id) {
  const h = ((Number(id) * 137.508) % 360 + 360) % 360; // golden-angle hash
  const s = 0.85, l = 0.6;
  const c = (1 - Math.abs(2 * l - 1)) * s;
  const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
  const m = l - c / 2;
  let r = 0, g = 0, b = 0;
  if (h < 60) { r = c; g = x; } else if (h < 120) { r = x; g = c; }
  else if (h < 180) { g = c; b = x; } else if (h < 240) { g = x; b = c; }
  else if (h < 300) { r = x; b = c; } else { r = c; b = x; }
  return ((r + m) * 255 << 16) | ((g + m) * 255 << 8) | ((b + m) * 255);
}

const displayName = (id) => S.nameById.get(Number(id)) || ('#' + id);

// ---------------- 3. Global state ----------------
const S = {
  ws: null,
  joined: false,
  myId: null,
  room: null,
  serverTick: 0,
  lastCtick: 0,            // last snapshot tick received (0 before first snap)
  arena: { w: ARENA_W, h: ARENA_H, obstacles: [] },
  players: new Map(),      // id -> latest server-known player state
  bullets: new Map(),      // id -> latest server-known bullet state
  nameById: new Map(),
  // predicted local state (client-side prediction)
  self: { x: ARENA_W / 2, y: ARENA_H / 2, vx: 0, vy: 0, aim: 0, hp: 100, alive: 1, score: 0, kills: 0 },
  serverSelf: null,        // last server snapshot of self (for debugging)
  seq: 0,
  unacked: [],             // inputs sent since last snapshot arrival (replay pool)
  activeInput: { keys: 0, aim: 0, fire: false }, // input currently driving the sim
  keysDown: new Set(),
  mouse: { x: 0, y: 0, fire: false },
  snapBuf: [],             // [{tick, tArrive, players:Map, bullets:Map}] for interpolation
  particles: [],           // {x,y,vx,vy,life,maxLife,color,size}
  rtt: 0,
  stats: { in: 0, out: 0, snaps: 0, corrections: 0 },
  perSec: { in: 0, out: 0, snaps: 0 },
  showHud: true,
  flashT: 0,               // muzzle-flash timestamp (local player)
  prevFire: false,
  lastHudUpdate: 0,
  lastStatReset: performance.now(),
};

// ---------------- 4. Movement model (MUST mirror the server) ----------------
/**
 * Fixed-step movement: accel toward input dir, friction when idle,
 * speed clamp, wall clamp, circle-vs-AABB obstacle push-out.
 * Mutates `s` ({x,y,vx,vy}). `keys` is the bitmask 1=up,2=down,4=left,8=right.
 */
function stepMovement(s, keys, dt, arena) {
  let ix = 0, iy = 0;
  if (keys & 1) iy -= 1;   // up (W)
  if (keys & 2) iy += 1;   // down (S)
  if (keys & 4) ix -= 1;   // left (A)
  if (keys & 8) ix += 1;   // right (D)
  if (ix !== 0 || iy !== 0) {
    const len = Math.hypot(ix, iy);
    ix /= len; iy /= len;
    s.vx += ix * ACCEL * dt;
    s.vy += iy * ACCEL * dt;
  } else {
    const f = Math.max(0, 1 - FRICTION * dt);
    s.vx *= f;
    s.vy *= f;
  }
  const sp = Math.hypot(s.vx, s.vy);
  if (sp > MAX_SPEED) {
    s.vx = s.vx / sp * MAX_SPEED;
    s.vy = s.vy / sp * MAX_SPEED;
  }
  s.x += s.vx * dt;
  s.y += s.vy * dt;

  // walls: clamp to [R, w-R], zero the velocity component on clamp
  const minX = PLAYER_R, maxX = arena.w - PLAYER_R;
  const minY = PLAYER_R, maxY = arena.h - PLAYER_R;
  if (s.x < minX) { s.x = minX; if (s.vx < 0) s.vx = 0; }
  if (s.x > maxX) { s.x = maxX; if (s.vx > 0) s.vx = 0; }
  if (s.y < minY) { s.y = minY; if (s.vy < 0) s.vy = 0; }
  if (s.y > maxY) { s.y = maxY; if (s.vy > 0) s.vy = 0; }

  // obstacles: circle (r=PLAYER_R) vs AABB, push out along smallest
  // penetration axis, zero velocity component into the surface
  for (const o of arena.obstacles) {
    const ox = o[0], oy = o[1], ow = o[2], oh = o[3];
    const cx = clamp(s.x, ox, ox + ow);
    const cy = clamp(s.y, oy, oy + oh);
    const dx = s.x - cx, dy = s.y - cy;
    const dist = Math.hypot(dx, dy);
    if (dist >= PLAYER_R) continue;
    if (dist > 0.0001) {
      // closest point on box: push out along the normal
      const nx = dx / dist, ny = dy / dist;
      s.x = cx + nx * PLAYER_R;
      s.y = cy + ny * PLAYER_R;
      const vn = s.vx * nx + s.vy * ny;
      if (vn < 0) { s.vx -= vn * nx; s.vy -= vn * ny; }
    } else {
      // center inside the box: push out along smallest penetration axis
      const left = s.x - ox, right = ox + ow - s.x;
      const top = s.y - oy, bottom = oy + oh - s.y;
      const m = Math.min(left, right, top, bottom);
      if (m === left) { s.x = ox - PLAYER_R; if (s.vx > 0) s.vx = 0; }
      else if (m === right) { s.x = ox + ow + PLAYER_R; if (s.vx < 0) s.vx = 0; }
      else if (m === top) { s.y = oy - PLAYER_R; if (s.vy > 0) s.vy = 0; }
      else { s.y = oy + oh + PLAYER_R; if (s.vy < 0) s.vy = 0; }
    }
  }
}

// ---------------- 5. Networking ----------------
function wsUrl() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const host = location.host || 'localhost:8080';
  return proto + '//' + host + '/ws';
}

function sendJSON(obj) {
  if (S.ws && S.ws.readyState === WebSocket.OPEN) {
    S.ws.send(JSON.stringify(obj));
    S.stats.out++;
  }
}

function connect(playerName) {
  setLobbyStatus('connecting...');
  const ws = new WebSocket(wsUrl());
  S.ws = ws;

  ws.onopen = () => {
    setLobbyStatus('connected, joining...');
    // join MUST be the first message after connect
    sendJSON({ t: 'join', name: playerName });
  };

  ws.onmessage = (ev) => {
    S.stats.in++;
    let msg;
    try { msg = JSON.parse(ev.data); }
    catch (e) { console.warn('[arena] non-JSON frame', e); return; }
    handleMessage(msg);
  };

  ws.onerror = () => setLobbyStatus('connection error');

  ws.onclose = () => {
    S.joined = false;
    if (S.myId !== null) show('disconnect-overlay'); // only after we had joined
    else setLobbyStatus('connection closed');
  };
}

function handleMessage(msg) {
  if (!msg || typeof msg.t !== 'string') {
    console.warn('[arena] malformed message', msg);
    return;
  }
  switch (msg.t) {
    case 'welcome': onWelcome(msg); break;
    case 'snap': onSnap(msg); break;
    case 'roster': onRoster(msg); break;
    case 'pong': S.rtt = Date.now() - msg.ct; break;
    case 'error': showError(msg.msg || 'server error'); break;
    default:
      // future server extensions / unknown types: log, never crash
      console.log('[arena] unknown server message type:', msg.t, msg);
  }
}

function onWelcome(msg) {
  S.myId = msg.id;
  S.room = msg.room;
  S.serverTick = msg.tick || 0;
  if (msg.arena) {
    S.arena.w = msg.arena.w || ARENA_W;
    S.arena.h = msg.arena.h || ARENA_H;
    S.arena.obstacles = msg.arena.obstacles || [];
  }
  for (const p of (msg.players || [])) S.nameById.set(Number(p.id), p.name);
  S.self.x = S.arena.w / 2; S.self.y = S.arena.h / 2;
  S.self.vx = S.self.vy = 0; S.self.hp = 100; S.self.alive = 1;
  S.joined = true;
  hide('lobby'); show('brand'); show('hud'); show('scoreboard');
  show('killfeed'); show('controls-hint');
  document.getElementById('brand-room').textContent = 'room ' + S.room;
  drawStaticWorld(); // arena size/obstacles now known
  requestScoreboardSeed();
}

/** ask roster once at join so the scoreboard populates immediately */
function requestScoreboardSeed() { /* server pushes roster; nothing to send */ }

// ---------------- 6. Snapshot decoding ----------------
// full player:  [id,x,y,vx,vy,aim,hp,alive,score,kills]
// delta player: [id,mask,v1,v2,...], mask bits map to the same field order
//   bit0=x bit1=y bit2=vx bit3=vy bit4=aim bit5=hp bit6=alive bit7=score bit8=kills
// full bullet:  [id,x,y,vx,vy,owner,ttl]
// delta bullet: [id,mask,...], bit0=x bit1=y bit2=vx bit3=vy bit4=owner bit5=ttl
// (new bullets are always sent full => mask with all bits set)
const P_FIELDS = ['x', 'y', 'vx', 'vy', 'aim', 'hp', 'alive', 'score', 'kills'];
const B_FIELDS = ['x', 'y', 'vx', 'vy', 'owner', 'ttl'];

function fullPlayer(e) {
  return {
    id: e[0], x: e[1], y: e[2], vx: e[3], vy: e[4], aim: e[5],
    hp: e[6], alive: e[7], score: e[8], kills: e[9],
  };
}

function fullBullet(e) {
  return { id: e[0], x: e[1], y: e[2], vx: e[3], vy: e[4], owner: e[5], ttl: e[6] };
}

function decodeMasked(fields, entry) {
  const id = entry[0], mask = entry[1], vals = entry.slice(2);
  const upd = { id };
  let vi = 0;
  for (let b = 0; b < fields.length; b++) {
    if (mask & (1 << b)) upd[fields[b]] = vals[vi++];
  }
  return upd;
}

const DEFAULT_PLAYER = { x: 800, y: 450, vx: 0, vy: 0, aim: 0, hp: 100, alive: 1, score: 0, kills: 0 };
const DEFAULT_BULLET = { x: 0, y: 0, vx: 0, vy: 0, owner: 0, ttl: 0 };

function onSnap(msg) {
  S.serverTick = msg.tick;
  S.lastCtick = msg.tick;   // feeds ctick in the next input message (0 before first snap)
  S.stats.snaps++;
  const tArrive = performance.now();

  if (msg.full) {
    const np = new Map();
    for (const e of (msg.p || [])) np.set(e[0], fullPlayer(e));
    S.players = np;
    const nb = new Map();
    for (const e of (msg.b || [])) nb.set(e[0], fullBullet(e));
    S.bullets = nb;
  } else {
    for (const e of (msg.p || [])) {
      const upd = decodeMasked(P_FIELDS, e);
      const old = S.players.get(upd.id);
      // never mutate in place: buffered snapshots keep referencing old objects
      S.players.set(upd.id, old ? Object.assign({}, old, upd) : Object.assign({}, DEFAULT_PLAYER, upd));
    }
    for (const e of (msg.b || [])) {
      const upd = decodeMasked(B_FIELDS, e);
      const old = S.bullets.get(upd.id);
      S.bullets.set(upd.id, old ? Object.assign({}, old, upd) : Object.assign({}, DEFAULT_BULLET, upd));
    }
    for (const id of (msg.gone || [])) S.bullets.delete(id);
  }

  // keep an immutable-per-snapshot history for entity interpolation
  S.snapBuf.push({ tick: msg.tick, tArrive, players: new Map(S.players), bullets: new Map(S.bullets) });
  if (S.snapBuf.length > SNAP_BUF_MAX) S.snapBuf.shift();

  for (const ev of (msg.events || [])) handleEvent(ev);

  reconcile();
}

// ---------------- 7. Client-side prediction & reconciliation ----------------
/**
 * The server sends no input-seq acks, so we approximate: every input sent
 * since the previous snapshot arrival is "unacked". On each snapshot we
 * compare predicted self position vs server position; if the error exceeds
 * RECONCILE_EPS we snap to the server state and re-simulate all unacked
 * inputs (each input covers 2 fixed ticks at 30Hz send / 60Hz sim).
 * Corrections are counted for the debug HUD.
 */
function reconcile() {
  const srv = S.players.get(S.myId);
  if (!srv) { S.unacked.length = 0; return; }
  S.serverSelf = Object.assign({}, srv);

  const err = Math.hypot(S.self.x - srv.x, S.self.y - srv.y);
  if (err > RECONCILE_EPS) {
    S.stats.corrections++;
    S.self.x = srv.x; S.self.y = srv.y;
    S.self.vx = srv.vx; S.self.vy = srv.vy;
    for (const inp of S.unacked) {
      stepMovement(S.self, inp.keys, DT, S.arena);
      stepMovement(S.self, inp.keys, DT, S.arena);
    }
  }
  // server-authoritative non-positional fields
  S.self.hp = srv.hp; S.self.alive = srv.alive;
  S.self.score = srv.score; S.self.kills = srv.kills;
  S.unacked.length = 0;
}

// ---------------- 8. Entity interpolation ----------------
/**
 * Remote players render at renderTime = latest snapshot arrival - 100ms,
 * linearly interpolated between the two bracketing snapshots. Beyond the
 * newest snapshot we extrapolate at most one tick (16.7ms) using velocity,
 * then freeze. Bullets are not interpolated (rendered at latest state).
 */
function interpPlayer(id, renderTime) {
  const buf = S.snapBuf;
  if (!buf.length) return null;
  let a = null, b = null;
  for (let i = 0; i < buf.length; i++) {
    if (buf[i].tArrive <= renderTime) a = buf[i];
    else { b = buf[i]; break; }
  }
  if (!a) return buf[0].players.get(id) || null;      // before oldest: freeze
  const pa = a.players.get(id);
  if (!b) {                                            // past newest: extrapolate <= 1 tick
    if (!pa) return null;
    const dt = Math.min(renderTime - a.tArrive, 1000 / 60) / 1000;
    return Object.assign({}, pa, { x: pa.x + pa.vx * dt, y: pa.y + pa.vy * dt });
  }
  const pb = b.players.get(id);
  if (!pa) return pb || null;
  if (!pb) return pa;
  const span = b.tArrive - a.tArrive;
  const t = span > 0 ? clamp((renderTime - a.tArrive) / span, 0, 1) : 1;
  return Object.assign({}, pa, {
    x: lerp(pa.x, pb.x, t),
    y: lerp(pa.y, pb.y, t),
    aim: lerpAngle(pa.aim, pb.aim, t),
  });
}

// ---------------- 9. Events: kills, respawns ----------------
function handleEvent(ev) {
  if (!Array.isArray(ev) || !ev.length) return;
  if (ev[0] === 'kill') {
    const [, killerId, victimId] = ev;
    const v = S.players.get(victimId);
    const pos = v ? { x: v.x, y: v.y } : { x: S.arena.w / 2, y: S.arena.h / 2 };
    spawnBurst(pos.x, pos.y, colorOf(victimId), 26);
    addKillFeed(killerId, victimId);
  } else if (ev[0] === 'respawn') {
    const [, id, x, y] = ev;
    spawnBurst(x, y, 0x22d3ee, 10);
  } else {
    console.log('[arena] unknown event type:', ev[0], ev);
  }
}

// ---------------- 10. Input capture & sending (30Hz) ----------------
function currentKeys() {
  let k = 0;
  const d = S.keysDown;
  if (d.has('KeyW') || d.has('ArrowUp')) k |= 1;
  if (d.has('KeyS') || d.has('ArrowDown')) k |= 2;
  if (d.has('KeyA') || d.has('ArrowLeft')) k |= 4;
  if (d.has('KeyD') || d.has('ArrowRight')) k |= 8;
  return k;
}

/** aim angle from predicted self position to mouse (world coords) */
function currentAim() {
  const w = screenToWorld(S.mouse.x, S.mouse.y);
  return Math.atan2(w.y - S.self.y, w.x - S.self.x); // 0 = facing right, y down
}

function sendInput() {
  if (!S.joined) return;
  const keys = currentKeys();
  const aim = currentAim();
  const fire = S.mouse.fire;
  S.seq++;
  const input = { seq: S.seq, keys, aim, fire };
  sendJSON({ t: 'input', seq: input.seq, ctick: S.lastCtick, keys, aim, fire });
  // remember for reconciliation replay (each input spans 2 sim ticks)
  S.unacked.push(input);
  if (S.unacked.length > 300) S.unacked.shift();
  S.activeInput = { keys, aim, fire };
  if (fire && !S.prevFire) S.flashT = performance.now(); // muzzle-flash edge
  S.prevFire = fire;
}

function sendPing() {
  if (!S.joined) return;
  sendJSON({ t: 'ping', ct: Date.now() });
}

// ---------------- 11. Rendering (PixiJS v7) ----------------
let app = null, world = null;
const view = { s: 1, ox: 0, oy: 0 };      // camera: fixed full-arena fit
const playerViews = new Map();            // id -> {c,body,aimLine,name,hpBg,hpFg,ring,flash,color,lastHp}
const bulletViews = new Map();            // id -> PIXI.Sprite
const bulletTexCache = new Map();         // color -> glow texture
let staticLayer = null, particleGfx = null;

const NAME_STYLE = new PIXI.TextStyle({
  fontFamily: '-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif',
  fontSize: 13, fill: '#ffffff', stroke: '#05070d', strokeThickness: 3,
});

function initPixi() {
  app = new PIXI.Application({
    width: window.innerWidth,
    height: window.innerHeight,
    backgroundColor: 0x05070d,
    antialias: true,
  });
  document.getElementById('game-container').appendChild(app.view);
  world = new PIXI.Container();
  app.stage.addChild(world);
  staticLayer = new PIXI.Graphics();
  world.addChild(staticLayer);
  particleGfx = new PIXI.Graphics();
  world.addChild(particleGfx); // particles on top
  window.addEventListener('resize', fitCamera);
  fitCamera();
}

/** fixed camera: fit the whole arena into the window, letterboxed */
function fitCamera() {
  const w = window.innerWidth, h = window.innerHeight;
  app.renderer.resize(w, h);
  const s = Math.min(w / S.arena.w, h / S.arena.h);
  view.s = s;
  view.ox = (w - S.arena.w * s) / 2;
  view.oy = (h - S.arena.h * s) / 2;
  world.scale.set(s);
  world.position.set(view.ox, view.oy);
}

function screenToWorld(mx, my) {
  return { x: (mx - view.ox) / view.s, y: (my - view.oy) / view.s };
}

/** static geometry: bg, grid, obstacles, boundary walls (rebuilt on welcome) */
function drawStaticWorld() {
  const g = staticLayer;
  g.clear();
  const W = S.arena.w, H = S.arena.h;
  // dark arena background
  g.beginFill(0x0a0e1a);
  g.drawRect(0, 0, W, H);
  g.endFill();
  // subtle grid
  g.lineStyle(1, 0x22d3ee, 0.05);
  for (let x = 80; x < W; x += 80) { g.moveTo(x, 0); g.lineTo(x, H); }
  for (let y = 80; y < H; y += 80) { g.moveTo(0, y); g.lineTo(W, y); }
  // obstacles
  for (const o of S.arena.obstacles) {
    g.beginFill(0x141b2e, 1);
    g.lineStyle(2, 0x22d3ee, 0.55);
    g.drawRoundedRect(o[0], o[1], o[2], o[3], 8);
    g.endFill();
  }
  // boundary walls
  g.lineStyle(3, 0x22d3ee, 0.9);
  g.drawRect(0, 0, W, H);
  fitCamera();
}

function getPlayerView(id) {
  let v = playerViews.get(id);
  if (v) return v;
  const color = colorOf(id);
  const c = new PIXI.Container();
  const ring = new PIXI.Graphics();   // self highlight
  const body = new PIXI.Graphics();
  const aimLine = new PIXI.Graphics();
  const name = new PIXI.Text(displayName(id), NAME_STYLE);
  name.anchor.set(0.5, 1);
  name.position.y = -28;
  const hpBg = new PIXI.Graphics();
  const hpFg = new PIXI.Graphics();
  hpBg.position.y = -22; hpFg.position.y = -22;
  const flash = new PIXI.Graphics();  // muzzle flash
  c.addChild(ring, body, aimLine, hpBg, hpFg, name, flash);
  world.addChild(c);
  v = { c, ring, body, aimLine, name, hpBg, hpFg, flash, color, lastHp: -1, lastAim: 999 };
  drawPlayerBody(v);
  drawHpBar(v, 100);
  playerViews.set(id, v);
  return v;
}

function drawPlayerBody(v) {
  const col = '#' + v.color.toString(16).padStart(6, '0');
  v.body.clear();
  v.body.beginFill(v.color, 0.28);
  v.body.drawCircle(0, 0, PLAYER_R);
  v.body.endFill();
  v.body.lineStyle(2.5, v.color, 1);
  v.body.drawCircle(0, 0, PLAYER_R - 1);
  // inner core
  v.body.beginFill(0xffffff, 0.85);
  v.body.drawCircle(0, 0, 5);
  v.body.endFill();
  v.ring.clear();
  v.ring.lineStyle(2, 0xffffff, 0.9);
  v.ring.drawCircle(0, 0, PLAYER_R + 5);
  void col;
}

function drawHpBar(v, hp) {
  const W = 40, H = 5;
  v.hpBg.clear();
  v.hpBg.beginFill(0x05070d, 0.85);
  v.hpBg.drawRect(-W / 2, -H / 2, W, H);
  v.hpBg.endFill();
  v.hpFg.clear();
  const frac = clamp(hp / 100, 0, 1);
  const col = frac > 0.5 ? 0x4ade80 : frac > 0.25 ? 0xfbbf24 : 0xff2d78;
  v.hpFg.beginFill(col, 1);
  v.hpFg.drawRect(-W / 2, -H / 2, W * frac, H);
  v.hpFg.endFill();
}

function drawAimLine(v, aim) {
  v.aimLine.clear();
  v.aimLine.lineStyle(2.5, v.color, 0.95);
  v.aimLine.moveTo(0, 0);
  v.aimLine.lineTo(Math.cos(aim) * (PLAYER_R + 14), Math.sin(aim) * (PLAYER_R + 14));
}

function drawFlash(v) {
  v.flash.clear();
  v.flash.beginFill(0xfff7cc, 0.95);
  const a = S.self.aim;
  const mx = Math.cos(a) * (PLAYER_R + 6), my = Math.sin(a) * (PLAYER_R + 6);
  v.flash.drawCircle(mx, my, 7);
  v.flash.endFill();
}

/** glowing bullet texture, cached per owner color */
function bulletTexture(color) {
  let tex = bulletTexCache.get(color);
  if (tex) return tex;
  const g = new PIXI.Graphics();
  g.beginFill(color, 0.30); g.drawCircle(0, 0, 11); g.endFill();
  g.beginFill(color, 0.85); g.drawCircle(0, 0, 5.5); g.endFill();
  g.beginFill(0xffffff, 1); g.drawCircle(0, 0, 2.4); g.endFill();
  tex = app.renderer.generateTexture(g);
  g.destroy();
  bulletTexCache.set(color, tex);
  return tex;
}

function syncBullets() {
  // create / move
  for (const [id, b] of S.bullets) {
    let spr = bulletViews.get(id);
    if (!spr) {
      spr = new PIXI.Sprite(bulletTexture(colorOf(b.owner)));
      spr.anchor.set(0.5);
      world.addChild(spr);
      bulletViews.set(id, spr);
    }
    spr.position.set(b.x, b.y);
  }
  // remove stale
  for (const [id, spr] of bulletViews) {
    if (!S.bullets.has(id)) {
      world.removeChild(spr);
      spr.destroy();
      bulletViews.delete(id);
    }
  }
}

function renderPlayers() {
  const latest = S.snapBuf.length ? S.snapBuf[S.snapBuf.length - 1].tArrive : performance.now();
  const renderTime = latest - INTERP_DELAY;
  const seen = new Set();

  // self: predicted position, server-authoritative hp/alive
  if (S.myId !== null) {
    const v = getPlayerView(S.myId);
    seen.add(S.myId);
    v.c.position.set(S.self.x, S.self.y);
    v.c.alpha = S.self.alive ? 1 : 0.35;
    v.ring.visible = true;
    v.name.text = displayName(S.myId);
    if (v.lastHp !== S.self.hp) { drawHpBar(v, S.self.hp); v.lastHp = S.self.hp; }
    if (Math.abs(v.lastAim - S.self.aim) > 0.01) { drawAimLine(v, S.self.aim); v.lastAim = S.self.aim; }
    if (performance.now() - S.flashT < 80) drawFlash(v); else v.flash.clear();
  }

  // remotes: interpolated
  for (const [id] of S.players) {
    if (id === S.myId) continue;
    const st = interpPlayer(id, renderTime);
    if (!st) continue;
    const v = getPlayerView(id);
    seen.add(id);
    v.c.position.set(st.x, st.y);
    v.c.alpha = st.alive ? 1 : 0.3;
    v.ring.visible = false;
    v.flash.clear();
    const nm = displayName(id);
    if (v.name.text !== nm) v.name.text = nm;
    if (v.lastHp !== st.hp) { drawHpBar(v, st.hp); v.lastHp = st.hp; }
    if (Math.abs(v.lastAim - st.aim) > 0.01) { drawAimLine(v, st.aim); v.lastAim = st.aim; }
  }

  // drop views for players that left
  for (const [id, v] of playerViews) {
    if (!seen.has(id)) {
      world.removeChild(v.c);
      v.c.destroy({ children: true });
      playerViews.delete(id);
    }
  }
}

// ---------------- 12. Particles ----------------
function spawnBurst(x, y, color, n) {
  for (let i = 0; i < n; i++) {
    const a = Math.random() * Math.PI * 2;
    const sp = 60 + Math.random() * 260;
    S.particles.push({
      x, y,
      vx: Math.cos(a) * sp, vy: Math.sin(a) * sp,
      life: 0, maxLife: 0.45 + Math.random() * 0.4,
      color, size: 2 + Math.random() * 3.5,
    });
  }
  if (S.particles.length > 600) S.particles.splice(0, S.particles.length - 600);
}

function updateParticles(dt) {
  const g = particleGfx;
  g.clear();
  for (let i = S.particles.length - 1; i >= 0; i--) {
    const p = S.particles[i];
    p.life += dt;
    if (p.life >= p.maxLife) { S.particles.splice(i, 1); continue; }
    p.x += p.vx * dt; p.y += p.vy * dt;
    p.vx *= (1 - 2.5 * dt); p.vy *= (1 - 2.5 * dt);
    const frac = 1 - p.life / p.maxLife;
    g.beginFill(p.color, frac);
    g.drawCircle(p.x, p.y, p.size * frac + 0.5);
    g.endFill();
  }
}

// ---------------- 13. DOM: HUD, scoreboard, kill feed, overlays ----------------
const $ = (id) => document.getElementById(id);
const show = (id) => $(id).classList.remove('hidden');
const hide = (id) => $(id).classList.add('hidden');

function setLobbyStatus(t) { $('lobby-status').textContent = t; }

let errorTimer = null;
function showError(msg) {
  const el = $('error-banner');
  el.textContent = 'server: ' + msg;
  show('error-banner');
  clearTimeout(errorTimer);
  errorTimer = setTimeout(() => hide('error-banner'), 5000);
}

function onRoster(msg) {
  const players = (msg.players || []).slice();
  for (const p of players) S.nameById.set(Number(p.id), p.name);
  players.sort((a, b) => (b.score || 0) - (a.score || 0) || (b.kills || 0) - (a.kills || 0));
  const ul = $('roster-list');
  ul.innerHTML = '';
  players.slice(0, 10).forEach((p, i) => {
    const li = document.createElement('li');
    if (Number(p.id) === S.myId) li.className = 'me';
    const crown = i === 0 ? '<span class="r-crown">#1</span>' : '';
    li.innerHTML = '<span class="r-name">' + crown + escapeHtml(p.name) + '</span>' +
      '<span class="r-stats">' + (p.kills || 0) + 'K / ' + (p.score || 0) + '</span>';
    ul.appendChild(li);
  });
  $('hud-players').textContent = players.length;
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

function addKillFeed(killerId, victimId) {
  const feed = $('killfeed');
  const div = document.createElement('div');
  const me = Number(killerId) === S.myId;
  div.className = 'kill-entry' + (me ? ' me-kill' : '');
  div.innerHTML = '<span class="k-killer">' + escapeHtml(displayName(killerId)) + '</span>' +
    '<span class="k-arrow">-&gt;</span>' +
    '<span class="k-victim">' + escapeHtml(displayName(victimId)) + '</span>';
  feed.prepend(div);
  while (feed.children.length > 5) feed.removeChild(feed.lastChild);
}

function updateHud() {
  if (!S.showHud) return;
  $('hud-rtt').textContent = S.joined ? Math.round(S.rtt) + ' ms' : '-';
  $('hud-tick').textContent = S.serverTick;
  $('hud-snaps').textContent = S.perSec.snaps;
  $('hud-pin').textContent = S.perSec.in;
  $('hud-pout').textContent = S.perSec.out;
  $('hud-interp').textContent = INTERP_DELAY + 'ms';
  $('hud-corr').textContent = S.stats.corrections;
  $('hud-pos').textContent = Math.round(S.self.x) + ', ' + Math.round(S.self.y);
}

// ---------------- 14. Main loop ----------------
let lastTs = 0, simAcc = 0, inputAcc = 0, pingAcc = 0;

function frame(ts) {
  requestAnimationFrame(frame);
  if (!app) return;
  const dtMs = Math.min(ts - (lastTs || ts), 100);
  lastTs = ts;
  const dt = dtMs / 1000;

  if (S.joined) {
    // fixed-step prediction using the latest sent input
    simAcc += dtMs;
    let steps = 0;
    while (simAcc >= DT * 1000 && steps < 5) {
      if (S.self.alive) stepMovement(S.self, S.activeInput.keys, DT, S.arena);
      simAcc -= DT * 1000;
      steps++;
    }
    if (steps === 5) simAcc = 0; // drop backlog after tab stall

    // send input at 30Hz
    inputAcc += dtMs;
    while (inputAcc >= INPUT_MS) { sendInput(); inputAcc -= INPUT_MS; }

    // ping every 2s
    pingAcc += dtMs;
    if (pingAcc >= PING_MS) { sendPing(); pingAcc = PING_MS; }
  }

  renderPlayers();
  syncBullets();
  updateParticles(dt);

  // respawn overlay
  if (S.joined && !S.self.alive) show('dead-overlay'); else hide('dead-overlay');

  // per-second stats
  const now = performance.now();
  if (now - S.lastStatReset >= 1000) {
    S.perSec = { in: S.stats.in, out: S.stats.out, snaps: S.stats.snaps };
    S.stats.in = S.stats.out = S.stats.snaps = 0;
    S.lastStatReset = now;
  }
  if (now - S.lastHudUpdate >= 250) {
    updateHud();
    S.lastHudUpdate = now;
  }
}

// ---------------- 15. Input wiring & boot ----------------
function initInput() {
  window.addEventListener('keydown', (e) => {
    if (e.code === 'KeyH' && S.joined) {
      S.showHud = !S.showHud;
      $('hud').classList.toggle('hidden', !S.showHud);
      return;
    }
    if (['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'Space'].includes(e.code)) e.preventDefault();
    S.keysDown.add(e.code);
  });
  window.addEventListener('keyup', (e) => S.keysDown.delete(e.code));
  window.addEventListener('blur', () => S.keysDown.clear());

  const track = (e) => { S.mouse.x = e.clientX; S.mouse.y = e.clientY; };
  window.addEventListener('mousemove', track);
  window.addEventListener('mousedown', (e) => {
    if (e.button === 0 && S.joined) { S.mouse.fire = true; track(e); }
  });
  window.addEventListener('mouseup', (e) => { if (e.button === 0) S.mouse.fire = false; });
  window.addEventListener('contextmenu', (e) => { if (S.joined) e.preventDefault(); });

  $('join-btn').addEventListener('click', doJoin);
  $('name-input').addEventListener('keydown', (e) => { if (e.key === 'Enter') doJoin(); });
  $('reconnect-btn').addEventListener('click', () => location.reload());

  const saved = localStorage.getItem('arenaName');
  if (saved) $('name-input').value = saved;
}

function doJoin() {
  const btn = $('join-btn');
  if (btn.disabled) return;
  const name = $('name-input').value.trim() || 'Player';
  localStorage.setItem('arenaName', name);
  btn.disabled = true;
  $('name-input').disabled = true;
  connect(name);
}

initPixi();
initInput();
requestAnimationFrame(frame);
