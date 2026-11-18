'use strict';
// Replays ORswitch's events through RingQ Core's real node services, offline.
//
// Loads /var/www/ringq/node/server.js (and optionally queue.js) unmodified, in this one process, with
// every outside connection redirected or faked:
//   modesl            real library, connected to the test switch's event socket (OR_ESL_PORT / OR_ESL_PASS)
//   redis             real library, private Redis (OR_SBX/redis.sock)
//   pg                real library, private PostgreSQL copy (OR_SBX socket), user postgres
//   socket.io-client  fake: every emit is recorded; emits to the queue service (port 4072) are handed to
//                     queue.js's handlers when queue.js is loaded (OR_TARGETS=server.js,queue.js)
//   socket.io (Server) fake (no port is opened)
//   firebase-admin, node-cron, ffmpeg, ws, ./secret.json, ./turnCredentials, dotenv: stubs
// Any other network connection and any child process is refused. It never touches the live services.
//
// Control: prints "HARNESS READY" when the event socket is connected; reads "dump" on stdin, writes the
// recorded socket.io emits to OR_OUT and exits.

const Module = require('module');
const path = require('path');
const fs = require('fs');
const net = require('net');
const cp = require('child_process');
const { EventEmitter } = require('events');

const NODE_DIR = process.env.OR_NODE_DIR || '/var/www/ringq/node';
const SBX = process.env.OR_SBX || '/tmp/rq_reports_sbx';
const ESL_PORT = Number(process.env.OR_ESL_PORT);
const ESL_PASS = process.env.OR_ESL_PASS || 'ClueCon';
const OUT = process.env.OR_OUT || '/tmp/rq_harness_out.json';
const TARGETS = (process.env.OR_TARGETS || 'server.js').split(',').filter(Boolean);

if (!ESL_PORT) { console.error('OR_ESL_PORT is required'); process.exit(2); }

const real = (m) => require(path.join(NODE_DIR, 'node_modules', m));
const emits = [];
const handlers = {}; // queue.js socket handlers by event name
const dbWrites = [];

// ---- deep fake for libraries we do not run (firebase-admin ...) ----
function deepFake() {
  const f = function () { return proxy; };
  const proxy = new Proxy(f, {
    get: (t, k) => (k === 'then' || k === Symbol.toPrimitive ? undefined : proxy),
    apply: () => proxy,
    construct: () => proxy,
  });
  return proxy;
}

// ---- fake socket.io-client ----
function fakeIo(url) {
  const s = new EventEmitter();
  s.connected = true;
  s.id = 'fake-' + url;
  s.connect = () => s;
  s.disconnect = () => s;
  s.close = () => s;
  s.on = s.on.bind(s);
  s.emit = (ev, ...args) => {
    const rec = { url: String(url), event: ev, args: JSON.parse(JSON.stringify(args, (k, v) => (typeof v === 'function' ? undefined : v))) };
    emits.push(rec);
    if (String(url).includes(':4072') && handlers[ev]) {
      const cb = args.find((a) => typeof a === 'function');
      try { Promise.resolve(handlers[ev](...args.filter((a) => typeof a !== 'function'), cb || (() => {}))).catch((e) => console.error('[harness] queue handler', ev, e && e.message)); } catch (e) { console.error('[harness] queue handler', ev, e && e.message); }
    }
    return true;
  };
  return s;
}

// ---- fake socket.io Server (queue.js) ----
class FakeServer extends EventEmitter {
  constructor() { super(); }
  listen() { return this; }
  use() { return this; }
  of() { return this; }
  to() { return { emit: (...a) => emits.push({ url: 'server', event: a[0], args: a.slice(1) }) }; }
  emit(...a) { emits.push({ url: 'server', event: a[0], args: a.slice(1) }); }
  on(ev, fn) {
    if (ev === 'connection') {
      const sock = new EventEmitter();
      sock.handshake = { headers: {}, query: {}, auth: {} };
      sock.id = 'fake-queue-client';
      sock.join = () => {};
      sock.to = () => ({ emit: () => {} });
      sock.emit = (...a) => emits.push({ url: 'queue->client', event: a[0], args: a.slice(1) });
      const origOn = sock.on.bind(sock);
      sock.on = (e, h) => { handlers[e] = h; return origOn(e, h); };
      setImmediate(() => { try { fn(sock); } catch (e) { console.error('[harness] connection handler', e && e.message); } });
    }
    return this;
  }
}

