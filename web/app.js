(() => {
  'use strict';

  // ── setup ────────────────────────────────────────────────────────────────
  const CFG = JSON.parse(document.getElementById('config').textContent || '{}');
  const $ = (id) => document.getElementById(id);
  const root = document.documentElement;
  const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
  const rand = (lo, hi) => lo + Math.random() * (hi - lo);
  const nf = new Intl.NumberFormat('en-US');
  const fmtInt = (n) => nf.format(Math.round(n || 0));
  const host = (() => { try { return new URL(CFG.hit).host; } catch (e) { return CFG.hit || ''; } })();

  function fmtMs(ms) {
    if (!ms) return '0 ms';
    if (ms < 1) return ms.toFixed(2) + ' ms';
    if (ms < 100) return ms.toFixed(1) + ' ms';
    if (ms < 10000) return Math.round(ms) + ' ms';
    return (ms / 1000).toFixed(1) + ' s';
  }
  const fmtNs = (ns) => fmtMs((ns || 0) / 1e6);
  function fmtClock(sec) {
    sec = Math.max(0, Math.round(sec || 0));
    const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
    if (h) return h + 'h ' + m + 'm';
    if (m) return m + 'm ' + String(s).padStart(2, '0') + 's';
    return s + 's';
  }
  function statusClass(s) {
    if (s >= 200 && s < 300) return 's2';
    if (s >= 300 && s < 400) return 's3';
    if (s >= 400 && s < 500) return 's4';
    if (s >= 500) return 's5';
    return 's0';
  }
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }
  function setText(id, text) {
    const e = $(id);
    if (e && e.textContent !== text) e.textContent = text;
  }
  function store(key, value) { try { localStorage.setItem(key, value); } catch (e) { /* private mode */ } }

  const state = {
    phase: 'connecting', serverT: 0, tAt: performance.now(), stats: null, finaleShown: false,
    lastFeedSeq: 0, rates: [],
  };
  const serverNow = () => state.serverT + (performance.now() - state.tAt) / 1000;

  // ── palette, theme, effects ──────────────────────────────────────────────
  const P = {};
  function readPalette() {
    const cs = getComputedStyle(root);
    ['bg', 'panel', 'panel-2', 'line', 'line-strong', 'text', 'muted', 'faint', 'pink', 'magenta', 'cyan',
      'yellow', 'orange', 'green', 'red', 'purple', 'sky-top', 'sky-mid', 'sky-low', 'horizon', 'sun-top',
      'sun-mid', 'sun-low', 'mountain', 'grid', 'floor', 'hair', 'robe', 'skin', 'shoe']
      .forEach((k) => { P[k] = cs.getPropertyValue('--' + k).trim(); });
    P.stars = parseFloat(cs.getPropertyValue('--stars')) || 0;
    P.dark = root.dataset.theme !== 'light';
  }
  const fxOn = () => root.dataset.fx !== 'off';
  function syncToggles() { $('fx-toggle').setAttribute('aria-pressed', String(fxOn())); }
  function toggleTheme() {
    const next = root.dataset.theme === 'light' ? 'dark' : 'light';
    root.dataset.theme = next;
    store('lemmings-theme', next);
    readPalette();
    scene.paintBackground();
    charts.redraw();
  }
  function toggleFx() {
    root.dataset.fx = fxOn() ? 'off' : 'on';
    store('lemmings-fx', root.dataset.fx);
    syncToggles();
  }
  $('theme-toggle').addEventListener('click', toggleTheme);
  $('fx-toggle').addEventListener('click', toggleFx);

  // ── the terrain: a synthwave scene of lemmings ───────────────────────────
  // Each lemming on stage is a sprite. The server tells us when it is born,
  // visits a page, queues, is admitted and dies; everything in between is
  // animation. Sprites are drawn as 8×10 pixel art.
  const SPRITE = {
    top: ['..HHH...', '.HHHHH..', '..SSSH..', '..SS....', '..BBB...', '.SBBBS..', '..BBB...', '..BBB...'],
    legs: [['..L.L...', '.LL.LL..'], ['.L...L..', 'LL...LL.'], ['..L.L...', '..L.L...']],
  };

  class Sprite {
    constructor(scene, slot, id, name, terrain, fromHatch) {
      this.slot = slot; this.id = id; this.name = name || ''; this.terrain = terrain || 0;
      this.lane = rand(scene.laneTop, scene.laneBottom);
      this.dir = Math.random() < 0.5 ? -1 : 1;
      this.speed = rand(24, 42);
      this.walked = rand(0, 20);
      this.hop = 0; this.vh = 0; this.daze = 0; this.glitch = 0; this.stumble = 0;
      this.flash = 0; this.flashColor = ''; this.alpha = 1; this.shrink = 1;
      this.queuePos = 0; this.flawless = false;
      if (fromHatch) {
        this.x = scene.hatchX + rand(-8, 8); this.y = scene.hatchY; this.vy = 0; this.mode = 'fall';
      } else {
        this.x = rand(scene.xMin, scene.xMax); this.y = this.lane; this.mode = 'walk';
      }
    }
  }

  const scene = {
    canvas: $('scene'), ctx: null, W: 0, H: 0, dpr: 1, unit: 1,
    bg: null, slots: new Map(), leaving: [], particles: [], floaters: [],
    hatchOpen: 0, gateGlow: 0, gateFlash: 0, exitGlow: 0, gridPhase: 0, hover: null, followed: null,

    init() {
      this.ctx = this.canvas.getContext('2d');
      new ResizeObserver(() => this.resize()).observe(this.canvas);
      this.canvas.addEventListener('pointermove', (e) => this.onPointer(e, false));
      this.canvas.addEventListener('pointerleave', () => { this.hover = null; this.canvas.style.cursor = 'crosshair'; });
      this.canvas.addEventListener('click', (e) => this.onPointer(e, true));
      this.resize();
    },

    resize() {
      const r = this.canvas.getBoundingClientRect();
      if (!r.width || !r.height) return;
      this.dpr = Math.min(window.devicePixelRatio || 1, 2);
      this.W = r.width; this.H = r.height;
      this.canvas.width = Math.round(r.width * this.dpr);
      this.canvas.height = Math.round(r.height * this.dpr);
      this.unit = clamp(this.W / 900, 0.7, 1.35);
      this.horizon = this.H * 0.5;
      this.laneTop = this.horizon + (this.H - this.horizon) * 0.3;
      this.laneBottom = this.H - 14 * this.unit;
      this.xMin = this.W * 0.14; this.xMax = this.W * 0.84;
      this.hatchX = this.W * 0.09; this.hatchY = this.horizon + 8 * this.unit;
      this.exitX = this.W * 0.925; this.exitY = this.laneTop + (this.laneBottom - this.laneTop) * 0.55;
      this.gateX = this.W * 0.6; this.queueY = this.laneTop + (this.laneBottom - this.laneTop) * 0.4;
      for (const s of this.all()) {
        s.x = clamp(s.x, 0, this.W);
        s.lane = clamp(s.lane, this.laneTop, this.laneBottom);
        if (s.mode === 'walk') s.y = s.lane;
      }
      this.paintBackground();
    },

    *all() { yield* this.slots.values(); yield* this.leaving; },

    depth(y) { return 0.62 + 0.62 * clamp((y - this.laneTop) / (this.laneBottom - this.laneTop), 0, 1); },

    // The sky, sun and mountains only change on resize and theme change.
    paintBackground() {
      if (!this.W) return;
      const W = this.W, H = this.H, hz = this.horizon, d = this.dpr;
      const bg = document.createElement('canvas');
      bg.width = Math.round(W * d); bg.height = Math.round(H * d);
      const c = bg.getContext('2d');
      c.scale(d, d);

      const sky = c.createLinearGradient(0, 0, 0, hz);
      sky.addColorStop(0, P['sky-top']); sky.addColorStop(0.6, P['sky-mid']); sky.addColorStop(1, P['sky-low']);
      c.fillStyle = sky; c.fillRect(0, 0, W, hz + 1);

      if (P.stars) {
        let seed = 7;
        const rnd = () => ((seed = (seed * 16807) % 2147483647) / 2147483647);
        for (let i = 0; i < 110; i++) {
          c.globalAlpha = 0.25 + rnd() * 0.6;
          c.fillStyle = '#fff';
          c.fillRect(rnd() * W, rnd() * hz * 0.8, rnd() < 0.15 ? 2 : 1, 1);
        }
        c.globalAlpha = 1;
      }

      // The sun: a gradient disc with the classic stripes cut out.
      const R = Math.min(H * 0.3, W * 0.17), cx = W * 0.5, cy = hz - R * 0.28;
      const sun = document.createElement('canvas');
      sun.width = Math.ceil(R * 2 * d); sun.height = Math.ceil(R * 2 * d);
      const sc = sun.getContext('2d');
      sc.scale(d, d);
      const sg = sc.createLinearGradient(0, 0, 0, R * 2);
      sg.addColorStop(0, P['sun-top']); sg.addColorStop(0.5, P['sun-mid']); sg.addColorStop(1, P['sun-low']);
      sc.fillStyle = sg; sc.beginPath(); sc.arc(R, R, R, 0, Math.PI * 2); sc.fill();
      sc.globalCompositeOperation = 'destination-out';
      for (let i = 0; i < 7; i++) {
        const y = R * (1.02 + i * 0.14), h = 1.5 + i * 1.6;
        sc.fillRect(0, y, R * 2, h);
      }
      if (P.dark) { c.shadowColor = P.magenta; c.shadowBlur = 40; }
      c.drawImage(sun, cx - R, cy - R, R * 2, R * 2);
      c.shadowBlur = 0;

      // Wireframe mountains along the horizon.
      let seed2 = 42;
      const rnd2 = () => ((seed2 = (seed2 * 48271) % 2147483647) / 2147483647);
      [[0.22, 0.55], [0.14, 0.9]].forEach(([height, alpha], layer) => {
        const pts = [[0, hz]];
        for (let x = 0; x <= W + 40; x += 34 + rnd2() * 40) {
          const center = Math.abs(x - cx) / (W * 0.5);
          const h = (layer ? 0.4 : 1) * H * height * (0.25 + 0.75 * rnd2()) * clamp(center * 1.4, 0.15, 1);
          pts.push([x, hz - h]);
        }
        pts.push([W, hz]);
        c.beginPath(); pts.forEach(([x, y], i) => (i ? c.lineTo(x, y) : c.moveTo(x, y))); c.closePath();
        c.fillStyle = P.floor; c.globalAlpha = 0.92; c.fill(); c.globalAlpha = 1;
        c.strokeStyle = P.mountain; c.lineWidth = 1; c.globalAlpha = alpha * (P.dark ? 0.8 : 0.5);
        if (P.dark) { c.shadowColor = P.mountain; c.shadowBlur = 8; }
        c.stroke(); c.shadowBlur = 0; c.globalAlpha = 1;
      });

      const floor = c.createLinearGradient(0, hz, 0, H);
      floor.addColorStop(0, P.floor); floor.addColorStop(1, P.dark ? '#0b0812' : P['panel-2']);
      c.fillStyle = floor; c.fillRect(0, hz, W, H - hz);

      const glow = c.createLinearGradient(0, hz - 2, 0, hz + 6);
      glow.addColorStop(0, 'transparent'); glow.addColorStop(0.5, P.horizon); glow.addColorStop(1, 'transparent');
      c.fillStyle = glow; c.fillRect(0, hz - 2, W, 8);
      this.bg = bg;
    },

    // ── events from the server ─────────────────────────────────────────────
    apply(ev) {
      const s = this.slots.get(ev.s);
      switch (ev.k) {
        case 'b': {
          if (s) this.retire(s, 'exit');
          const ns = new Sprite(this, ev.s, ev.id, ev.n, ev.t, true);
          this.slots.set(ev.s, ns);
          this.hatchOpen = 1;
          break;
        }
        case 'v': if (s) this.react(s, ev); break;
        case 'q':
          if (s) { s.mode = 'queue'; s.queuePos = ev.pos; }
          break;
        case 'a':
          if (s) { s.mode = 'admit'; s.queuePos = 0; this.gateFlash = 1; if (s.id === this.followed) this.float(s, 'admitted', P.green); }
          break;
        case 'd':
          if (s) { s.flawless = !!ev.ok; this.retire(s, s.mode === 'queue' ? 'ghost' : 'exit'); }
          break;
      }
      if (s && s.id === this.followed) inspector.soon();
    },

    retire(s, how) {
      this.slots.delete(s.slot);
      if (how === 'ghost') { s.mode = 'ghost'; this.float(s, 'gave up', P.muted); }
      else if (s.mode !== 'exit' && s.mode !== 'enter') s.mode = 'exit';
      this.leaving.push(s);
    },

    // A visit makes a lemming react: green hop, cyan bounce, yellow stumble,
    // red knockback, purple glitch.
    react(s, ev) {
      const cls = ev.f ? (ev.st >= 500 ? 's5' : ev.st === 0 ? 's0' : 's4') : statusClass(ev.st);
      const label = (ev.st || '×') + (ev.c > 1 ? ' ×' + ev.c : '');
      const followed = s.id === this.followed;
      const fx = fxOn();
      switch (cls) {
        case 's2':
          // Successes are the common case: a hop and a sparkle, no label,
          // so the failures stand out. The followed lemming narrates.
          s.vh = -130;
          if (followed) this.float(s, label + ' ' + ev.p, P.green);
          if (fx) this.burst(s.x, s.y - 12 * this.unit, P.green, 3, 45);
          break;
        case 's3':
          s.vh = -170; s.dir *= Math.random() < 0.3 ? -1 : 1;
          if (followed) this.float(s, '↪ ' + label + ' ' + ev.p, P.cyan);
          break;
        case 's4':
          s.stumble = 0.6; this.float(s, '? ' + label, P.yellow);
          break;
        case 's5':
          s.daze = 1.3; s.vh = -150; s.x -= s.dir * 10 * this.unit;
          this.float(s, label, P.red, true);
          if (fx) this.burst(s.x, s.y - 14 * this.unit, P.red, 16, 140);
          break;
        default:
          s.glitch = 0.9; this.float(s, '× no response', P.purple);
      }
      s.flash = 0.35; s.flashColor = { s2: P.green, s3: P.cyan, s4: P.yellow, s5: P.red, s0: P.purple }[cls];
      if (followed) inspector.soon();
    },

    // Re-sync with the server's view of who is on stage.
    reconcile(stage) {
      const seen = new Set();
      for (const e of stage) {
        seen.add(e.s);
        let s = this.slots.get(e.s);
        if (!s || s.id !== e.id) {
          if (s) this.retire(s, 'exit');
          s = new Sprite(this, e.s, e.id, e.n, e.t, false);
          this.slots.set(e.s, s);
        }
        if (e.state === 'queued') { s.mode = 'queue'; s.queuePos = e.pos || 0; }
        else if (s.mode === 'queue') s.mode = 'admit';
      }
      for (const [slot, s] of this.slots) if (!seen.has(slot)) this.retire(s, 'exit');
    },

    float(s, text, color, big) {
      if (this.floaters.length > 140) this.floaters.shift();
      this.floaters.push({ x: s.x, y: s.y - 30 * this.unit * this.depth(s.y), text, color, t: 0, life: big ? 1.6 : 1.1, big });
    },

    burst(x, y, color, n, speed) {
      for (let i = 0; i < n && this.particles.length < 700; i++) {
        const a = rand(0, Math.PI * 2), v = rand(0.3, 1) * speed;
        this.particles.push({ x, y, vx: Math.cos(a) * v, vy: Math.sin(a) * v - speed * 0.4, color, t: 0, life: rand(0.5, 1), size: rand(1.5, 3) });
      }
    },

    // ── simulation ─────────────────────────────────────────────────────────
    update(dt) {
      const u = this.unit;
      if (fxOn()) this.gridPhase = (this.gridPhase + dt * 0.55) % 1;
      this.hatchOpen = Math.max(0, this.hatchOpen - dt * 1.6);
      this.gateFlash = Math.max(0, this.gateFlash - dt * 2);

      // Queue order follows the server's queue positions.
      const queued = [...this.slots.values()].filter((s) => s.mode === 'queue').sort((a, b) => a.queuePos - b.queuePos);
      this.gateGlow = clamp(this.gateGlow + (queued.length ? dt * 2 : -dt), 0, 1);
      queued.forEach((s, i) => { s.qx = this.gateX - 22 * u - i * 11 * u; s.qy = this.queueY + (i % 2) * 14 * u; s.qrank = i; });

      for (const s of this.all()) {
        s.flash = Math.max(0, s.flash - dt);
        s.glitch = Math.max(0, s.glitch - dt);
        if (s.vh || s.hop < 0) { s.vh += 900 * dt; s.hop += s.vh * dt; if (s.hop >= 0) { s.hop = 0; s.vh = 0; } }
        const speed = s.speed * u * this.depth(s.y);
        switch (s.mode) {
          case 'fall':
            s.vy += 700 * dt; s.y += s.vy * dt;
            if (s.y >= s.lane) { s.y = s.lane; s.mode = 'walk'; if (fxOn()) this.burst(s.x, s.y, P.muted, 4, 30); }
            break;
          case 'walk':
            if (s.daze > 0) { s.daze -= dt; break; }
            if (s.stumble > 0) { s.stumble -= dt; break; }
            s.x += s.dir * speed * dt; s.walked += speed * dt;
            if (s.x < this.xMin) { s.x = this.xMin; s.dir = 1; }
            if (s.x > this.xMax) { s.x = this.xMax; s.dir = -1; }
            if (Math.random() < dt * 0.08) s.dir *= -1;
            s.y += (s.lane - s.y) * Math.min(1, dt * 2);
            break;
          case 'queue':
          case 'admit':
          case 'exit': {
            let tx, ty;
            if (s.mode === 'queue') { tx = s.qx ?? this.gateX; ty = s.qy ?? this.queueY; }
            else if (s.mode === 'admit') { tx = this.gateX + 36 * u; ty = s.lane; }
            else { tx = this.exitX; ty = this.exitY; }
            const dx = tx - s.x, dy = ty - s.y, dist = Math.hypot(dx, dy);
            const step = Math.max(speed, 40 * u) * (s.mode === 'exit' ? 1.6 : 1.2) * dt;
            if (dist > step) {
              s.x += (dx / dist) * step; s.y += (dy / dist) * step; s.walked += step;
              s.dir = dx >= 0 ? 1 : -1;
            } else {
              s.x = tx; s.y = ty;
              if (s.mode === 'admit') { s.mode = 'walk'; s.dir = 1; }
              else if (s.mode === 'exit') { s.mode = 'enter'; s.t = 0; this.exitGlow = 1; }
            }
            break;
          }
          case 'enter':
            s.t += dt; s.alpha = 1 - s.t / 0.55; s.shrink = 1 - s.t * 0.6;
            if (s.t >= 0.55) {
              s.done = true;
              if (s.flawless) { this.float(s, 'saved', P.green); if (fxOn()) this.burst(this.exitX, this.exitY - 20 * u, P.green, 12, 70); }
            }
            break;
          case 'ghost':
            s.t = (s.t || 0) + dt; s.y -= 28 * u * dt; s.x += Math.sin(s.t * 5) * 12 * dt; s.alpha = 1 - s.t / 1.8;
            if (s.t >= 1.8) s.done = true;
            break;
        }
      }
      this.exitGlow = Math.max(0, this.exitGlow - dt * 1.5);
      this.leaving = this.leaving.filter((s) => !s.done);

      for (const p of this.particles) { p.t += dt; p.vy += 260 * dt; p.x += p.vx * dt; p.y += p.vy * dt; }
      this.particles = this.particles.filter((p) => p.t < p.life);
      for (const f of this.floaters) f.t += dt;
      this.floaters = this.floaters.filter((f) => f.t < f.life);
    },

    // ── drawing ────────────────────────────────────────────────────────────
    draw() {
      const c = this.ctx, W = this.W, H = this.H;
      if (!W || !this.bg) return;
      c.setTransform(1, 0, 0, 1, 0, 0);
      c.drawImage(this.bg, 0, 0);
      c.setTransform(this.dpr, 0, 0, this.dpr, 0, 0);
      this.drawGrid(c, W, H);
      this.drawHatch(c);
      this.drawGate(c);
      this.drawExit(c);

      const sprites = [...this.all()].sort((a, b) => a.y - b.y);
      for (const s of sprites) this.drawSprite(c, s);

      for (const p of this.particles) {
        c.globalAlpha = 1 - p.t / p.life; c.fillStyle = p.color;
        c.fillRect(p.x, p.y, p.size * this.unit, p.size * this.unit);
      }
      c.globalAlpha = 1;
      c.textAlign = 'center';
      for (const f of this.floaters) {
        const k = f.t / f.life;
        c.globalAlpha = k < 0.7 ? 1 : 1 - (k - 0.7) / 0.3;
        c.font = `700 ${Math.round((f.big ? 14 : 11) * this.unit)}px ui-monospace, Menlo, monospace`;
        c.fillStyle = f.color;
        if (P.dark) { c.shadowColor = f.color; c.shadowBlur = 6; }
        c.fillText(f.text, f.x, f.y - k * 26 * this.unit);
        c.shadowBlur = 0;
      }
      c.globalAlpha = 1;
      if (this.hover) this.drawTag(c, this.hover, P.text);
      const followed = this.followed && [...this.all()].find((s) => s.id === this.followed);
      if (followed) this.drawTag(c, followed, P.yellow);
    },

    drawGrid(c, W, H) {
      const hz = this.horizon, fh = H - hz, cx = W / 2;
      c.save();
      c.strokeStyle = P.grid; c.lineWidth = 1;
      if (P.dark) { c.shadowColor = P.grid; c.shadowBlur = 4; }
      const lines = 14;
      for (let k = 0; k < lines; k++) {
        const d = (k + this.gridPhase) / lines;
        const y = hz + fh * Math.pow(d, 2.2);
        c.globalAlpha = 0.12 + 0.55 * d;
        c.beginPath(); c.moveTo(0, y); c.lineTo(W, y); c.stroke();
      }
      const spread = W / 14;
      for (let i = -16; i <= 16; i++) {
        c.globalAlpha = 0.35;
        c.beginPath(); c.moveTo(cx + i * spread * 0.1, hz); c.lineTo(cx + i * spread * 1.6, H); c.stroke();
      }
      c.restore();
    },

    drawHatch(c) {
      const u = this.unit, x = this.hatchX, y = this.hatchY, w = 46 * u, h = 16 * u;
      c.save();
      c.strokeStyle = P.pink; c.lineWidth = 2;
      if (P.dark) { c.shadowColor = P.magenta; c.shadowBlur = 10; }
      c.fillStyle = P.dark ? 'rgba(20,12,30,.85)' : 'rgba(255,255,255,.8)';
      c.fillRect(x - w / 2, y - h - 4 * u, w, h); c.strokeRect(x - w / 2, y - h - 4 * u, w, h);
      // Trapdoor flaps swing open when a lemming is born.
      const open = this.hatchOpen, fw = (w / 2) * (1 - open * 0.8);
      c.fillStyle = P.pink; c.globalAlpha = 0.9;
      c.fillRect(x - w / 2, y - 4 * u, fw, 3 * u); c.fillRect(x + w / 2 - fw, y - 4 * u, fw, 3 * u);
      c.globalAlpha = 1; c.shadowBlur = 0;
      c.font = `700 ${Math.round(9 * u)}px ui-monospace, Menlo, monospace`; c.textAlign = 'center';
      c.fillStyle = P.dark ? '#fff' : P.pink; c.fillText('ENTRANCE', x, y - h - 8 * u);
      c.restore();
    },

    drawExit(c) {
      const u = this.unit, x = this.exitX, y = this.exitY, w = 30 * u, h = 44 * u, t = performance.now() / 1000;
      c.save();
      const glow = 0.6 + 0.4 * Math.sin(t * 3) * 0.3 + this.exitGlow * 0.5;
      const portal = c.createLinearGradient(0, y - h, 0, y);
      portal.addColorStop(0, P.yellow); portal.addColorStop(0.5, P.orange); portal.addColorStop(1, P.magenta);
      c.globalAlpha = 0.35 + glow * 0.4; c.fillStyle = portal;
      c.beginPath(); c.moveTo(x - w / 2, y); c.lineTo(x - w / 2, y - h + w / 2);
      c.arc(x, y - h + w / 2, w / 2, Math.PI, 0); c.lineTo(x + w / 2, y); c.closePath(); c.fill();
      c.globalAlpha = 1; c.strokeStyle = P.yellow; c.lineWidth = 2;
      if (P.dark) { c.shadowColor = P.orange; c.shadowBlur = 14 * glow; }
      c.stroke(); c.shadowBlur = 0;
      // Neon torches either side, as the game's exits had.
      [-1, 1].forEach((side) => {
        const tx = x + side * (w / 2 + 7 * u), ty = y - h * 0.62, f = 4 * u + Math.sin(t * 13 + side) * 1.5 * u;
        c.fillStyle = P.orange; c.beginPath(); c.moveTo(tx - 3 * u, ty); c.lineTo(tx, ty - f * 2); c.lineTo(tx + 3 * u, ty); c.fill();
        c.fillStyle = P.muted; c.fillRect(tx - 1 * u, ty, 2 * u, 10 * u);
      });
      c.font = `700 ${Math.round(9 * u)}px ui-monospace, Menlo, monospace`; c.textAlign = 'center';
      c.fillStyle = P.dark ? '#fff' : P.magenta; c.fillText('EXIT', x, y - h - 6 * u);
      c.restore();
    },

    drawGate(c) {
      if (this.gateGlow <= 0.01) return;
      const u = this.unit, x = this.gateX, y = this.queueY + 8 * u, h = 40 * u;
      c.save();
      c.globalAlpha = this.gateGlow;
      c.strokeStyle = this.gateFlash > 0 ? P.green : P.orange; c.lineWidth = 3;
      if (P.dark) { c.shadowColor = c.strokeStyle; c.shadowBlur = 10 + this.gateFlash * 20; }
      c.beginPath(); c.moveTo(x, y); c.lineTo(x, y - h); c.lineTo(x + 18 * u, y - h); c.lineTo(x + 18 * u, y); c.stroke();
      c.beginPath(); c.moveTo(x, y - h * 0.5); c.lineTo(x + 18 * u, y - h * (0.5 + 0.15 * Math.sin(performance.now() / 300))); c.stroke();
      c.shadowBlur = 0;
      c.font = `700 ${Math.round(9 * u)}px ui-monospace, Menlo, monospace`; c.textAlign = 'center';
      c.fillStyle = P.orange; c.fillText('WAITING ROOM', x + 9 * u, y - h - 6 * u);
      c.restore();
    },

    drawSprite(c, s) {
      const d = this.depth(s.y) * this.unit * (s.shrink || 1), px = 2.4 * d;
      const x = s.x, y = s.y + s.hop * d;
      c.save();
      c.globalAlpha = clamp(s.alpha, 0, 1) * (s.mode === 'ghost' ? 0.55 : 1);

      if (s.mode !== 'fall' && s.mode !== 'ghost') {
        c.fillStyle = 'rgba(0,0,0,.28)';
        c.beginPath(); c.ellipse(x, s.y + 1, 7 * d, 2.2 * d, 0, 0, Math.PI * 2); c.fill();
      }
      if (s.id === this.followed) {
        c.strokeStyle = P.yellow; c.lineWidth = 2;
        if (P.dark) { c.shadowColor = P.yellow; c.shadowBlur = 10; }
        c.beginPath(); c.ellipse(x, s.y + 1, 12 * d, 4 * d, 0, 0, Math.PI * 2); c.stroke();
        c.shadowBlur = 0;
      }

      const moving = s.mode === 'walk' || s.mode === 'exit' || s.mode === 'admit' || (s.mode === 'queue' && Math.hypot((s.qx ?? s.x) - s.x, (s.qy ?? s.y) - s.y) > 1);
      const legs = s.mode === 'fall' ? SPRITE.legs[2] : moving && s.daze <= 0 && s.stumble <= 0
        ? SPRITE.legs[Math.floor(s.walked / (5 * d)) % 2] : SPRITE.legs[2];
      const rows = SPRITE.top.concat(legs);
      const colors = { H: P.hair, S: P.skin, B: s.flash > 0 ? s.flashColor : P.robe, L: P.shoe };
      if (s.mode === 'ghost') { colors.H = colors.S = colors.B = colors.L = P.muted; }
      const ox = x - 4 * px, oy = y - rows.length * px;
      const draw = (dx, tint) => {
        for (let r = 0; r < rows.length; r++) {
          const row = rows[r];
          for (let i = 0; i < 8; i++) {
            const ch = row[s.dir > 0 ? i : 7 - i];
            if (ch === '.') continue;
            c.fillStyle = tint || colors[ch];
            c.fillRect(Math.round(ox + i * px + dx), Math.round(oy + r * px), Math.ceil(px), Math.ceil(px));
          }
        }
      };
      if (s.glitch > 0) {
        const j = rand(-3, 3) * d;
        c.globalAlpha *= 0.7; draw(j - 2 * d, P.cyan); draw(j + 2 * d, P.magenta); c.globalAlpha /= 0.7;
        draw(j);
      } else {
        draw(0);
      }

      if (s.daze > 0) {
        const t = performance.now() / 180;
        c.fillStyle = P.yellow;
        for (let i = 0; i < 3; i++) {
          const a = t + (i * Math.PI * 2) / 3;
          c.fillRect(x + Math.cos(a) * 7 * d - d, oy - 4 * d + Math.sin(a) * 2 * d, 2 * d, 2 * d);
        }
      }
      if (s.mode === 'queue' && s.queuePos && (s.qrank < 5 || s.id === this.followed)) {
        const label = '#' + fmtInt(s.queuePos);
        c.font = `700 ${Math.round(9 * this.unit)}px ui-monospace, Menlo, monospace`;
        const tw = c.measureText(label).width + 8;
        c.fillStyle = P.orange; c.globalAlpha = 0.9;
        c.fillRect(x - tw / 2, oy - 16 * d, tw, 12 * this.unit);
        c.globalAlpha = 1; c.fillStyle = '#1d1727'; c.textAlign = 'center';
        c.fillText(label, x, oy - 16 * d + 9 * this.unit);
      }
      c.restore();
    },

    drawTag(c, s, color) {
      const d = this.depth(s.y) * this.unit, top = s.y + s.hop * d - 10 * 2.4 * d - 10 * this.unit;
      c.save();
      c.font = `600 ${Math.round(11 * this.unit)}px -apple-system, "Segoe UI", sans-serif`;
      c.textAlign = 'center';
      const w = c.measureText(s.name).width + 12;
      c.fillStyle = P.dark ? 'rgba(20,14,32,.85)' : 'rgba(255,255,255,.92)';
      c.fillRect(s.x - w / 2, top - 14 * this.unit, w, 17 * this.unit);
      c.strokeStyle = color; c.lineWidth = 1; c.strokeRect(s.x - w / 2, top - 14 * this.unit, w, 17 * this.unit);
      c.fillStyle = color; c.fillText(s.name, s.x, top - 1.5 * this.unit);
      c.restore();
    },

    onPointer(e, click) {
      const r = this.canvas.getBoundingClientRect();
      const x = e.clientX - r.left, y = e.clientY - r.top;
      let best = null, bestD = 22 * this.unit;
      for (const s of this.slots.values()) {
        const d = this.depth(s.y) * this.unit;
        const dist = Math.hypot(s.x - x, s.y - 12 * d - y);
        if (dist < bestD) { best = s; bestD = dist; }
      }
      this.hover = best;
      this.canvas.style.cursor = best ? 'pointer' : 'crosshair';
      if (click && best) inspector.follow(best.id);
    },

    randomSprite() {
      const all = [...this.slots.values()].filter((s) => s.mode !== 'fall');
      return all.length ? all[Math.floor(Math.random() * all.length)] : null;
    },
  };

  // ── lemming inspector ────────────────────────────────────────────────────
  const inspector = {
    id: null, timer: 0, pending: 0, life: null, shown: new Set(),

    follow(id) {
      this.id = id; scene.followed = id; this.life = null; this.shown = new Set();
      setText('inspector-hint', 'following');
      clearInterval(this.timer);
      this.timer = setInterval(() => this.refresh(), 1000);
      this.refresh();
    },
    release() {
      this.id = null; scene.followed = null; this.life = null;
      clearInterval(this.timer);
      setText('inspector-hint', 'click a lemming');
      this.renderEmpty('Click any lemming on the terrain to follow its life.');
    },
    followRandom() {
      const s = scene.randomSprite();
      if (s) this.follow(s.id);
      else this.renderEmpty('No lemmings are on the terrain right now.');
    },
    soon() {
      if (this.pending) return;
      this.pending = setTimeout(() => { this.pending = 0; this.refresh(); }, 250);
    },
    async refresh() {
      const id = this.id;
      if (!id) return;
      try {
        const r = await fetch('/api/lemming?id=' + encodeURIComponent(id), { cache: 'no-store' });
        if (id !== this.id) return;
        if (r.status === 404) { this.renderEmpty('That lemming is no longer tracked — its life ended a while ago.'); clearInterval(this.timer); return; }
        if (!r.ok) return;
        this.life = await r.json();
        this.render(this.life);
        if (this.life.state === 'home' || this.life.state === 'gone') clearInterval(this.timer);
      } catch (e) { /* the next tick retries */ }
    },
    renderEmpty(message) {
      const body = $('inspector-body');
      body.replaceChildren();
      const empty = el('div', 'empty');
      const pix = document.querySelector('.pix');
      if (pix) empty.append(pix.cloneNode(true));
      empty.append(el('p', null, message));
      const b = el('button', 'btn', 'follow a random lemming');
      b.type = 'button'; b.dataset.action = 'follow-random';
      empty.append(b);
      body.append(empty);
    },
    render(l) {
      const body = $('inspector-body');
      const frag = document.createDocumentFragment();

      const who = el('div', 'who');
      const names = el('div');
      names.append(el('h3', null, l.name), el('div', 'id', '#' + l.id.slice(0, 8) + ' · terrain ' + l.terrain + ', pack ' + l.pack));
      const stateLabel = { walking: 'walking', queued: 'queued #' + fmtInt(l.queue_pos), home: 'home safe', gone: 'gone' }[l.state] || l.state;
      who.append(names, el('span', 'state ' + l.state, stateLabel));
      frag.append(who);

      const badges = el('div', 'badges');
      [l.persona, l.language && l.language.split(',')[0]].filter(Boolean).forEach((b) => badges.append(el('span', 'badge', b)));
      frag.append(badges);

      const end = l.died || serverNow();
      const vitals = el('div', 'vitals');
      [[fmtClock(end - l.born), 'age'], [fmtInt(l.visits), 'pages'], [fmtInt(l.failed), 'failed'],
        [l.history.length ? fmtMs(l.history.reduce((a, v) => a + v.ms, 0) / l.history.length) : '—', 'avg page']]
        .forEach(([v, k]) => { const d = el('div'); d.append(el('b', null, v), el('span', null, k)); vitals.append(d); });
      frag.append(vitals);

      if (l.died) {
        const home = l.state === 'home';
        const why = { lifespan: 'its time ran out', 'page-limit': 'it read enough pages', 'journey-complete': 'it finished the journey', cancelled: 'the run was stopped' }[l.exit] || l.exit || 'it left';
        frag.append(el('div', 'epitaph ' + (home ? 'home' : 'gone'),
          (home ? '✓ Made it home without a single failed page — ' : '✗ Left with ' + fmtInt(l.failed) + ' failed page' + (l.failed === 1 ? '' : 's') + ' — ') + why + '.'));
      }

      const list = el('ol', 'life');
      const hist = l.history.slice().reverse();
      const slowest = Math.max(1, ...hist.map((v) => v.ms));
      for (const v of hist) {
        // Only visits not shown before slide in; the list is rebuilt every
        // refresh and must not re-animate rows the reader has already seen.
        const li = el('li', (v.f ? 'failed' : '') + (this.shown.has(v.n) ? '' : ' fresh'));
        this.shown.add(v.n);
        li.append(el('span', 'n', '#' + v.n));
        li.append(el('span', 'path', (v.step ? v.step + ' · ' : '') + v.p));
        li.append(el('span', 'chip ' + (v.c ? 's0' : statusClass(v.s)), v.c ? 'cut' : String(v.s || '×')));
        if (v.title) li.append(el('span', 'title', v.title));
        const timing = el('span', 'timing');
        const bar = el('span', 'bar');
        const total = el('i'); total.style.width = clamp((v.ms / slowest) * 100, 1, 100) + '%';
        const ttfb = el('i', 'ttfb'); ttfb.style.width = clamp((v.ttfb / slowest) * 100, 0, 100) + '%';
        bar.append(total, ttfb);
        timing.append(bar, el('span', null, fmtMs(v.ms) + (v.q ? ' · queued ' + fmtMs(v.q) : '') + (v.hops ? ' · ' + v.hops + ' redirect' + (v.hops > 1 ? 's' : '') : '')));
        li.append(timing);
        if (v.why) li.append(el('span', 'why', v.why));
        list.append(li);
      }
      if (!hist.length) list.append(el('li', null, 'Waiting for its first page…'));
      if (l.omitted) list.append(el('li', 'muted', fmtInt(l.omitted) + ' earlier pages not shown'));
      frag.append(list);

      const actions = el('div');
      actions.style.marginTop = '10px';
      const next = el('button', 'btn', 'follow another'); next.type = 'button'; next.dataset.action = 'follow-random';
      const release = el('button', 'btn ghost', 'let go'); release.type = 'button'; release.dataset.action = 'release';
      release.style.marginLeft = '6px';
      actions.append(next, release);
      frag.append(actions);

      body.replaceChildren(frag);
    },
  };

  // ── charts ───────────────────────────────────────────────────────────────
  const charts = {
    specs: {},
    tip: null,

    draw(id, spec) {
      this.specs[id] = spec;
      const canvas = $(id);
      const r = canvas.getBoundingClientRect();
      if (!r.width) return;
      const dpr = Math.min(window.devicePixelRatio || 1, 2);
      canvas.width = Math.round(r.width * dpr); canvas.height = Math.round(r.height * dpr);
      const c = canvas.getContext('2d');
      c.setTransform(dpr, 0, 0, dpr, 0, 0);
      const W = r.width, H = r.height, L = 44, R = spec.right ? 40 : 10, T = 10, B = 22;
      const pw = W - L - R, ph = H - T - B, xs = spec.xs;
      c.clearRect(0, 0, W, H);
      c.font = '11px ui-monospace, Menlo, monospace';
      if (xs.length < 2) {
        c.fillStyle = P.muted; c.textAlign = 'center'; c.fillText('collecting…', W / 2, H / 2);
        return;
      }
      const nice = (v) => {
        if (v <= 0) return 1;
        const m = Math.pow(10, Math.floor(Math.log10(v / 4))), r2 = v / 4 / m;
        const step = (r2 <= 1 ? 1 : r2 <= 2 ? 2 : r2 <= 5 ? 5 : 10) * m;
        return Math.ceil(v / step) * step;
      };
      const lmax = nice(Math.max(...spec.series.filter((s) => !s.right).flatMap((s) => s.values), ...(spec.hlines || []).map((h) => h.value)));
      const rmax = spec.right ? nice(Math.max(...spec.series.filter((s) => s.right).flatMap((s) => s.values))) : 1;
      const x0 = xs[0], x1 = xs[xs.length - 1] || 1;
      const X = (t) => L + ((t - x0) / Math.max(x1 - x0, 1e-9)) * pw;
      const Y = (v, right) => T + ph - (v / (right ? rmax : lmax)) * ph;

      c.strokeStyle = P.line; c.fillStyle = P.muted; c.lineWidth = 1;
      for (let i = 0; i <= 4; i++) {
        const v = (lmax / 4) * i, y = Y(v);
        c.beginPath(); c.moveTo(L, y); c.lineTo(W - R, y); c.stroke();
        c.textAlign = 'right'; c.fillText(spec.fmt(v), L - 6, y + 4);
        if (spec.right) { c.textAlign = 'left'; c.fillText(spec.rfmt((rmax / 4) * i), W - R + 6, y + 4); }
      }
      c.textAlign = 'center';
      for (let i = 0; i <= 4; i++) {
        const t = x0 + ((x1 - x0) / 4) * i;
        c.fillText(fmtClock(t), X(t), H - 6);
      }

      const barW = Math.max(pw / xs.length - 1, 1);
      for (const s of spec.series) {
        c.save();
        c.strokeStyle = s.color; c.fillStyle = s.color; c.lineWidth = 2; c.lineJoin = 'round';
        if (s.kind === 'bars') {
          c.globalAlpha = 0.85;
          s.values.forEach((v, i) => { if (v > 0) c.fillRect(X(xs[i]) - barW / 2, Y(v, s.right), barW, T + ph - Y(v, s.right)); });
        } else {
          c.beginPath();
          s.values.forEach((v, i) => (i ? c.lineTo(X(xs[i]), Y(v, s.right)) : c.moveTo(X(xs[i]), Y(v, s.right))));
          if (s.kind === 'dash') c.setLineDash([6, 5]);
          if (P.dark && s.kind !== 'dash') { c.shadowColor = s.color; c.shadowBlur = 6; }
          c.stroke();
          if (s.kind === 'area') {
            c.shadowBlur = 0; c.lineTo(X(xs[xs.length - 1]), T + ph); c.lineTo(X(xs[0]), T + ph); c.closePath();
            const g = c.createLinearGradient(0, T, 0, T + ph);
            g.addColorStop(0, s.color); g.addColorStop(1, 'transparent');
            c.globalAlpha = 0.22; c.fillStyle = g; c.fill();
          }
        }
        c.restore();
      }
      for (const h of spec.hlines || []) {
        c.save(); c.strokeStyle = h.color; c.setLineDash([4, 4]); c.lineWidth = 1.5;
        c.beginPath(); c.moveTo(L, Y(h.value)); c.lineTo(W - R, Y(h.value)); c.stroke(); c.restore();
      }
      if (spec.hover != null) {
        const i = spec.hover, x = X(xs[i]);
        c.save(); c.strokeStyle = P['line-strong']; c.setLineDash([3, 3]);
        c.beginPath(); c.moveTo(x, T); c.lineTo(x, T + ph); c.stroke(); c.restore();
        for (const s of spec.series) {
          c.fillStyle = s.color; c.beginPath(); c.arc(x, Y(s.values[i], s.right), 3.5, 0, Math.PI * 2); c.fill();
        }
      }
      spec.geom = { L, pw, x0, x1 };
    },

    redraw() { Object.keys(this.specs).forEach((id) => this.draw(id, this.specs[id])); },

    hover(id, e) {
      const spec = this.specs[id];
      if (!spec || !spec.geom || spec.xs.length < 2) return;
      const r = $(id).getBoundingClientRect(), g = spec.geom;
      const t = g.x0 + ((e.clientX - r.left - g.L) / g.pw) * (g.x1 - g.x0);
      let best = 0;
      spec.xs.forEach((x, i) => { if (Math.abs(x - t) < Math.abs(spec.xs[best] - t)) best = i; });
      spec.hover = best;
      this.draw(id, spec);
      if (!this.tip) { this.tip = el('div', 'tip'); }
      const wrap = $(id).parentElement;
      if (this.tip.parentElement !== wrap) wrap.append(this.tip);
      this.tip.replaceChildren(el('b', null, 'at ' + fmtClock(spec.xs[best])));
      for (const s of spec.series) {
        const row = el('span'); const sw = el('i'); sw.style.background = s.color;
        row.append(sw, document.createTextNode(s.label + ': ' + s.tip(s.values[best])));
        this.tip.append(row);
      }
      const x = e.clientX - r.left;
      this.tip.style.left = (x > r.width - 170 ? x - 160 : x + 12) + 'px';
      this.tip.style.top = '8px';
    },

    leave(id) {
      const spec = this.specs[id];
      if (spec) { spec.hover = null; this.draw(id, spec); }
      if (this.tip) this.tip.remove();
    },

    update(live) {
      const tl = (live && live.timeline) || [];
      const xs = tl.map((p) => p.t);
      const width = tl.length > 1 ? tl[1].t - tl[0].t : 1;
      this.draw('chart-traffic', {
        xs, right: true, fmt: (v) => fmtInt(v), rfmt: (v) => fmtInt(v),
        series: [
          { label: 'visits/s', values: tl.map((p) => p.rps), color: P.cyan, kind: 'area', tip: (v) => v.toFixed(1) },
          { label: 'failures/s', values: tl.map((p) => p.failed / width), color: P.red, kind: 'bars', tip: (v) => v.toFixed(1) },
          { label: 'alive', values: tl.map((p) => p.alive), color: P.yellow, kind: 'dash', right: true, tip: (v) => fmtInt(v) },
        ],
      });
      const budget = CFG.p95_gate > 0 ? [{ value: CFG.p95_gate * 1000, color: P.red }] : [];
      $('budget-legend').hidden = !budget.length;
      this.draw('chart-latency', {
        xs, fmt: (v) => (v >= 1000 ? (v / 1000).toFixed(1) + 's' : Math.round(v) + 'ms'), hlines: budget,
        series: [
          { label: 'p95', values: tl.map((p) => p.p95_ms), color: P.pink, kind: 'line', tip: fmtMs },
          { label: 'p50', values: tl.map((p) => p.p50_ms), color: P.cyan, kind: 'line', tip: fmtMs },
        ],
      });
    },
  };
  ['chart-traffic', 'chart-latency'].forEach((id) => {
    $(id).addEventListener('pointermove', (e) => charts.hover(id, e));
    $(id).addEventListener('pointerleave', () => charts.leave(id));
  });

  // ── the feed ─────────────────────────────────────────────────────────────
  const GLYPH = { fail: '✗', queue: '⧗', admit: '→', home: '✓', terrain: '▦', swarm: '★', drop: '!', unborn: '∅' };
  function feedLine(item) {
    const li = el('li', item.k);
    if (item.id) li.dataset.id = item.id;
    li.append(el('span', 't', fmtClock(item.t)), el('span', 'g', GLYPH[item.k] || '·'));
    const text = el('span');
    const who = () => el('b', null, item.n || 'a lemming');
    switch (item.k) {
      case 'fail':
        text.append(who(), ' hit ', el('code', null, item.p || '/'), ' — ' + (item.st || 'no response') + (item.x ? ' · ' + item.x : '') + ' · ' + fmtMs(item.ms));
        break;
      case 'queue': text.append(who(), ' queued at #' + (item.x || '?') + ' for ', el('code', null, item.p || '/')); break;
      case 'admit': text.append(who(), ' admitted after ' + fmtMs(item.ms)); break;
      case 'home': text.append(who(), ' went home safe after ' + (item.x || '?') + ' pages'); break;
      case 'terrain': text.append('terrain ' + item.tr + ' is ' + item.x); break;
      case 'unborn': text.append('a lemming in terrain ' + item.tr + ' never started' + (item.x ? ': ' + item.x : '')); break;
      default: text.append(item.x || item.k);
    }
    li.append(text);
    return li;
  }
  function addFeed(items) {
    const list = $('feed');
    for (const item of items) {
      if (item.seq && item.seq <= state.lastFeedSeq) continue;
      if (item.seq) state.lastFeedSeq = item.seq;
      list.prepend(feedLine(item));
    }
    while (list.children.length > 80) list.lastChild.remove();
  }
  $('feed').addEventListener('click', (e) => {
    const li = e.target.closest('li[data-id]');
    if (li) inspector.follow(li.dataset.id);
  });

  // ── stats (once a second) ────────────────────────────────────────────────
  function setPhase(phase) {
    if (!phase || phase === state.phase) return;
    state.phase = phase;
    const e = $('phase');
    e.dataset.phase = phase; e.textContent = phase;
    if (phase === 'done') showFinale();
  }

  function applyStats(s) {
    state.stats = s;
    const m = s.m, live = s.live;
    setPhase(s.phase);
    setText('elapsed', fmtClock(m.elapsed_secs));
    setText('eta', CFG.eta ? 'of ~' + fmtClock(CFG.eta) : '');
    setText('alive', fmtInt(m.alive));
    setText('alive-sub', CFG.total ? fmtInt(m.completed) + ' of ' + fmtInt(CFG.total) + ' lived' : '');
    setText('completed', fmtInt(m.completed));
    setText('failed', fmtInt(m.failed));
    setText('visits', fmtInt(m.total_visits));
    setText('bytes', m.total_bytes);
    setText('terrains', fmtInt(m.terrains_online) + ' terrain' + (m.terrains_online === 1 ? '' : 's') + ' online');
    ['XX2', 'XX3', 'XX4', 'XX5'].forEach((k) => setText(k, fmtInt(m[k])));
    setText('wr', fmtInt(m.waiting_room));
    document.title = (m.alive ? fmtInt(m.alive) + ' alive · ' : '') + 'lemmings — ' + host;
    setText('scene-count', scene.slots.size + ' on the terrain' + (m.alive > scene.slots.size ? ' of ' + fmtInt(m.alive) + ' alive' : ''));

    const prev = state.lastVisits;
    state.lastVisits = m.total_visits;
    if (prev !== undefined) setText('rate', fmtInt(m.total_visits - prev) + ' / s');

    if (live) {
      const rate = live.failure_rate * 100;
      setText('failrate', rate.toFixed(rate < 10 ? 2 : 1) + '%');
      setText('failed-visits', fmtInt(live.failed) + ' visits' + (live.cancelled ? ' · ' + fmtInt(live.cancelled) + ' cut' : ''));
      $('failrate-kpi').className = 'kpi ' + (live.failed ? (CFG.fail_gate >= 0 && live.failure_rate > CFG.fail_gate ? 'red' : 'yellow') : 'green');
      setText('p95', fmtNs(live.p95_ns));
      setText('p50', 'p50 ' + fmtNs(live.p50_ns) + ' · p99 ' + fmtNs(live.p99_ns));
      setText('rescued', live.sessions ? live.rescued_percent.toFixed(1) + '%' : '—');
      setText('rescued-sub', live.sessions ? fmtInt(live.rescued) + ' of ' + fmtInt(live.sessions) + ' flawless' : 'flawless lives');
      setText('wr-sub', live.room.held ? 'p95 wait ' + fmtNs(live.room.p95_wait_ns) + (live.room.died_in_line ? ' · ' + fmtInt(live.room.died_in_line) + ' gave up' : '') : 'never queued');
      charts.update(live);
      renderPaths(live.paths || []);
      renderCauses(live.causes || []);
    }
    renderTerrains(s.terrains || []);

    if (m.dropped_logs > 0) showBanner('⚠ ' + fmtInt(m.dropped_logs) + ' life records were dropped because the collector fell behind. Visit totals remain exact.', false);
  }

  function renderPaths(paths) {
    const body = $('paths');
    if (!paths.length) return;
    const rows = paths.map((p) => {
      const tr = el('tr');
      const url = el('td', 'url'); url.append(document.createTextNode(p.url));
      const bar = el('div', 'bar'); bar.style.width = clamp(p.hit_share * 100, 1, 100) + '%'; url.append(bar);
      tr.append(url, el('td', 'num', fmtInt(p.hits)), el('td', 'num', fmtNs(p.p50_ns)), el('td', 'num', fmtNs(p.p95_ns)),
        el('td', 'num' + (p.failed ? ' bad' : ''), fmtInt(p.failed)));
      return tr;
    });
    body.replaceChildren(...rows);
  }

  function renderCauses(causes) {
    const list = $('causes');
    if (!causes.length) {
      const li = el('li');
      li.append(el('span', 'none', 'nothing — every visit passed'));
      list.replaceChildren(li);
      return;
    }
    list.replaceChildren(...causes.slice(0, 8).map((c) => {
      const li = el('li');
      li.append(el('code', null, c.label), el('span', null, fmtInt(c.count)));
      const bar = el('span', 'bar'); bar.style.width = clamp(c.share * 100, 2, 100) + '%';
      li.append(bar);
      return li;
    }));
  }

  function renderTerrains(tiles) {
    const map = $('terrain-map');
    if (!tiles.length) return;
    if (map.children.length !== tiles.length) {
      map.replaceChildren(...tiles.map(() => el('div', 'tile')));
      setText('map-note', tiles.length < CFG.terrain ? fmtInt(CFG.terrain) + ' terrains in ' + tiles.length + ' tiles' : fmtInt(CFG.terrain) + ' terrains');
    }
    const perTile = Math.max(1, (CFG.pack || 1) * (CFG.terrain || 1) / tiles.length);
    tiles.forEach((t, i) => {
      const d = map.children[i];
      const cls = 'tile' + (t.s === 1 ? ' on' : t.s === 2 ? ' done' : '') + (t.f ? ' hurt' : '');
      if (d.className !== cls) d.className = cls;
      d.style.setProperty('--heat', clamp(t.a / perTile, 0.08, 1).toFixed(2));
      d.style.setProperty('--pain', clamp(t.v ? t.f / t.v * 5 : 0, 0.1, 1).toFixed(2));
      d.title = (t.from === t.to ? 'terrain ' + t.from : 'terrains ' + t.from + '–' + t.to) + ' · ' +
        ['waiting', 'online', 'done'][t.s] + ' · ' + fmtInt(t.a) + ' alive · ' + fmtInt(t.v) + ' visits · ' + fmtInt(t.f) + ' failed';
    });
  }

  function showBanner(text, bad) {
    const b = $('banner');
    b.textContent = text; b.className = 'banner' + (bad ? ' bad' : ''); b.hidden = false;
  }
  const hideBanner = () => { $('banner').hidden = true; };

  function showFinale() {
    if (state.finaleShown) return;
    state.finaleShown = true;
    setTimeout(() => {
      const live = state.stats && state.stats.live;
      if (!live) return;
      const pct = live.sessions ? live.rescued_percent : 0;
      const score = $('finale-score');
      score.textContent = pct.toFixed(1) + '%';
      score.className = 'score ' + (pct >= 99.95 ? 'good' : pct >= 50 ? 'meh' : 'bad');
      setText('finale-title', !live.sessions ? 'No lemmings made it home to report.'
        : live.rescued === live.sessions ? 'All lemmings accounted for.'
          : pct >= 90 ? 'So close. A few lemmings hit trouble.' : pct >= 50 ? 'Oh no! Many lemmings hit trouble.' : 'Oh no! Most lemmings hit trouble.');
      setText('finale-caption', 'rescued — ' + fmtInt(live.rescued) + ' of ' + fmtInt(live.sessions) + ' lived without a failed page');
      setText('finale-visits', fmtInt(live.visits));
      setText('finale-failed', (live.failure_rate * 100).toFixed(2) + '%');
      setText('finale-p95', fmtNs(live.p95_ns));
      const gates = $('finale-gates');
      gates.replaceChildren();
      if (CFG.fail_gate >= 0) {
        const ok = live.failure_rate <= CFG.fail_gate;
        gates.append(el('span', 'gate ' + (ok ? 'pass' : 'fail'), (ok ? '✓' : '✗') + ' failure rate ≤ ' + (CFG.fail_gate * 100).toFixed(2) + '%'));
      }
      if (CFG.p95_gate > 0) {
        const ok = live.p95_ns / 1e9 <= CFG.p95_gate;
        gates.append(el('span', 'gate ' + (ok ? 'pass' : 'fail'), (ok ? '✓' : '✗') + ' p95 ≤ ' + fmtMs(CFG.p95_gate * 1000)));
      }
      $('finale').hidden = false;
      $('finale').querySelector('button').focus();
    }, 900);
  }

  // ── frames (several times a second) ──────────────────────────────────────
  function applyFrame(f) {
    state.serverT = f.t; state.tAt = performance.now();
    setPhase(f.phase);
    for (const ev of f.ev || []) scene.apply(ev);
    if (f.stage) scene.reconcile(f.stage);
    if (f.feed && f.feed.length) addFeed(f.feed);
  }

  // ── connection ───────────────────────────────────────────────────────────
  let source = null, failures = 0;
  function connect() {
    source = new EventSource('/events');
    source.onopen = () => { failures = 0; if (state.phase !== 'done') hideBanner(); };
    source.onmessage = (e) => {
      let msg;
      try { msg = JSON.parse(e.data); } catch (err) { return; }
      if (msg.kind === 'frame') applyFrame(msg.data);
      else if (msg.kind === 'metrics') applyStats(msg.data);
    };
    source.onerror = async () => {
      failures++;
      if (state.phase === 'done') {
        source.close();
        showBanner('The run is over. This page is frozen at its final moment — the report is on disk.', false);
        return;
      }
      showBanner('Connection lost — reconnecting…', false);
      try {
        const r = await fetch('/metrics', { cache: 'no-store' });
        if (r.status === 401) location.reload();
      } catch (err) {
        if (failures >= 3) {
          source.close();
          showBanner('The lemmings process has exited. This page shows the last moment it saw.', true);
        }
      }
    };
  }

  async function coldStart() {
    try {
      const r = await fetch('/api/state', { cache: 'no-store' });
      if (r.status === 401) { location.reload(); return; }
      const s = await r.json();
      state.serverT = s.t; state.tAt = performance.now();
      setPhase(s.phase);
      scene.reconcile(s.stage || []);
      addFeed(s.feed || []);
      applyStats(s.stats);
      if (!(s.feed || []).length) {
        // Nothing curated yet: fall back to the raw event history.
        const rr = await fetch('/replay', { cache: 'no-store' });
        if (rr.ok) {
          const replay = await rr.json();
          addFeed((replay.events || []).filter((e) => e.kind === 'terrain.online' || e.kind === 'visit.error').slice(-20)
            .map((e) => (e.kind === 'visit.error'
              ? { k: 'fail', t: 0, id: e.lemming_id, tr: e.terrain, st: e.status_code, p: (() => { try { return new URL(e.url).pathname; } catch (x) { return e.url; } })(), ms: (e.duration_ns || 0) / 1e6 }
              : { k: 'terrain', t: 0, tr: e.terrain, x: 'online' })));
        }
      }
    } catch (e) { /* SSE will bring us up to date */ }
  }

  // ── wiring ───────────────────────────────────────────────────────────────
  document.addEventListener('click', (e) => {
    const action = e.target.closest('[data-action]');
    if (!action) return;
    switch (action.dataset.action) {
      case 'follow-random': inspector.followRandom(); break;
      case 'release': inspector.release(); break;
      case 'close-finale': $('finale').hidden = true; break;
    }
  });
  document.addEventListener('keydown', (e) => {
    if (e.target.matches('input, textarea') || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === 'f') inspector.followRandom();
    else if (e.key === 't') toggleTheme();
    else if (e.key === 'x') toggleFx();
    else if (e.key === 'Escape') { if (!$('finale').hidden) $('finale').hidden = true; else inspector.release(); }
  });
  window.matchMedia('(prefers-color-scheme: light)').addEventListener('change', (e) => {
    let saved = null;
    try { saved = localStorage.getItem('lemmings-theme'); } catch (err) { /* ignore */ }
    if (saved) return;
    root.dataset.theme = e.matches ? 'light' : 'dark';
    readPalette(); scene.paintBackground(); charts.redraw();
  });
  window.addEventListener('resize', () => charts.redraw());

  setText('plan', fmtInt(CFG.terrain) + ' × ' + fmtInt(CFG.pack) + ' lemmings · ' +
    (CFG.journey ? 'journey “' + CFG.journey + '”' : CFG.navigation + ' navigation') + ' · think ' +
    fmtMs(CFG.think_min * 1000) + '–' + fmtMs(CFG.think_max * 1000));

  readPalette();
  syncToggles();
  scene.init();
  let last = performance.now();
  const loop = (now) => {
    const dt = Math.min(0.05, (now - last) / 1000);
    last = now;
    scene.update(dt);
    scene.draw();
    requestAnimationFrame(loop);
  };
  requestAnimationFrame(loop);
  coldStart().then(connect);
})();