// OR_TRACE_KEYS=regex: log reads and writes of matching Redis keys with a time stamp (ordering problems)
const TRACE = process.env.OR_TRACE_KEYS ? new RegExp(process.env.OR_TRACE_KEYS) : null;
function traceRedis(c) {
  if (!TRACE) return c;
  for (const m of ['get', 'set', 'del']) {
    const orig = c[m].bind(c);
    c[m] = (k, ...a) => { if (TRACE.test(String(k))) console.log('[redis-trace]', new Date().toISOString().slice(11, 23), m, k, a.length ? JSON.stringify(a[0]) : ''); return orig(k, ...a); };
  }
  return c;
}

// ---- module hooks ----
const origLoad = Module._load;
Module._load = function (request, parent, isMain) {
  if (request === 'modesl') {
    const e = real('modesl');
    function Connection(h, p, pw, cb) { return new e.Connection('127.0.0.1', ESL_PORT, ESL_PASS, cb); }
    return Object.assign({}, e, { Connection });
  }
  if (request === 'redis') {
    const r = real('redis');
    return Object.assign({}, r, {
      createClient: (opts) => traceRedis(r.createClient({ socket: { path: SBX + '/redis.sock', reconnectStrategy: (n) => Math.min(n * 100, 2000) } })),
    });
  }
  if (request === 'pg') {
    const pg = real('pg');
    class Pool extends pg.Pool {
      constructor() { super({ host: SBX, user: 'postgres', database: 'ringq', port: 5432, max: 6 }); }
      query(sql, ...rest) {
        const text = typeof sql === 'string' ? sql : (sql && sql.text) || '';
        if (/^\s*(insert|update|delete)/i.test(text)) dbWrites.push(text.replace(/\s+/g, ' ').slice(0, 160));
        return super.query(sql, ...rest);
      }
    }
    return Object.assign({}, pg, { Pool });
  }
  if (request === 'socket.io-client') return { io: fakeIo, default: fakeIo, connect: fakeIo };
  if (request === 'socket.io') return { Server: FakeServer };
  if (request === 'firebase-admin') return deepFake();
  if (request === 'node-cron') return { schedule: () => ({ stop() {}, start() {} }), validate: () => true };
  if (request === 'ffmpeg-static') return '/bin/false';
  if (request === 'fluent-ffmpeg') { const f = () => deepFake(); f.setFfmpegPath = () => {}; return f; }
  if (request === 'dotenv') return { config: () => ({}), parse: real('dotenv').parse };
  if (/(^|\/)secret\.json$/.test(request)) return {};
  if (request === './turnCredentials') return { start() {} };
  return origLoad.apply(this, arguments);
};

// ---- network and process guards ----
const origConnect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function (...args) {
  let o = args[0];
  if (Array.isArray(o)) o = o[0];
  const isPath = (typeof o === 'string' && isNaN(Number(o))) || (o && typeof o === 'object' && o.path);
  const port = o && typeof o === 'object' ? o.port : (typeof o === 'number' ? o : Number(args[0]));
  const host = o && typeof o === 'object' ? o.host : args[1];
  if (isPath || (port === ESL_PORT && (!host || host === '127.0.0.1' || host === 'localhost'))) return origConnect.apply(this, args);
  console.error('[harness] blocked connection to', host, port);
  process.nextTick(() => this.destroy(new Error('blocked by harness')));
  return this;
};
for (const fn of ['exec', 'execSync', 'spawn', 'spawnSync', 'execFile', 'execFileSync', 'fork']) {
  cp[fn] = function () {
    console.error('[harness] blocked child process', fn, String(arguments[0]).slice(0, 80));
    const e = new EventEmitter(); e.stdout = new EventEmitter(); e.stderr = new EventEmitter(); e.kill = () => {};
    if (/Sync$/.test(fn)) throw new Error('blocked by harness');
    process.nextTick(() => e.emit('error', new Error('blocked by harness')));
    return e;
  };
}
process.on('uncaughtException', (e) => console.error('[harness] uncaught', e && e.stack || e));
process.on('unhandledRejection', (e) => console.error('[harness] unhandled', e && e.stack || e));

// ---- load Core's services, queue.js last (it needs nothing from server.js) ----
for (const t of TARGETS) {
  console.log('[harness] loading', t);
  require(path.join(NODE_DIR, t));
}

// ready: poll until the ESL connection answers
setTimeout(() => console.log('HARNESS READY'), Number(process.env.OR_READY_MS || 2500));

function dump() {
  fs.writeFileSync(OUT, JSON.stringify({ emits, dbWrites }, null, 1));
  console.log('[harness] dumped', emits.length, 'emits', dbWrites.length, 'writes');
  setTimeout(() => process.exit(0), 200);
}
let buf = '';
process.stdin.on('data', (d) => { buf += d; if (/dump/.test(buf)) dump(); });
process.stdin.resume();
