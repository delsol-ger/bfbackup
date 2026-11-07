'use strict';
// BFBackup web UI. Plain JS, no build step. All user data is inserted as
// text (never innerHTML), so file names cannot inject markup.

// ---------- DOM helpers ----------
function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (k === 'dataset') Object.assign(el.dataset, v);
      else if (typeof v === 'boolean' || typeof v === 'number' && k in el) el[k] = v;
      else el.setAttribute(k, v);
    }
  }
  add(el, kids);
  return el;
}
function add(el, kids) {
  for (const k of kids.flat(Infinity)) {
    if (k == null || k === false) continue;
    el.append(k instanceof Node ? k : document.createTextNode(String(k)));
  }
  return el;
}
// fill replaces the children of el; null and false are skipped, unlike
// replaceChildren, which would show them as "null".
function fill(el, ...kids) {
  el.replaceChildren();
  return add(el, kids);
}
const $ =(sel, root = document) => root.querySelector(sel);

// Small inline SVG icons (emoji do not render on every system).
const ICONS = {
  dir: 'M1.5 4a1 1 0 0 1 1-1h3.7l1.5 1.5h5.8a1 1 0 0 1 1 1V12a1 1 0 0 1-1 1h-11a1 1 0 0 1-1-1z',
  file: 'M4 1.5h5l3 3V14.5H4z M9 1.5v3h3',
  link: 'M6.6 9.4l2.8-2.8 M5.2 7.5 4 8.7a2.3 2.3 0 0 0 3.3 3.3l1.2-1.2 M10.8 8.5 12 7.3A2.3 2.3 0 0 0 8.7 4L7.5 5.2',
};
function icon(kind) {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('class', 'ico ico-' + kind);
  svg.setAttribute('aria-hidden', 'true');
  const p = document.createElementNS(NS, 'path');
  p.setAttribute('d', ICONS[kind]);
  svg.append(p);
  return svg;
}
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

// ---------- formatting ----------
function fmtBytes(n) {
  n = Number(n || 0);
  // Decimal units, as Unraid shows them.
  if (n < 1000) return n + ' B';
  const u = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let i = -1;
  do { n /= 1000; i++; } while (n >= 1000 && i < u.length - 1);
  return n.toFixed(n < 10 ? 2 : n < 100 ? 1 : 0) + ' ' + u[i];
}
const fmtNum = n => Number(n || 0).toLocaleString();
// Dates and times follow Settings → Display (state.meta.date_format):
// iso 2026-11-07 14:30, de 07.11.2026 14:30, uk 07/11/2026 14:30,
// us 11/07/2026 2:30 PM.
const DATE_FORMATS = { iso: 'YYYY-MM-DD, 24 h', de: 'DD.MM.YYYY, 24 h', uk: 'DD/MM/YYYY, 24 h', us: 'MM/DD/YYYY, 12 h' };
const display = { date: 'iso', features: { usb_drives: true, hooks: true } };
const pad2 = n => String(n).padStart(2, '0');
function fmtDateOf(d, fmt = display.date) {
  const Y = d.getFullYear(), M = pad2(d.getMonth() + 1), D = pad2(d.getDate());
  return fmt === 'de' ? `${D}.${M}.${Y}` : fmt === 'uk' ? `${D}/${M}/${Y}` : fmt === 'us' ? `${M}/${D}/${Y}` : `${Y}-${M}-${D}`;
}
function fmtClockOf(d, seconds, fmt = display.date) {
  const s = seconds ? ':' + pad2(d.getSeconds()) : '';
  if (fmt === 'us') return `${d.getHours() % 12 || 12}:${pad2(d.getMinutes())}${s} ${d.getHours() < 12 ? 'AM' : 'PM'}`;
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}${s}`;
}
const fmtDateTimeOf = (d, seconds, fmt) => fmtDateOf(d, fmt) + ' ' + fmtClockOf(d, seconds, fmt);
function fmtTime(ts) {
  if (!ts) return '–';
  return fmtDateTimeOf(new Date(ts * 1000), false);
}
// Time of day only, with seconds.
function fmtClock(ts) {
  if (!ts) return '–';
  return fmtClockOf(new Date(ts * 1000), true);
}
// A day "YYYY-MM-DD" without the year, for chart labels.
function fmtDayShort(key) {
  const [, M, D] = key.split('-');
  return display.date === 'de' ? `${D}.${M}.` : display.date === 'uk' ? `${D}/${M}` : display.date === 'us' ? `${M}/${D}` : `${M}-${D}`;
}
const fmtDay = key => { const [Y, M, D] = key.split('-').map(Number); return fmtDateOf(new Date(Y, M - 1, D)); };
// Names sorted the way people expect: case-insensitive, numbers by value.
const byName = (a, b) => a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' });
function fmtNano(ns) { return ns ? fmtTime(Math.floor(ns / 1e9)) : '–'; }
function ago(ts) {
  if (!ts) return 'never';
  const s = Math.round(Date.now() / 1000 - ts);
  const abs = Math.abs(s);
  const f = abs < 60 ? [abs, 'second'] : abs < 3600 ? [Math.round(abs / 60), 'minute'] :
    abs < 86400 ? [Math.round(abs / 3600), 'hour'] : [Math.round(abs / 86400), 'day'];
  const txt = `${f[0]} ${f[1]}${f[0] === 1 ? '' : 's'}`;
  return s >= 0 ? txt + ' ago' : 'in ' + txt;
}
function fmtDur(sec) {
  if (sec == null || sec < 0) return '–';
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm ' + (sec % 60) + 's';
  if (sec < 86400) return Math.floor(sec / 3600) + 'h ' + Math.floor(sec % 3600 / 60) + 'm';
  return Math.floor(sec / 86400) + 'd ' + Math.floor(sec % 86400 / 3600) + 'h';
}
function runDuration(r) {
  if (!r.started_at) return '–';
  const end = r.finished_at || Math.floor(Date.now() / 1000);
  return fmtDur(end - r.started_at);
}
const KIND = {
  backup: 'Backup', dryrun: 'Dry run', verify_quick: 'Quick verify', verify_deep: 'Deep verify',
  restore: 'Restore', delete: 'Delete', rebuild_index: 'Rebuild index',
};
const STATUS = {
  success: ['ok', 'Success'], warning: ['warn', 'Warning'], failed: ['bad', 'Failed'], cancelled: ['', 'Cancelled'],
  running: ['info', 'Running'], queued: ['', 'Queued'], awaiting_approval: ['violet', 'Needs approval'],
  approved: ['ok', 'Continued'], interrupted: ['warn', 'Interrupted'], declined: ['', 'Stopped by you'],
};
// runKind names a run's kind; a copy job's backup run once as a sync is "Sync".
function runKind(r) { return r.details && r.details.sync ? 'Sync' : KIND[r.kind] || r.kind; }
function badge(status) {
  const [cls, label] = STATUS[status] || ['', status];
  return h('span', { class: 'badge ' + cls }, label);
}

// ---------- API ----------
class ApiError extends Error {
  constructor(msg, status) { super(msg); this.status = status; }
}
async function api(method, path, body) {
  const opt = { method, headers: { 'X-BFB-Request': '1' }, credentials: 'same-origin' };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const res = await fetch(path, opt);
  let data = null;
  try { data = await res.json(); } catch (_) { /* empty body */ }
  if (res.status === 401 && !path.startsWith('/api/auth/')) {
    showLogin();
    throw new ApiError('Please log in.', 401);
  }
  if (!res.ok) throw new ApiError((data && data.error) || res.statusText, res.status);
  return data;
}
const GET = p => api('GET', p);
const POST = (p, b) => api('POST', p, b === undefined ? {} : b);
const PUT = (p, b) => api('PUT', p, b);
const DEL = p => api('DELETE', p);
const q = obj => new URLSearchParams(Object.entries(obj).filter(([, v]) => v !== '' && v != null)).toString();

// ---------- toasts, modals ----------
function toast(msg, kind) {
  const t = h('div', { class: 'toast ' + (kind || '') }, msg);
  $('#toasts').append(t);
  setTimeout(() => t.remove(), kind === 'bad' ? 7000 : 3500);
}
function guard(fn) {
  return async (...args) => {
    try { await fn(...args); } catch (e) { if (e.status !== 401) toast(e.message, 'bad'); }
  };
}

function modal({ title, body, actions = [], wide = false, onClose }) {
  const back = h('div', { class: 'modal-back' });
  const close = () => { back.remove(); document.removeEventListener('keydown', esc); if (onClose) onClose(); };
  const esc = e => { if (e.key === 'Escape') close(); };
  document.addEventListener('keydown', esc);
  back.addEventListener('mousedown', e => { if (e.target === back) close(); });
  const foot = h('div', { class: 'modal-foot' });
  for (const a of actions) {
    const b = h('button', { type: 'button', class: a.kind || '' }, a.label);
    b.addEventListener('click', async () => {
      if (!a.onclick) return close();
      b.disabled = true;
      try { if ((await a.onclick(close)) !== false) close(); }
      catch (e) { toast(e.message, 'bad'); }
      finally { b.disabled = false; }
    });
    foot.append(b);
  }
  back.append(h('div', { class: 'modal' + (wide ? ' wide' : ''), role: 'dialog', 'aria-modal': 'true' },
    h('div', { class: 'modal-head' }, h('h2', null, title), h('button', { type: 'button', class: 'link', onclick: close, 'aria-label': 'Close' }, '✕')),
    h('div', { class: 'modal-body' }, body),
    actions.length ? foot : null));
  document.body.append(back);
  const first = $('input, select, textarea', back);
  if (first) first.focus();
  return { close, el: back };
}

function confirmBox(title, message, { ok = 'Confirm', danger = false, word = '' } = {}) {
  return new Promise(resolve => {
    let done = false;
    const input = word ? h('input', { type: 'text', placeholder: word, autocomplete: 'off' }) : null;
    modal({
      title,
      body: [h('p', null, message), word ? h('label', { class: 'field' }, h('span', null, `Type "${word}" to confirm`), input) : null],
      actions: [
        { label: 'Cancel' },
        {
          label: ok, kind: danger ? 'danger solid' : 'primary', onclick: () => {
            if (word && input.value.trim() !== word) { toast(`Type "${word}" to confirm.`, 'bad'); return false; }
            done = true; resolve(true);
          },
        },
      ],
      onClose: () => { if (!done) resolve(false); },
    });
  });
}

function copyButton(text) {
  return h('button', {
    type: 'button', class: 'small', onclick: async e => {
      try { await navigator.clipboard.writeText(text); toast('Copied'); }
      catch (_) {
        const sel = e.target.closest('.modal-body, .panel');
        toast('Copy not allowed on plain http; select the text and copy it manually.');
        if (sel) { const t = $('.token', sel); if (t) window.getSelection().selectAllChildren(t); }
      }
    },
  }, 'Copy');
}

// ---------- shared state ----------
const state = { nodes: [], drives: [], jobs: [], meta: null };
async function loadNodes() {
  [state.nodes, state.drives] = await Promise.all([GET('/api/nodes'), GET('/api/drives').then(v => v.drives).catch(() => state.drives)]);
  return state.nodes;
}
async function loadJobs() { state.jobs = await GET('/api/jobs'); return state.jobs; }
// ensureDrives reloads the USB drives when a target names one the page does
// not know yet (set up in another window, or since this page was opened), so
// labels show its name instead of its ID.
async function ensureDrives(jobs) {
  if (jobs.some(j => j.targets.some(t => isDrive(t.node_id) && !driveOf(t.node_id)))) {
    state.drives = await GET('/api/drives').then(v => v.drives).catch(() => state.drives);
  }
}
// USB drives are nodes "usb:<uuid>"; paths on them are relative to the drive.
const isDrive = id => typeof id === 'string' && id.startsWith('usb:');
const driveOf = id => state.drives.find(d => d.node_id === id);
function nodeName(id) {
  if (isDrive(id)) { const d = driveOf(id); return 'USB · ' + (d ? d.name : id.slice(4)); }
  const n = state.nodes.find(n => n.id === id);
  return n ? n.name : id;
}
// driveFolder is the default folder of a job on a USB drive.
const driveFolder = name => '/' + (name.replace(/[/\\]/g, '_').trim() || 'backup');
function findTarget(id) {
  for (const j of state.jobs) for (const t of j.targets) if (t.id === id) return { job: j, target: t };
  return null;
}
function runLabel(r) {
  const job = state.jobs.find(j => j.id === r.job_id);
  if (!job) return `job #${r.job_id}`;
  const t = job.targets.find(t => t.id === r.target_id);
  return t ? `${job.name} → ${nodeName(t.node_id)}` : job.name;
}

// ---------- router ----------
let cleanups = [];
function onLeave(fn) { cleanups.push(fn); }
function every(ms, fn) { const id = setInterval(fn, ms); onLeave(() => clearInterval(id)); }

const routes = [
  [/^#?\/?(?:\?.*)?$/, 'dashboard', () => viewDashboard()],
  [/^#\/jobs(?:\?.*)?$/, 'jobs', () => viewJobs()],
  [/^#\/jobs\/new(?:\?.*)?$/, 'jobs', () => viewJobEdit(null)],
  [/^#\/jobs\/(\d+)(?:\?.*)?$/, 'jobs', m => viewJobEdit(Number(m[1]))],
  [/^#\/browse\/(\d+)(?:\?.*)?$/, 'jobs', m => viewBrowse(Number(m[1]))],
  [/^#\/runs(?:\?.*)?$/, 'runs', () => viewRuns()],
  [/^#\/runs\/(\d+)(?:\?.*)?$/, 'runs', m => viewRun(Number(m[1]))],
  [/^#\/logs(?:\?.*)?$/, 'logs', () => viewLogs()],
  [/^#\/nodes(?:\?.*)?$/, 'nodes', () => viewNodes()],
  [/^#\/drives(?:\?.*)?$/, 'drives', () => viewDrives()],
  [/^#\/stats(?:\?.*)?$/, 'stats', () => viewStats()],
  [/^#\/settings(?:\?.*)?$/, 'settings', () => viewSettings()],
];

async function render() {
  cleanups.forEach(f => f());
  cleanups = [];
  const hash = location.hash || '#/';
  const main = $('#main');
  for (const [re, name, view] of routes) {
    const m = hash.match(re);
    if (!m) continue;
    $$('#nav a, #tabbar a').forEach(a => a.classList.toggle('active', a.dataset.route === name));
    $('#more').classList.toggle('active', $('#more').dataset.routes.split(' ').includes(name));
    main.replaceChildren(h('div', { class: 'spinner' }));
    try {
      const node = await view(m);
      main.replaceChildren(node);
    } catch (e) {
      if (e.status === 401) return;
      main.replaceChildren(h('div', { class: 'callout bad' }, 'Could not load this page: ' + e.message));
    }
    window.scrollTo(0, 0);
    return;
  }
  main.replaceChildren(h('div', { class: 'empty' }, 'Page not found. ', h('a', { href: '#/' }, 'Go to the dashboard')));
}

function pageHead(title, sub, ...actions) {
  return h('div', { class: 'page-head' },
    h('div', null, h('h1', null, title), sub ? h('div', { class: 'sub' }, sub) : null),
    h('div', { class: 'row' }, actions));
}
function panel(title, body, ...actions) {
  return h('section', { class: 'panel' },
    title ? h('div', { class: 'panel-head' }, h('h2', null, title), h('div', { class: 'row' }, actions)) : null,
    body);
}
// foldPanel is a panel that opens and closes, closed at first; onOpen runs
// the first time it opens.
function foldPanel(title, body, onOpen) {
  const d = h('details', { class: 'panel fold' }, h('summary', { class: 'panel-head' }, h('h2', null, title)), body);
  d.addEventListener('toggle', () => {
    if (d.open && onOpen) { const f = onOpen; onOpen = null; f(); }
  });
  return d;
}
function table(headers, rows, empty = 'Nothing here yet.') {
  if (!rows.length) return h('div', { class: 'empty' }, empty);
  return h('div', { class: 'table-wrap' }, h('table', null,
    h('thead', null, h('tr', null, headers.map(x => typeof x === 'string' ? h('th', null, x) : x))),
    h('tbody', null, rows)));
}
function progressBar(frac, cls = '') {
  const inner = h('div');
  inner.style.width = Math.max(0, Math.min(100, frac * 100)).toFixed(1) + '%';
  return h('div', { class: 'bar ' + cls }, inner);
}

// Repeatedly loads data and redraws a container, but only when the data
// changed, so buttons are not replaced under the mouse while nothing happens.
function live(ms, load, draw) {
  const box = h('div', { class: 'stack' });
  let busy = false;
  let last = '';
  const tick = async () => {
    if (busy) return;
    busy = true;
    try {
      const data = await load();
      const key = JSON.stringify(data);
      if (key !== last) {
        last = key;
        fill(box, draw(data));
      }
    }
    catch (e) { last = ''; if (e.status !== 401) box.replaceChildren(h('div', { class: 'callout bad' }, e.message)); }
    finally { busy = false; }
  };
  every(ms, tick);
  return tick().then(() => box);
}

// ---------- runs: start, describe ----------
async function startRun(targetId, kind, extra = {}) {
  const r = await POST(`/api/targets/${targetId}/run`, { kind, ...extra });
  toast(`${KIND[kind]} started (run #${r.id})`);
  return r;
}

function runCard(ar, withLabel = true) {
  const p = ar.progress || {};
  const pct = p.percent >= 0 ? p.percent : null;
  const queued = p.phase === 'queued';
  return h('div', { class: 'run-card stack' },
    h('div', { class: 'row between' },
      h('div', null,
        h('a', { href: `#/runs/${ar.id}` }, h('strong', null, `${runKind(ar)} #${ar.id}`)),
        withLabel ? h('span', { class: 'muted' }, ' · ', runLabel(ar)) : null),
      h('div', { class: 'row' }, badge(queued ? 'queued' : ar.status), h('span', { class: 'muted small' }, queued ? ar.waiting || 'waiting to start' : p.phase || ''),
        h('button', { type: 'button', class: 'small danger', onclick: guard(() => stopRun(ar.id)) }, 'Stop'))),
    h('div', { class: 'progress-line' },
      progressBar(pct == null ? 0 : pct / 100, pct == null && !queued ? 'indet' : ''),
      h('strong', { class: 'pct' }, pct == null ? '' : pct.toFixed(pct < 10 ? 1 : 0) + ' %')),
    h('div', { class: 'small muted' }, progressDetails(p, ar)),
    queued ? null : progressAlert(p),
    currentWork(p),
    p.latest && !queued ? h('div', { class: 'latest small muted', title: p.latest }, 'Latest: ', p.latest, p.latest_at ? ` · ${ago(p.latest_at)}` : '') : null);
}

function progressDetails(p, ar) {
  const scanning = /^(scanning|checking the target)/.test(p.phase || '');
  const parts = [];
  if (scanning) parts.push(`${fmtNum(p.scanned)} files (${fmtBytes(p.scanned_bytes || 0)}) scanned`);
  if (scanning && p.dirs_read) parts.push(`${fmtNum(p.dirs_read)} folders read`);
  if (scanning && p.hashed_bytes) parts.push(`${fmtBytes(p.hashed_bytes)} read for checksums`);
  if (!scanning && p.files_total) parts.push(`${fmtNum(p.files_done)} / ${fmtNum(p.files_total)} files`);
  if (!scanning && p.bytes_total) parts.push(`${fmtBytes(p.bytes_done)} / ${fmtBytes(p.bytes_total)}`);
  if (p.bytes_per_sec) parts.push(`${fmtBytes(p.bytes_per_sec)}/s`);
  if (p.eta_seconds >= 0) parts.push(`about ${fmtDur(p.eta_seconds)} left${scanning ? ' in the scan' : ''}`);
  if (p.phase && p.phase !== 'queued' && p.phase_seconds >= 60) parts.push(`this step ${fmtDur(p.phase_seconds)}`);
  if (ar && ar.started_at) parts.push(`running ${runDuration(ar)}`);
  return parts.join(' · ');
}

// No warning before this: short pauses (a large folder, a slow disk
// spinning up) are normal.
const IDLE_WARN = 180;

// What holds the run up: a peer it waits for, or nothing moving for a while,
// with when the watchdog will stop it.
function progressAlert(p) {
  if (p.waiting) return h('div', { class: 'run-alert warn' }, '⏳ ', p.waiting);
  if (!(p.idle_seconds >= IDLE_WARN) || /hook/.test(p.phase || '')) return null;
  const stall = p.stall_seconds || 0;
  const where = (p.current || [])[0] || ((p.files || [])[0] || {}).path;
  let txt = `⚠ nothing has changed for ${fmtDur(p.idle_seconds)} while ${p.phase}`;
  if (where) txt += ` (${where})`;
  if (stall) {
    const left = stall - p.idle_seconds;
    txt += left > 0 ? ` · stopped automatically in ${fmtDur(left)}` : ' · being stopped';
  }
  return h('div', { class: 'run-alert ' + (stall && p.idle_seconds >= stall * 2 / 3 ? 'bad' : 'warn') }, txt);
}

// What the run works on now: folders a scan reads, files being copied.
function currentWork(p) {
  const rows = (p.current || []).slice(0, 4).map(c => h('div', { class: 'current' }, c));
  const files = p.files || [];
  for (const f of files.slice(0, 6)) {
    const big = f.size >= 64 << 20;
    rows.push(h('div', { class: 'current file' },
      h('span', { class: 'name' }, f.path),
      big ? progressBar(f.done / f.size, 'thin') : null,
      h('span', { class: 'nowrap' }, big ? `${fmtBytes(f.done)} / ${fmtBytes(f.size)}` : fmtBytes(f.size),
        f.bytes_per_sec >= 1 && f.seconds >= 2 ? ` · ${fmtBytes(f.bytes_per_sec)}/s` : '',
        f.seconds >= 60 ? ` · ${fmtDur(f.seconds)}` : '')));
  }
  if (files.length > 6) rows.push(h('div', { class: 'current' }, `+ ${files.length - 6} more`));
  return rows.length ? h('div', null, rows) : null;
}

// The steps of a run with their durations; the running one ends now.
function phaseTimeline(phases) {
  const now = Math.floor(Date.now() / 1000);
  const spans = phases.map(s => ({ ...s, dur: Math.max(0, (s.end || now) - s.start) }));
  const longest = Math.max(1, ...spans.map(s => s.dur));
  return table(['Step', 'Started', h('th', { class: 'num' }, 'Duration'), h('th', { class: 'sm-hide' })], spans.map(s => h('tr', null,
    h('td', null, s.name, s.end ? null : [' ', h('span', { class: 'badge info' }, 'now')]),
    h('td', { class: 'small nowrap' }, fmtClock(s.start)),
    h('td', { class: 'num nowrap' }, fmtDur(s.dur)),
    h('td', { class: 'timeline-cell sm-hide' }, progressBar(s.dur / longest)))));
}

// Short "45 % · 2h left" text for tables.
function progressShort(p) {
  if (!p) return 'running…';
  if (p.phase === 'queued') return 'queued';
  const parts = [p.percent >= 0 ? Math.floor(p.percent) + ' %' : p.phase || 'running'];
  if (p.eta_seconds >= 0) parts.push(fmtDur(p.eta_seconds) + ' left');
  return parts.join(' · ');
}

async function stopRun(id) {
  if (!await confirmBox('Stop run', 'Stop this run now? Files that are completely copied stay in the backup; the next backup continues where this one stopped.', { ok: 'Stop run', danger: true })) return;
  await POST(`/api/runs/${id}/cancel`);
  toast('Stopping…');
}

async function stopAll(n) {
  if (!await confirmBox('Stop all runs', `Stop all ${n} running and queued run(s)? Completely copied files are kept.`, { ok: 'Stop all', danger: true })) return;
  const r = await POST('/api/runs/cancel-all');
  toast(`Stopping ${r.stopped} run(s)…`);
}

// Lets the user click through the source and add files or folders to the
// exclusions. add(pattern) is called for every pick.
function pickExcludes({ node, base, add }) {
  base = base.replace(/\/$/, '');
  let current = base;
  const list = h('div', { class: 'picker-list' });
  const crumbs = h('div', { class: 'crumbs' });
  const pattern = (abs, isDir) => '/' + abs.slice(base.length + 1) + (isDir ? '/' : '');
  const load = async () => {
    list.replaceChildren(h('div', { class: 'spinner' }));
    crumbs.replaceChildren(h('button', { type: 'button', onclick: () => { current = base; load(); } }, base));
    const rel = current.slice(base.length + 1);
    let acc = base;
    for (const p of rel.split('/').filter(Boolean)) {
      acc += '/' + p;
      const to = acc;
      crumbs.append(h('span', { class: 'sep' }, '/'), h('button', { type: 'button', onclick: () => { current = to; load(); } }, p));
    }
    if (current !== base) {
      crumbs.append(h('button', { type: 'button', class: 'small', onclick: () => { add(pattern(current, true)); } }, 'Exclude this folder'));
    }
    try {
      const res = await GET(`/api/nodes/${encodeURIComponent(node)}/ls?` + q({ path: current }));
      res.entries.sort((a, b) => ((b.t === 3) - (a.t === 3)) || byName(a.n, b.n));
      list.replaceChildren(...(res.entries.length ? res.entries.map(e => {
        const abs = current + '/' + e.n;
        const isDir = e.t === 3;
        return h('div', { class: 'picker-row' },
          isDir ? h('button', { type: 'button', class: 'grow', onclick: () => { current = abs; load(); } }, icon('dir'), e.n)
            : h('span', { class: 'grow' }, icon(e.t === 2 ? 'link' : 'file'), e.n, h('span', { class: 'muted small' }, '  ', fmtBytes(e.s))),
          h('button', { type: 'button', class: 'small', onclick: () => add(pattern(abs, isDir)) }, 'Exclude'));
      }) : [h('div', { class: 'empty' }, 'Empty folder')]));
    } catch (e) { list.replaceChildren(h('div', { class: 'callout bad' }, e.message)); }
  };
  modal({
    title: 'Pick exclusions', wide: true,
    body: [h('div', { class: 'help' }, 'Each pick adds a pattern anchored at the source folder, e.g. /Photos/cache/. Open folders to go deeper.'), crumbs, list],
    actions: [{ label: 'Done', kind: 'primary' }],
  });
  load();
}

function diskBar(d) {
  const frac = d.total ? d.used / d.total : 0;
  return h('div', { class: 'disk' },
    h('div', { class: 'row between small' }, h('span', { class: 'mono break' }, d.path), h('span', { class: 'muted nowrap' }, `${fmtBytes(d.free)} free of ${fmtBytes(d.total)}`)),
    progressBar(frac, frac > 0.9 ? 'bad' : frac > 0.8 ? 'warn' : ''));
}

// ---------- dashboard ----------
async function viewDashboard() {
  const body = await live(3000, async () => {
    const o = await GET('/api/overview');
    await ensureDrives(o.jobs);
    return o;
  }, o => {
    state.nodes = o.nodes;
    state.jobs = o.jobs;
    const targets = o.jobs.flatMap(j => j.targets.map(t => ({ j, t })));
    const peers = o.nodes.filter(n => !n.local);
    const online = peers.filter(n => n.online).length;
    const out = [];
    out.push(h('div', { class: 'grid tiles' },
      tile('Stored in backups', fmtBytes(o.stored_bytes), fmtNum(o.stored_files) + ' files'),
      tile('Backup jobs', o.jobs.length, `${targets.length} target${targets.length === 1 ? '' : 's'}`),
      tile('Peers online', `${online} / ${peers.length}`, peers.length ? '' : 'no peers added yet'),
      tile('Needs attention', o.approvals.length + o.failures.length, o.approvals.length ? `${o.approvals.length} waiting for your decision` : 'no decisions pending'),
      o.drives && display.features.usb_drives ? h('a', { class: 'tile-link', href: '#/drives' }, tile('USB drives', `${o.drives_connected} / ${o.drives}`,
        o.drives_overdue ? h('span', { class: 'warn-text' }, `${o.drives_overdue} overdue`) : 'connected')) : null));

    const attention = [...o.approvals.map(r => decisionCard(r)), ...o.failures.map(r => problemCard(r))];
    if (attention.length) out.push(panel('Needs your attention', h('div', { class: 'stack' }, attention)));
    if (o.active.length) {
      const anyQueued = o.active.some(a => (a.progress || {}).phase === 'queued');
      out.push(panel(anyQueued ? 'Running and queued' : 'Running now', h('div', null, o.active.map(a => runCard(a))),
        o.active.length > 1 ? h('button', { type: 'button', class: 'small danger', onclick: guard(() => stopAll(o.active.length)) }, 'Stop all') : null));
    }
    if (!o.jobs.length) {
      out.push(panel(null, h('div', { class: 'empty' }, 'No backup jobs yet. ',
        h('a', { href: '#/jobs/new' }, 'Create the first job'), ' – or ', h('a', { href: '#/nodes' }, 'add a peer'), ' first.')));
    } else if (!attention.length && !o.active.length) {
      out.push(panel(null, h('div', { class: 'empty' }, 'All quiet: nothing needs your attention. ', h('a', { href: '#/jobs' }, 'Backup jobs'))));
    }
    return out;
  });
  return h('div', null, pageHead('Dashboard', null, h('a', { class: 'btn', href: '#/jobs/new' }, '+ New job')), body);
}
function tile(label, value, hint) {
  return h('div', { class: 'panel tile' }, h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), hint ? h('div', { class: 'hint' }, hint) : null);
}
function nodeStatusDot(online) {
  return h('span', { class: 'badge plain ' + (online ? 'ok' : 'bad'), title: online ? 'online' : 'offline' }, online ? '●' : '○');
}
// decide answers a run that waits: continue (yes) or stop. No pop-up: the
// card the buttons sit on says what happens.
async function decide(id, yes) {
  if (yes) {
    const r = await POST(`/api/runs/${id}/approve`);
    toast(`Continues as run #${r.id}`);
  } else {
    await POST(`/api/runs/${id}/decline`);
    toast('Stopped. Nothing was changed.');
  }
  render();
}
async function dismiss(id) {
  await POST(`/api/runs/${id}/ack`);
  render();
}
const ANOMALY_NAMES = {
  history: 'Unusual compared with earlier backups', fixed: 'Too many changed files', deletions: 'Many files would be removed',
  encryption: 'Files look encrypted', extensions: 'Suspicious renames', shrink: 'Source much smaller',
  truncation: 'Files became empty', time_jump: 'Times in the future',
};
// decisionCard shows a run that waits for the user, with what it found.
function decisionCard(r) {
  const d = (r.details && r.details.decision) || {};
  const anomalies = d.anomalies || [];
  const what = d.kind === 'conflict' ? 'Continue replaces the changed files on the target.' : 'Continue runs the backup anyway; Stop leaves the target as it is.';
  return h('div', { class: 'callout violet stack' },
    h('div', { class: 'row between' },
      h('div', null, h('strong', null, `${runKind(r)} waits for your decision · `, runLabel(r)), h('div', { class: 'small muted' }, ago(r.started_at), ' · ', what)),
      h('div', { class: 'row' },
        h('a', { class: 'btn small', href: `#/runs/${r.id}` }, 'Details'),
        h('button', { type: 'button', class: 'small', onclick: guard(() => decide(r.id, false)) }, 'Stop'),
        h('button', { type: 'button', class: 'small primary', onclick: guard(() => decide(r.id, true)) }, 'Continue'))),
    anomalies.length ? h('ul', { class: 'small anomaly-list' }, anomalies.map(a => h('li', null, h('strong', null, ANOMALY_NAMES[a.check] || a.check), ': ', a.message,
      a.sample && a.sample.length ? h('div', { class: 'mono muted break' }, a.sample.join(', ')) : null)))
      : h('div', { class: 'small' }, r.error));
}
// problemCard shows a failed or incomplete run until it is dismissed.
function problemCard(r) {
  return h('div', { class: 'callout ' + (r.status === 'failed' ? 'bad' : 'warn') + ' row between' },
    h('div', null, h('strong', null, `${runKind(r)} #${r.id} · `, runLabel(r), ' '), badge(r.status),
      h('div', { class: 'small' }, ago(r.started_at), ' · ', r.error || (r.stats.failed ? `${r.stats.failed} files failed` : 'finished with problems'))),
    h('div', { class: 'row' },
      h('a', { class: 'btn small', href: `#/runs/${r.id}` }, 'Details'),
      h('button', { type: 'button', class: 'small', onclick: guard(() => retry(r.id)) }, 'Try again'),
      h('button', { type: 'button', class: 'small', onclick: guard(() => dismiss(r.id)) }, 'Dismiss')));
}
// retry starts a failed run again with the same parameters (a delete only
// where it did not finish); the old card goes away.
async function retry(id) {
  const r = await POST(`/api/runs/${id}/retry`);
  toast(`Started again as run #${r.id}`);
  render();
}
function fmtNext(ts) { return ts ? `${fmtTime(ts)} (${ago(ts)})` : ''; }

// ---------- jobs list ----------
async function viewJobs() {
  await loadNodes();
  // Filter box and quick filters sit outside the live box, so typing is not
  // interrupted by refreshes; a change redraws from the last loaded data.
  let cur = null, filter = 'all', query = '', body = null;
  const filters = [['all', 'All', () => true], ['attn', 'Needs you', jobNeedsYou], ['run', 'Running', j => j.targets.some(t => t.active_run)], ['off', 'Disabled', j => !j.enabled]];
  const seg = h('div', { class: 'seg' });
  const count = h('span', { class: 'muted small' });
  const search = h('input', { type: 'search', placeholder: 'Filter jobs, peers, paths…', 'aria-label': 'Filter jobs', oninput: () => { query = search.value.trim().toLowerCase(); fill(body, drawJobs()); } });
  function drawJobs() {
    const { jobs, active } = cur;
    fill(seg, filters.map(([k, label, f]) => h('button', { type: 'button', class: k === filter ? 'active' : '', onclick: () => { filter = k; fill(body, drawJobs()); } },
      label, ' ', h('span', { class: 'muted' }, jobs.filter(f).length))));
    const f = filters.find(x => x[0] === filter)[2];
    const shown = jobs.filter(j => f(j) && (!query || [j.name, j.source_name, j.source_path, ...j.targets.flatMap(t => [t.node_name, t.path])].join(' ').toLowerCase().includes(query)));
    const nt = shown.reduce((n, j) => n + j.targets.length, 0);
    count.textContent = `${shown.length} job${shown.length === 1 ? '' : 's'} · ${nt} target${nt === 1 ? '' : 's'}`;
    const byTarget = new Map(active.filter(a => a.target_id).map(a => [a.target_id, a]));
    const out = [];
    if (active.length) {
      out.push(h('div', { class: 'callout info row between' },
        h('span', null, `${active.length} run${active.length === 1 ? '' : 's'} active`),
        h('button', { type: 'button', class: 'small danger', onclick: guard(() => stopAll(active.length)) }, 'Stop all')));
    }
    if (!jobs.length) out.push(panel(null, h('div', { class: 'empty' }, 'No jobs yet. ', h('a', { href: '#/jobs/new' }, 'Create the first job'), '.')));
    else if (!shown.length) out.push(panel(null, h('div', { class: 'empty' }, 'No jobs match the filter.')));
    else {
      // One masonry per page, refilled on refresh: rebuilding it left the
      // list empty for a moment and the browser jumped to the top.
      const cards = shown.map(j => jobCard(j, byTarget, active.filter(a => a.job_id === j.id && !a.target_id)));
      if (packed) packed.update(cards);
      else packed = masonry(cards, 330, 12);
      out.push(packed.el);
    }
    return out;
  }
  let packed = null;
  onLeave(() => { if (packed) packed.stop(); });
  body = await live(3000, async () => {
    const [jobs, runs] = await Promise.all([GET('/api/jobs'), GET('/api/runs?limit=1')]);
    state.jobs = jobs;
    return { jobs, active: runs.active };
  }, data => { cur = data; return drawJobs(); });
  const toolbar = h('div', { class: 'toolbar' }, search, seg, h('span', { class: 'spacer' }), count);
  return h('div', { class: 'stack' }, pageHead('Backup jobs', null, h('a', { class: 'btn primary', href: '#/jobs/new' }, '+ New job')), toolbar, body);
}

// A target needs the user when a backup waits for a decision or failed and
// was not dismissed yet.
function targetNeedsYou(t) {
  const r = t.last_backup;
  return !!r && (r.status === 'awaiting_approval' || (['failed', 'warning', 'interrupted'].includes(r.status) && !r.acknowledged));
}
const jobNeedsYou = j => j.targets.some(targetNeedsYou);

// masonry lays items out in as many columns as fit (each at least minWidth
// wide) and puts every item into the shortest column, so a tall card does
// not leave a hole next to it the way grid rows do. It lays out again when
// the width changes the column count; update(items) swaps the items at once
// into the same columns (no empty moment, so the page keeps its scroll
// position while it refreshes); stop() ends the watching.
function masonry(items, minWidth, gap) {
  const el = h('div', { class: 'masonry' });
  let cols = 0, boxes = [];
  const place = () => {
    boxes.forEach(b => b.replaceChildren());
    for (const it of items) boxes.reduce((a, b) => b.offsetHeight < a.offsetHeight ? b : a).append(it);
  };
  const lay = () => {
    const n = Math.max(1, Math.floor((el.clientWidth + gap) / (minWidth + gap)));
    if (!el.clientWidth || n === cols) return;
    cols = n;
    boxes = Array.from({ length: n }, () => h('div', { class: 'masonry-col' }));
    el.replaceChildren(...boxes);
    place();
  };
  const ro = new ResizeObserver(lay);
  ro.observe(el);
  return {
    el, stop: () => ro.disconnect(),
    update(next) {
      items = next;
      if (!cols) return; // not laid out yet: the first layout uses the new items
      // place() measures the columns while they refill; that layout would
      // already shrink the page and pull the scroll position up, so the
      // list keeps its height until the cards are back.
      el.style.minHeight = el.offsetHeight + 'px';
      place();
      el.style.minHeight = '';
    },
  };
}

// onlineDot is the small online/offline marker on the job cards.
function onlineDot(online) {
  return h('span', { class: 'odot' + (online ? '' : ' off'), title: online ? 'online' : 'offline' });
}

function jobCard(j, byTarget, jobRuns) {
  const off = !j.enabled;
  const waits = j.targets.some(t => t.last_backup && t.last_backup.status === 'awaiting_approval');
  const failed = !waits && jobNeedsYou(j);
  return h('section', { class: 'panel job-card' + (waits ? ' attn' : failed ? ' fail' : '') + (off ? ' off' : '') },
    h('div', { class: 'job-card-head' },
      h('div', { class: 'grow' },
        h('div', { class: 'row tight' }, h('a', { class: 'job-name', href: `#/jobs/${j.id}` }, j.name),
          h('span', { class: 'tag' }, j.mode === 'sync' ? 'sync' : 'copy'), off ? h('span', { class: 'badge plain' }, 'disabled') : null),
        h('div', { class: 'job-src small muted', title: `compare by ${j.compare_mode}` }, onlineDot(j.source_online), h('span', { class: 'nowrap' }, j.source_name),
          h('span', { class: 'mono ell' }, j.source_path), j.includes.length ? h('span', { class: 'nowrap' }, `· ${j.includes.length} folder${j.includes.length === 1 ? '' : 's'}`) : null)),
      h('a', { class: 'btn small', href: `#/jobs/${j.id}` }, 'Settings')),
    jobRuns.map(a => compactRun(a)),
    j.targets.length ? j.targets.map(t => targetLine(j, t, byTarget.get(t.id)))
      : h('div', { class: 'callout warn small' }, 'This job has no target yet. ', h('a', { href: `#/jobs/${j.id}` }, 'Add one'), '.'));
}

function targetLine(j, t, run) {
  const r = t.last_backup;
  const off = !t.enabled || !j.enabled;
  const drive = isDrive(t.node_id) ? driveOf(t.node_id) : null;
  const next = t.next_backup ? ago(t.next_backup) : t.run_on_connect ? 'when the peer comes online'
    : drive && drive.on_connect ? 'when plugged in' : 'manual only';
  const nextTitle = [t.next_backup && !off ? `next backup: ${fmtTime(t.next_backup)}` : '', t.next_backup && t.run_on_connect && !off ? 'also when the peer comes online' : '',
    `next verify: ${off ? 'off' : t.next_verify ? fmtNext(t.next_verify) : 'manual only'}`].filter(Boolean).join('\n');
  let main;
  if (run) main = h('div', { class: 'row tight' }, h('button', { type: 'button', class: 'small danger', onclick: guard(() => stopRun(run.id)) }, 'Stop'));
  else if (r && r.status === 'awaiting_approval') main = h('div', { class: 'row tight' },
    h('button', { type: 'button', class: 'small', onclick: guard(() => decide(r.id, false)) }, 'Stop'),
    h('button', { type: 'button', class: 'small primary', onclick: guard(() => decide(r.id, true)) }, 'Continue'));
  else main = h('button', { type: 'button', class: 'small', disabled: !!t.active_run, onclick: guard(() => startRun(t.id, 'backup')) }, 'Back up now');
  const problem = r && targetNeedsYou(t) && !run;
  return h('div', { class: 'tline' + (t.enabled ? '' : ' off') },
    h('div', { class: 'tline-top' }, onlineDot(t.node_online), h('strong', { class: 'nowrap' }, t.node_name), h('span', { class: 'mono ell muted', title: t.path }, t.path)),
    h('div', { class: 'row tight nowrap-row' }, main, targetMenuButton(t)),
    run ? compactRun(run) : h('div', { class: 'tline-sub' },
      r ? h('span', { class: 'tline-last' }, badge(r.status), h('a', { class: 'muted', href: `#/runs/${r.id}` }, ago(r.finished_at || r.started_at)))
        : h('span', { class: 'tline-last' }, h('span', { class: 'badge bad' }, 'Never backed up')),
      h('span', { class: 'ell', title: nextTitle }, off ? 'schedule off' : ['next ', next]),
      h('span', { class: 'num' }, fmtBytes(t.stored_bytes))),
    problem ? h('div', { class: 'tline-why small ' + (r.status === 'awaiting_approval' ? 'violet' : r.status === 'failed' ? 'bad' : 'warn') },
      h('span', { class: 'ell' }, problemText(r)), h('a', { href: `#/runs/${r.id}` }, 'Details')) : null);
}

// problemText is the one-line reason a backup waits or failed.
function problemText(r) {
  if (r.status === 'awaiting_approval') {
    const a = ((r.details && r.details.decision) || {}).anomalies || [];
    return a.length ? a.map(x => ANOMALY_NAMES[x.check] || x.check).join(', ') : r.error || 'waits for your decision';
  }
  return r.error || (r.stats && r.stats.failed ? `${fmtNum(r.stats.failed)} files failed` : 'finished with problems');
}

// compactRun is a run as a progress line: bar, percent, phase and ETA.
function compactRun(ar) {
  const p = ar.progress || {};
  const pct = p.percent >= 0 ? p.percent : null;
  const queued = p.phase === 'queued';
  const parts = [queued ? ar.waiting || 'waiting to start' : p.phase || runKind(ar)];
  if (!queued && p.bytes_per_sec) parts.push(`${fmtBytes(p.bytes_per_sec)}/s`);
  if (p.eta_seconds >= 0) parts.push(`${fmtDur(p.eta_seconds)} left`);
  return h('div', { class: 'tline-run' },
    h('div', { class: 'progress-line' }, progressBar(pct == null ? 0 : pct / 100, pct == null && !queued ? 'indet' : ''),
      h('strong', { class: 'pct small' }, pct == null ? '' : pct.toFixed(pct < 10 ? 1 : 0) + ' %')),
    h('div', { class: 'small muted ell' }, h('a', { href: `#/runs/${ar.id}` }, `${runKind(ar)} #${ar.id}`), ' · ', parts.join(' · ')));
}

// The ⋯ menu holds the rarely used actions of a target.
let openMenu = null;
function closeMenu() { if (openMenu) { openMenu.remove(); openMenu = null; document.removeEventListener('mousedown', menuOutside); document.removeEventListener('keydown', menuEsc); } }
function menuOutside(e) { if (openMenu && !openMenu.contains(e.target)) closeMenu(); }
function menuEsc(e) { if (e.key === 'Escape') closeMenu(); }
function targetMenuButton(t) {
  return h('button', { type: 'button', class: 'small more', title: 'More actions', 'aria-label': 'More actions', 'aria-haspopup': 'menu', onclick: e => {
    e.stopPropagation();
    const was = openMenu && openMenu.dataset.target === String(t.id);
    closeMenu();
    if (!was) showTargetMenu(e.currentTarget, t);
  } }, '⋯');
}
function showTargetMenu(btn, t) {
  const busy = !!t.active_run;
  const act = (label, kind, extra = {}) => h('button', { type: 'button', role: 'menuitem', disabled: busy, onclick: guard(async () => {
    closeMenu();
    if (kind === 'rebuild_index' && !await confirmBox('Rebuild index', 'Replace this target’s index with what is on the target disk. Use this after losing the controller database. Checksums are learned by the next deep verify.', { ok: 'Rebuild' })) return;
    await startRun(t.id, kind, extra);
  }) }, label);
  const go = (label, href) => h('a', { role: 'menuitem', href, onclick: closeMenu }, label);
  const job = (findTarget(t.id) || {}).job;
  const sync = job && (job.mode || 'copy') === 'copy'
    ? h('button', { type: 'button', role: 'menuitem', disabled: busy, onclick: () => { closeMenu(); syncDialog(job, t); } }, 'Sync now') : null;
  openMenu = h('div', { class: 'menu', role: 'menu', dataset: { target: String(t.id) } },
    act('Dry run', 'dryrun'), sync, act('Quick verify', 'verify_quick'), act('Deep verify', 'verify_deep'),
    act('Deep verify + repair', 'verify_deep', { repair: true }), act('Rebuild index', 'rebuild_index', { hash: false }),
    h('hr'), go('Browse files', `#/browse/${t.id}`), go('Run history', `#/runs?target=${t.id}`));
  document.body.append(openMenu);
  const r = btn.getBoundingClientRect();
  const w = openMenu.offsetWidth, ht = openMenu.offsetHeight;
  openMenu.style.left = Math.max(8, Math.min(r.right - w, innerWidth - w - 8)) + 'px';
  openMenu.style.top = (r.bottom + 4 + ht > innerHeight ? Math.max(8, r.top - ht - 4) : r.bottom + 4) + 'px';
  document.addEventListener('mousedown', menuOutside);
  document.addEventListener('keydown', menuEsc);
  onLeave(closeMenu);
}

// syncDialog runs one backup of a copy job's target as a sync: files gone
// from the source are removed from this target, to its trash or for good.
function syncDialog(job, t) {
  const how = h('div', { class: 'choice' },
    h('label', null, h('input', { type: 'radio', name: 'syncrm', value: 'trash', checked: true }),
      h('span', null, h('strong', null, 'Move to the trash'), h('span', { class: 'muted small' }, 'Removed files wait in .bfbackup-trash on the target until the trash retention period ends'))),
    h('label', null, h('input', { type: 'radio', name: 'syncrm', value: 'delete' }),
      h('span', null, h('strong', null, 'Delete permanently'), h('span', { class: 'muted small' }, 'Removed files are gone at once'))));
  const note = h('div', { class: 'callout small' });
  const removal = () => $('input[name=syncrm]:checked', how).value;
  let okBtn = null;
  const drawNote = () => {
    const del = removal() === 'delete';
    note.className = 'callout small ' + (del ? 'bad' : 'warn');
    note.textContent = del
      ? 'Files that are not in the source are deleted permanently from this target. This cannot be undone.'
      : 'Files that are not in the source are moved to the trash on this target.';
    if (okBtn) okBtn.textContent = del ? 'Sync and delete permanently' : 'Sync and remove extra files';
  };
  how.addEventListener('change', drawNote);
  const dlg = modal({
    title: 'Sync now',
    body: h('div', { class: 'stack' },
      h('p', null, 'Runs one backup of ', h('strong', null, `${t.node_name || nodeName(t.node_id)}: ${t.path}`),
        ' as a sync: new and changed files are copied, and everything on this target that is no longer in the source is removed. The job stays a copy job. The anomaly checks still stop and ask when much would go.'),
      how, note),
    actions: [{ label: 'Cancel' }, {
      label: 'Sync and remove extra files', kind: 'danger solid', onclick: async () => {
        const r = await POST(`/api/targets/${t.id}/run`, { kind: 'backup', sync: true, sync_removal: removal() });
        toast(`Sync started (run #${r.id})`);
      },
    }],
  });
  okBtn = $$('.modal-foot button', dlg.el).pop();
  drawNote();
}

const SCHEDULES = [
  ['', 'Manual only'],
  ['0 * * * *', 'Every hour'],
  ['0 */6 * * *', 'Every 6 hours'],
  ['0 2 * * *', 'Daily at 02:00'],
  ['0 23 * * *', 'Daily at 23:00'],
  ['0 3 * * 0', 'Weekly, Sunday 03:00'],
  ['0 4 1 * *', 'Monthly, 1st at 04:00'],
];
function scheduleText(s) {
  if (!s) return 'manual';
  const p = SCHEDULES.find(x => x[0] === s);
  return p ? p[1] : s;
}
// Schedule picker: presets plus a custom cron field. Returns [element, getter].
function schedulePicker(value, id) {
  const sel = h('select', { id }, SCHEDULES.map(([v, l]) => h('option', { value: v }, l)), h('option', { value: '__custom' }, 'Custom (cron)…'));
  const custom = h('input', { type: 'text', placeholder: 'min hour day month weekday, e.g. 30 1 * * 1-5', value: value || '' });
  const known = SCHEDULES.some(([v]) => v === (value || ''));
  sel.value = known ? (value || '') : '__custom';
  custom.classList.toggle('hidden', known);
  sel.addEventListener('change', () => custom.classList.toggle('hidden', sel.value !== '__custom'));
  return [h('div', { class: 'stack' }, sel, custom), () => sel.value === '__custom' ? custom.value.trim() : sel.value];
}

// nodeSelect picks a node; with drives, USB drives are offered too (targets).
function nodeSelect(value, { onlineOnly = false, id, drives = false } = {}) {
  const nodes = state.nodes.map(n => h('option', { value: n.id, selected: n.id === value, disabled: onlineOnly && !n.online },
    n.name + (n.online ? '' : ' (offline)')));
  if (!drives || !state.drives.length || !display.features.usb_drives) return h('select', { id }, nodes);
  return h('select', { id }, h('optgroup', { label: 'Servers' }, nodes),
    h('optgroup', { label: 'USB drives' }, state.drives.map(d => h('option', { value: d.node_id, selected: d.node_id === value },
      'USB · ' + d.name + (d.connected ? '' : ' (not connected)')))));
}

// targetFields are the inputs of a new target: node, folder, schedule and
// "when the peer comes online". For a USB drive, the folder is relative to
// the drive and there is no schedule: drives run when plugged in.
function targetFields(jobName) {
  const firstPeer = state.nodes.find(x => !x.local);
  const n = nodeSelect(firstPeer ? firstPeer.id : 'local', { drives: true });
  const p = h('input', { type: 'text', class: 'mono' });
  const [schedEl, schedGet] = schedulePicker('');
  const onConnect = h('input', { type: 'checkbox' });
  const sched = h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Schedule'), schedEl);
  const onRow = h('label', { class: 'check-row' }, onConnect, h('span', null, h('strong', null, 'Also back up when the peer comes online'),
    h('div', { class: 'help' }, 'The target’s peer, or the source’s for pulls. Starts after more than 5 minutes offline.')));
  const usbNote = h('div', { class: 'help' }, 'USB drives have no schedule: they back up when plugged in (drive settings on the USB drives page) or with “Back up now”.');
  let wasDrive = null;
  const sync = () => {
    const drive = isDrive(n.value);
    const name = typeof jobName === 'function' ? jobName() : jobName;
    p.placeholder = drive ? driveFolder(name || 'Photos') : '/mnt/user/Backups/' + (name || 'Photos');
    if (drive !== wasDrive && wasDrive !== null) p.value = drive ? driveFolder(name || 'backup') : '';
    if (drive && !p.value) p.value = driveFolder(name || 'backup');
    wasDrive = drive;
    sched.classList.toggle('hidden', drive);
    onRow.classList.toggle('hidden', drive);
    usbNote.classList.toggle('hidden', !drive);
  };
  n.addEventListener('change', sync);
  sync();
  const browse = h('button', { type: 'button', onclick: async () => { const r = await pickFolder({ node: n.value, start: p.value }); if (r) p.value = r; } }, 'Browse…');
  const el = h('div', { class: 'form-grid' },
    h('label', { class: 'field' }, h('span', null, 'Target node or drive'), n),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Target folder'), h('div', { class: 'input-group' }, p, browse)),
    sched, onRow, usbNote);
  const value = () => {
    const drive = isDrive(n.value);
    return { node_id: n.value, path: p.value.trim(), schedule: drive ? '' : schedGet(), enabled: true, run_on_connect: !drive && onConnect.checked };
  };
  return { el, value, path: p };
}

// Folder picker. With `base`, navigation stays below base and the result is
// relative to it.
function pickFolder({ node, start, base = null, title = 'Choose a folder' }) {
  return new Promise(resolve => {
    let current = start || base || '';
    let picked = false;
    const list = h('div', { class: 'picker-list' });
    const crumbs = h('div', { class: 'crumbs' });
    const load = async () => {
      list.replaceChildren(h('div', { class: 'spinner' }));
      try {
        const res = await GET(`/api/nodes/${encodeURIComponent(node)}/ls?` + q({ path: current }));
        const dirs = res.entries.filter(e => e.t === 3).sort((a, b) => byName(a.n, b.n));
        crumbs.replaceChildren();
        if (current) {
          const parts = current.split('/').filter(Boolean);
          crumbs.append(h('button', { type: 'button', onclick: () => { if (!base) { current = ''; load(); } } }, base ? '' : 'roots'));
          let acc = '';
          parts.forEach(p => {
            acc += '/' + p;
            const target = acc;
            crumbs.append(h('span', { class: 'sep' }, '/'),
              h('button', { type: 'button', onclick: () => { if (!base || target.startsWith(base)) { current = target; load(); } } }, p));
          });
        } else crumbs.append(h('span', { class: 'muted' }, 'Allowed roots on this node'));
        list.replaceChildren(...(dirs.length ? dirs.map(d => h('button', { type: 'button', onclick: () => { current = current ? current.replace(/\/$/, '') + '/' + d.n : d.n; load(); } }, icon('dir'), d.n)) :
          [h('div', { class: 'empty' }, 'No subfolders')]));
      } catch (e) { list.replaceChildren(h('div', { class: 'callout bad' }, e.message)); }
      newRow.classList.toggle('hidden', !current);
    };
    // A new folder inside the open one; it is opened right away.
    const newName = h('input', { type: 'text', placeholder: 'New folder name', autocomplete: 'off' });
    const create = guard(async () => {
      const name = newName.value.trim();
      if (!name || name === '.' || name === '..' || name.includes('/')) { toast('Enter a folder name without "/".', 'bad'); return; }
      const path = current.replace(/\/$/, '') + '/' + name;
      await POST(`/api/nodes/${encodeURIComponent(node)}/mkdir`, { path });
      toast('Folder created');
      newName.value = '';
      current = path;
      await load();
    });
    newName.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); create(); } });
    const newRow = h('div', { class: 'row picker-new hidden' }, newName, h('button', { type: 'button', onclick: create }, 'Create folder'));
    modal({
      title, wide: true,
      body: [h('div', { class: 'muted small' }, 'Node: ', nodeName(node)), crumbs, list, newRow],
      actions: [
        { label: 'Cancel' },
        {
          label: 'Select this folder', kind: 'primary', onclick: () => {
            if (!current) { toast('Open a folder first.', 'bad'); return false; }
            picked = true;
            if (base) {
              const rel = current === base ? '' : current.slice(base.replace(/\/$/, '').length + 1);
              resolve(rel);
            } else resolve(current);
          },
        },
      ],
      onClose: () => { if (!picked) resolve(null); },
    });
    load();
  });
}

// ---------- job editor ----------
async function viewJobEdit(id) {
  await loadNodes();
  const [jobs, settings] = await Promise.all([loadJobs(), GET('/api/settings')]);
  const globalEx = settings.global_excludes.filter(l => !l.startsWith('#'));
  const job = id ? jobs.find(j => j.id === id) : {
    name: '', enabled: true, source_node: 'local', source_path: '', includes: [], excludes: [],
    compare_mode: 'mtime', workers: 4, pre_hook: '', post_hook: '', targets: [],
    mode: 'copy', on_conflict: 'newer', old_copies: 'trash', clean_excluded: true,
  };
  if (!job) throw new Error('Job not found');

  const f = {
    name: h('input', { type: 'text', value: job.name, required: true, maxlength: 100 }),
    enabled: h('input', { type: 'checkbox', checked: job.enabled }),
    node: nodeSelect(job.source_node),
    path: h('input', { type: 'text', value: job.source_path, placeholder: '/mnt/user/Photos', class: 'mono' }),
    excludes: h('textarea', { placeholder: '*.tmp\n.Recycle.Bin/\ncache/' }, (job.excludes || []).join('\n')),
    workers: h('input', { type: 'number', min: 1, max: 32, value: job.workers }),
    useGlobal: h('input', { type: 'checkbox', checked: !job.ignore_global_excludes }),
    clean: h('input', { type: 'checkbox', checked: !!job.clean_excluded }),
    conflict: h('select', null, [['source', 'Always use the source (replace the target’s copy)'], ['newer', 'Replace only if the source is newer'],
      ['keep', 'Always keep the target’s copy (copy only new files)'], ['ask', 'Stop and ask before replacing']].map(([v, l]) => h('option', { value: v, selected: (job.on_conflict || 'newer') === v }, l))),
    old: h('select', null, [['trash', 'Move to the target’s trash (default)'], ['delete', 'Delete them']].map(([v, l]) => h('option', { value: v, selected: (job.old_copies || 'trash') === v }, l))),
    useHooks: h('input', { type: 'checkbox', checked: !!(job.pre_hook || job.post_hook) }),
    pre: h('input', { type: 'text', value: job.pre_hook, placeholder: 'script name, e.g. stop-containers.sh' }),
    post: h('input', { type: 'text', value: job.post_hook, placeholder: 'script name' }),
  };
  let includes = [...(job.includes || [])];
  const chips = h('div', { class: 'chips' });
  const drawChips = () => chips.replaceChildren(...(includes.length ? includes.map((inc, i) =>
    h('span', { class: 'chip' }, inc, h('button', { type: 'button', 'aria-label': 'remove', onclick: () => { includes.splice(i, 1); drawChips(); } }, '✕'))) :
    [h('span', { class: 'muted small' }, 'Everything below the source folder')]));
  drawChips();
  const hookList = h('datalist', { id: 'hooklist' });
  const loadHooks = async () => {
    hookList.replaceChildren();
    try {
      const info = await GET(`/api/nodes/${encodeURIComponent(f.node.value)}/info`);
      (info.hooks || []).forEach(x => hookList.append(h('option', { value: x })));
    } catch (_) { /* offline */ }
  };
  f.pre.setAttribute('list', 'hooklist');
  f.post.setAttribute('list', 'hooklist');
  f.node.addEventListener('change', loadHooks);
  loadHooks();

  const modes = [
    ['size', 'Size only', 'Fastest. Good for media that never changes in place (movies, music).'],
    ['mtime', 'Size + modification time', 'Default. Finds every normal change without reading the files.'],
    ['checksum', 'Checksum (SHA-256)', 'Reads every source file each run. For important data where silent changes matter.'],
  ];
  const modeBox = h('div', { class: 'choice' }, modes.map(([v, t, d]) => h('label', null,
    h('input', { type: 'radio', name: 'mode', value: v, checked: job.compare_mode === v }),
    h('span', null, h('strong', null, t), h('span', { class: 'muted small' }, d)))));

  const jobModes = [
    ['copy', 'Copy', 'Copies new and changed files. Files that disappear from the source stay on the target (listed as “not in source”).'],
    ['sync', 'Sync', 'Like copy, and files that disappear from the source are removed from the target too. Stops and asks when many files would be removed (Settings → Anomaly checks).'],
  ];
  const typeBox = h('div', { class: 'choice' }, jobModes.map(([v, t, d]) => h('label', null,
    h('input', { type: 'radio', name: 'jobmode', value: v, checked: (job.mode || 'copy') === v }),
    h('span', null, h('strong', null, t), h('span', { class: 'muted small' }, d)))));

  const collect = () => ({
    name: f.name.value, enabled: f.enabled.checked, source_node: f.node.value, source_path: f.path.value.trim(),
    includes, excludes: f.excludes.value.split('\n').map(s => s.trim()).filter(Boolean),
    ignore_global_excludes: !f.useGlobal.checked, clean_excluded: f.clean.checked,
    compare_mode: $('input[name=mode]:checked', modeBox).value,
    workers: Number(f.workers.value), mode: $('input[name=jobmode]:checked', typeBox).value,
    on_conflict: f.conflict.value, old_copies: f.old.value,
    pre_hook: f.useHooks.checked ? f.pre.value.trim() : '', post_hook: f.useHooks.checked ? f.post.value.trim() : '',
  });

  // New jobs collect their targets in the form.
  const newTargets = [];
  const newTargetBox = h('div', { class: 'stack' });
  const addNewTargetRow = () => {
    const row = targetFields(() => f.name.value.trim());
    newTargets.push(row);
    newTargetBox.append(h('div', { class: 'callout' }, row.el));
  };

  const save = guard(async () => {
    const body = collect();
    if (!id) {
      body.targets = newTargets.map(t => t.value()).filter(t => t.path);
      const created = await POST('/api/jobs', body);
      toast('Job created');
      location.hash = `#/jobs/${created.id}`;
    } else {
      await PUT(`/api/jobs/${id}`, body);
      toast('Job saved');
      render();
    }
  });

  const hookBox = h('div', { class: 'stack' + (f.useHooks.checked ? '' : ' hidden') },
    h('div', { class: 'help' }, 'Scripts from /config/hooks on the source node. The post-hook gets BFB_RUN_STATUS. A failing pre-hook skips the backup.'),
    h('div', { class: 'form-grid' },
      h('label', { class: 'field' }, h('span', null, 'Before the backup'), f.pre),
      h('label', { class: 'field' }, h('span', null, 'After the backup'), f.post)), hookList);
  f.useHooks.addEventListener('change', () => hookBox.classList.toggle('hidden', !f.useHooks.checked));

  const form = h('div', { class: 'stack' },
    panel('General', h('div', { class: 'form-grid' },
      h('label', { class: 'field' }, h('span', null, 'Name'), f.name),
      h('label', { class: 'check-row' }, f.enabled, h('span', null, h('strong', null, 'Enabled'), h('div', { class: 'help' }, 'Disabled jobs never run on a schedule.'))))),
    panel('Source', h('div', { class: 'stack' },
      h('div', { class: 'form-grid' },
        h('label', { class: 'field' }, h('span', null, 'Node with the files'), f.node,
          h('div', { class: 'help' }, 'Pick a peer here to pull files from it to the target.')),
        h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Source folder'),
          h('div', { class: 'input-group' }, f.path, h('button', { type: 'button', onclick: async () => { const r = await pickFolder({ node: f.node.value, start: f.path.value }); if (r) f.path.value = r; } }, 'Browse…')),
          h('div', { class: 'help' }, 'Paths on the target mirror the paths below this folder. One job per share works best.'))),
      h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Only these folders (optional)'), chips,
        h('button', { type: 'button', class: 'small', onclick: async () => {
          if (!f.path.value.trim()) { toast('Set the source folder first.', 'bad'); return; }
          const r = await pickFolder({ node: f.node.value, base: f.path.value.trim().replace(/\/$/, ''), title: 'Folder to include' });
          if (r !== null && r !== '' && !includes.includes(r)) { includes.push(r); drawChips(); }
        } }, '+ Add folder')),
      h('div', { class: 'field' }, h('div', { class: 'row between' }, h('span', { class: 'field-label' }, 'Exclusions for this job'),
        h('button', { type: 'button', class: 'small', onclick: () => {
          if (!f.path.value.trim()) { toast('Set the source folder first.', 'bad'); return; }
          pickExcludes({ node: f.node.value, base: f.path.value.trim(), add: pat => {
            const lines = f.excludes.value.split('\n').map(s => s.trim()).filter(Boolean);
            if (lines.includes(pat)) { toast(pat + ' is already excluded'); return; }
            lines.push(pat);
            f.excludes.value = lines.join('\n');
            toast('Excluded ' + pat);
          } });
        } }, 'Browse…')), f.excludes,
        h('div', { class: 'help' }, 'One pattern per line. ', h('code', null, '*.tmp'), ' matches a name anywhere, ', h('code', null, 'cache/'), ' only directories, ',
          h('code', null, '/Downloads'), ' only at the top, ', h('code', null, 'a/**/b'), ' any depth. Lines starting with # are comments.')),
      h('div', { class: 'stack' },
        h('label', { class: 'check-row' }, f.useGlobal, h('span', null, h('strong', null, `Also apply the global exclusions (${globalEx.length} patterns)`),
          h('div', { class: 'help' }, 'Junk files like .DS_Store, Thumbs.db and recycle bins. Edit the list in ', h('a', { href: '#/settings' }, 'Settings'), '.'))),
        h('details', null, h('summary', { class: 'small muted' }, 'Show global exclusions'), h('div', { class: 'chips' }, globalEx.map(x => h('span', { class: 'chip plain' }, x)))),
        h('label', { class: 'check-row' }, f.clean, h('span', null, h('strong', null, 'Remove excluded files from the targets')))))),
    panel('Backup type', h('div', { class: 'stack' }, typeBox,
      h('div', { class: 'form-grid' },
        h('label', { class: 'field' }, h('span', null, 'When a changed file already exists on the target'), f.conflict,
          h('div', { class: 'help' }, '“Stop and ask” halts the backup before it changes anything; continue or stop it on the dashboard or the jobs page.')),
        h('label', { class: 'field' }, h('span', null, 'Removed files on the target'), f.old,
          h('div', { class: 'help' }, 'Files a backup removes from the target. The trash is purged after the retention in Settings. Changed files are overwritten in place, without a copy in the trash.'))))),
    panel('Change detection', modeBox),
    panel('Speed', h('div', { class: 'form-grid' },
      h('label', { class: 'field' }, h('span', null, 'Parallel transfers'), f.workers,
        h('div', { class: 'help' }, '2–4 for big files on spinning disks, 8+ for many small files.')))),
    // Hooks switched off in Settings: the block is hidden, saved hooks stay.
    display.features.hooks ? panel('Optional', h('div', { class: 'stack' },
      h('label', { class: 'check-row' }, f.useHooks, h('span', null, h('strong', null, 'Use hooks'))),
      hookBox)) : null);

  if (!id) {
    addNewTargetRow();
    form.append(panel('Targets', h('div', { class: 'stack' }, newTargetBox,
      h('button', { type: 'button', class: 'small', onclick: addNewTargetRow }, '+ Another target'))));
  } else {
    form.append(targetsPanel(job), trashPanel(id));
  }
  form.append(h('div', { class: 'row' },
    h('button', { type: 'button', class: 'primary', onclick: save }, id ? 'Save job' : 'Create job'),
    h('a', { class: 'btn', href: '#/jobs' }, 'Cancel'),
    id ? h('button', { type: 'button', class: 'danger', onclick: guard(async () => {
      if (!await confirmBox('Delete job', `Delete "${job.name}" and its index? Backed-up files stay on the targets; you can re-add them later and rebuild the index.`, { ok: 'Delete job', danger: true })) return;
      await DEL(`/api/jobs/${id}`);
      toast('Job deleted');
      location.hash = '#/jobs';
    }) }, 'Delete job') : null));

  return h('div', { class: 'stack' }, pageHead(id ? job.name : 'New backup job', id ? `${job.source_name} ${job.source_path}` : 'Choose what to back up and where to store it.'), form);
}

function targetsPanel(job) {
  const box = h('div', { class: 'stack' });
  for (const t of job.targets) box.append(targetCard(job, t));
  const nt = targetFields(job.name);
  box.append(h('details', null, h('summary', null, h('strong', null, '+ Add a target')),
    h('div', { class: 'stack callout' },
      nt.el,
      h('div', null, h('button', { type: 'button', class: 'primary', onclick: guard(async () => {
        await POST(`/api/jobs/${job.id}/targets`, nt.value());
        toast('Target added');
        render();
      }) }, 'Add target')))));
  return panel('Targets', box);
}

// The "run when the peer comes online" checkbox. The peer is the target's
// node, or the source node for pulls; with no peer involved it does nothing.
function onConnectRow(box, job, targetNode) {
  const peers = [...new Set([targetNode, job.source_node])].filter(id => id && id !== 'local').map(nodeName);
  return h('label', { class: 'check-row' }, box, h('span', null, h('strong', null, 'Also back up when ', peers.length ? peers.join(' or ') : 'the peer', ' comes online'),
    h('div', { class: 'help' }, peers.length
      ? 'Starts a backup when the peer connects after more than 5 minutes offline, in addition to the schedule. Both sides must be online.'
      : 'Source and target are on the controller, so there is no peer to wait for.')));
}

function targetCard(job, t) {
  const [schedEl, schedGet] = schedulePicker(t.schedule, 's' + t.id);
  const [vEl, vGet] = schedulePicker(t.verify_schedule, 'v' + t.id);
  const vmode = h('select', null, h('option', { value: 'quick', selected: t.verify_mode === 'quick' }, 'Quick (size and time)'),
    h('option', { value: 'deep', selected: t.verify_mode === 'deep' }, 'Deep (re-read and checksum everything)'));
  const enabled = h('input', { type: 'checkbox', checked: t.enabled });
  const onConnect = h('input', { type: 'checkbox', checked: t.run_on_connect });
  return h('div', { class: 'callout stack' },
    h('div', { class: 'row between' },
      h('div', null, h('strong', null, nodeStatusDot(t.node_online), ' ', t.node_name), ' ', h('span', { class: 'mono break' }, t.path),
        h('div', { class: 'small muted' }, `${fmtNum(t.stored_files)} files · ${fmtBytes(t.stored_bytes)}`,
          t.last_backup ? [' · last backup ', ago(t.last_backup.started_at), ' '] : ' · never backed up ',
          t.last_backup ? badge(t.last_backup.status) : null)),
      h('div', { class: 'row' }, t.active_run ? h('a', { class: 'btn small', href: `#/runs/${t.active_run}` }, 'Running…') : null,
        h('a', { class: 'btn small', href: '#/jobs' }, 'Run, verify…'),
        h('a', { class: 'btn small', href: `#/browse/${t.id}` }, 'Browse'), h('a', { class: 'btn small', href: `#/runs?target=${t.id}` }, 'History'))),
    isDrive(t.node_id) ? h('div', { class: 'stack' },
      h('label', { class: 'check-row' }, enabled, h('span', null, 'Enabled')),
      h('div', { class: 'help' }, 'Backs up when the drive is plugged in, as set on the ', h('a', { href: '#/drives' }, 'USB drives'), ' page, or with “Back up now”.'))
      : h('div', { class: 'form-grid' },
        h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Backup schedule'), schedEl),
        h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Verify schedule'), vEl),
        h('label', { class: 'field' }, h('span', null, 'Verify mode'), vmode),
        h('label', { class: 'check-row' }, enabled, h('span', null, 'Enabled')),
        onConnectRow(onConnect, job, t.node_id)),
    h('div', { class: 'row' },
      h('button', { type: 'button', class: 'small', onclick: guard(async () => {
        await PUT(`/api/targets/${t.id}`, isDrive(t.node_id) ? { verify_mode: t.verify_mode, enabled: enabled.checked }
          : { schedule: schedGet(), verify_schedule: vGet(), verify_mode: vmode.value, enabled: enabled.checked, run_on_connect: onConnect.checked });
        toast('Target saved');
        render();
      }) }, 'Save target'),
      h('button', { type: 'button', class: 'small danger', onclick: guard(async () => {
        if (!await confirmBox('Remove target', 'Remove this target and its index? The files on the target are not deleted.', { ok: 'Remove', danger: true })) return;
        await DEL(`/api/targets/${t.id}`);
        toast('Target removed');
        render();
      }) }, 'Remove target')));
}

// ---------- browse / restore / delete ----------
async function viewBrowse(targetId) {
  await loadNodes();
  await loadJobs();
  const ref = findTarget(targetId);
  if (!ref) throw new Error('Target not found');
  const { job, target } = ref;
  const params = new URLSearchParams(location.hash.split('?')[1] || '');
  let tab = params.get('tab') || 'files';
  let dir = '';
  let sortBy = 'size';
  const selected = new Map(); // rel -> {size, isDir}
  const content = h('div');
  const selbar = h('div', { class: 'selbar hidden' });
  const tabs = h('div', { class: 'tabs' });

  const drawSelbar = () => {
    selbar.classList.toggle('hidden', selected.size === 0);
    const bytes = [...selected.values()].reduce((a, s) => a + (s.size || 0), 0);
    selbar.replaceChildren(
      h('strong', null, `${selected.size} selected · ${fmtBytes(bytes)}`),
      h('button', { type: 'button', onclick: () => restoreDialog(job, target, [...selected.keys()]) }, 'Restore…'),
      h('button', { type: 'button', onclick: () => deleteDialog(job, target, [...selected.keys()], tab === 'missing') }, 'Delete…'),
      h('button', { type: 'button', onclick: () => { selected.clear(); draw(); } }, 'Clear'));
    // The "select all" box of each table: on, off, or in between.
    for (const all of $$('thead input[type=checkbox]', content)) {
      const boxes = $$('tbody input[type=checkbox]', all.closest('table'));
      const on = boxes.filter(b => b.checked).length;
      all.checked = boxes.length > 0 && on === boxes.length;
      all.indeterminate = on > 0 && on < boxes.length;
    }
  };
  // allBox selects or clears every row shown in its table.
  // The wanted state is read once: redrawing per row would flip the header
  // box (and so the wanted state) after the first row.
  const allBox = () => h('input', {
    type: 'checkbox', 'aria-label': 'select all shown',
    onchange: e => {
      const want = e.target.checked;
      for (const b of $$('tbody input[type=checkbox]', e.target.closest('table'))) {
        b.checked = want;
        b.select(want);
      }
      drawSelbar();
    },
  });
  const checkbox = (rel, info) => {
    const box = h('input', {
      type: 'checkbox', checked: selected.has(rel), 'aria-label': 'select ' + rel,
      onclick: e => e.stopPropagation(),
      onchange: e => { box.select(e.target.checked); drawSelbar(); },
    });
    box.select = on => { if (on) selected.set(rel, info); else selected.delete(rel); };
    return box;
  };
  const notInSource = f => f.not_in_source ? [' ', h('span', { class: 'badge warn', title: 'Deleted from the source or no longer selected' }, 'not in source')] : null;
  // A folder: all its files gone from the source, or only some.
  const dirNotInSource = it => !it.missing ? null : [' ', h('span', { class: 'badge warn', title: 'Files below this folder that were not seen in the last scan of the source' },
    it.missing >= it.files ? 'not in source' : `${fmtNum(it.missing)} not in source`)];
  const share = (size, total, isDir) => {
    const frac = total > 0 ? size / total : 0;
    const inner = h('div');
    inner.style.width = (frac * 100).toFixed(2) + '%';
    return h('div', { class: 'share-cell' },
      h('div', { class: 'share' + (isDir ? '' : ' file') }, inner),
      h('span', { class: 'share-pct' }, total > 0 ? (frac * 100 < 0.1 && size > 0 ? '<0.1 %' : (frac * 100).toFixed(frac < 0.1 ? 1 : 0) + ' %') : ''));
  };
  // Flat file list for the other tabs.
  const fileRow = f => {
    const rel = f.dir ? f.dir + '/' + f.name : f.name;
    return h('tr', null,
      h('td', { class: 'check' }, checkbox(rel, { size: f.size })),
      h('td', { class: 'break' }, icon(f.type === 2 ? 'link' : 'file'), h('span', { class: 'mono small' }, rel), notInSource(f)),
      h('td', { class: 'num' }, fmtBytes(f.size)),
      h('td', { class: 'small nowrap' }, fmtNano(f.mtime)),
      h('td', { class: 'small nowrap muted' }, fmtTime(f.synced_at)));
  };
  const flatHeaders = () => [h('th', { class: 'check' }, allBox()), 'Path', h('th', { class: 'num' }, 'Size'), 'Modified', 'Backed up'];

  async function drawFiles() {
    const res = await GET(`/api/targets/${targetId}/browse?` + q({ dir }));
    const items = [
      ...res.dirs.map(d => ({ isDir: true, name: d.name, size: d.bytes, files: d.files, missing: d.not_in_source || 0 })),
      ...res.files.map(f => ({ isDir: false, name: f.name, size: f.size, f })),
    ];
    const total = items.reduce((a, i) => a + i.size, 0);
    const fileCount = res.files.length + res.dirs.reduce((a, d) => a + d.files, 0);
    if (sortBy === 'size') items.sort((a, b) => b.size - a.size || byName(a.name, b.name));
    else items.sort((a, b) => (b.isDir - a.isDir) || byName(a.name, b.name));

    const crumbs = h('div', { class: 'crumbs' }, h('button', { type: 'button', onclick: () => { dir = ''; draw(); } }, target.path));
    let acc = '';
    for (const p of dir.split('/').filter(Boolean)) {
      acc = acc ? acc + '/' + p : p;
      const to = acc;
      crumbs.append(h('span', { class: 'sep' }, '/'), h('button', { type: 'button', onclick: () => { dir = to; draw(); } }, p));
    }
    const seg = h('div', { class: 'seg', role: 'group', 'aria-label': 'Sort' },
      ...[['size', 'Size'], ['name', 'Name']].map(([k, l]) => h('button', { type: 'button', class: sortBy === k ? 'active' : '', onclick: () => { sortBy = k; draw(); } }, l)));
    const up = dir ? h('tr', { class: 'click', onclick: () => { dir = dir.includes('/') ? dir.slice(0, dir.lastIndexOf('/')) : ''; draw(); } },
      h('td'), h('td', { class: 'muted' }, '↩ ..'), h('td', { class: 'sm-hide' }), h('td'), h('td', { class: 'sm-hide' }), h('td')) : null;
    const rows = items.map(it => {
      const rel = dir ? dir + '/' + it.name : it.name;
      if (it.isDir) {
        return h('tr', { class: 'click', onclick: () => { dir = rel; draw(); } },
          h('td', { class: 'check' }, checkbox(rel, { isDir: true, size: it.size })),
          h('td', { class: 'break' }, icon('dir'), h('strong', null, it.name), dirNotInSource(it)),
          h('td', { class: 'sm-hide' }, share(it.size, total, true)),
          h('td', { class: 'num' }, fmtBytes(it.size)),
          h('td', { class: 'num muted small sm-hide' }, fmtNum(it.files)),
          h('td', { class: 'small muted' }, ''));
      }
      const f = it.f;
      return h('tr', null,
        h('td', { class: 'check' }, checkbox(rel, { size: f.size })),
        h('td', { class: 'break' }, icon(f.type === 2 ? 'link' : 'file'), f.name, notInSource(f)),
        h('td', { class: 'sm-hide' }, share(f.size, total, false)),
        h('td', { class: 'num' }, fmtBytes(f.size)),
        h('td', { class: 'num muted small sm-hide' }, ''),
        h('td', { class: 'small nowrap muted', title: 'backed up ' + fmtTime(f.synced_at) + (f.hash ? '\nSHA-256 ' + f.hash : '') }, fmtNano(f.mtime)));
    });
    return [
      h('div', { class: 'row between' }, crumbs, h('div', { class: 'row' }, h('span', { class: 'muted small' }, `${fmtBytes(total)} in ${fmtNum(fileCount)} files`), h('span', { class: 'muted small' }, 'Sort by'), seg)),
      h('div', { class: 'panel flush' }, items.length || up ? h('div', { class: 'table-wrap' }, h('table', { class: 'browse' },
        h('thead', null, h('tr', null, h('th', { class: 'check' }, allBox()), h('th', null, 'Name'), h('th', { class: 'share-col sm-hide' }, 'Share of this folder'),
          h('th', { class: 'num' }, 'Size'), h('th', { class: 'num sm-hide' }, 'Files'), h('th', null, 'Modified'))),
        h('tbody', null, up, rows))) : h('div', { class: 'empty' }, 'This folder is empty in the backup.')),
    ];
  }
  let missingOffset = 0;
  async function drawMissing() {
    const res = await GET(`/api/targets/${targetId}/not-in-source?` + q({ limit: 200, offset: missingOffset }));
    return [
      h('div', { class: 'callout info' }, `${fmtNum(res.total)} file(s) are in the backup but were not seen in the last scan of the source (deleted there, or no longer selected). They are kept until you delete them here.`),
      h('div', { class: 'panel flush' }, table(flatHeaders(), res.files.map(fileRow), 'Every backed-up file still exists on the source.')),
      res.total > 200 ? h('div', { class: 'row' },
        h('button', { type: 'button', disabled: missingOffset === 0, onclick: () => { missingOffset = Math.max(0, missingOffset - 200); draw(); } }, '← Previous'),
        h('span', { class: 'muted' }, `${missingOffset + 1}–${Math.min(res.total, missingOffset + 200)} of ${fmtNum(res.total)}`),
        h('button', { type: 'button', disabled: missingOffset + 200 >= res.total, onclick: () => { missingOffset += 200; draw(); } }, 'Next →')) : null,
    ];
  }
  let searchTerm = '';
  async function drawSearch() {
    const input = h('input', { type: 'search', placeholder: 'Part of a file or folder name', value: searchTerm });
    const form = h('form', { class: 'input-group', onsubmit: e => { e.preventDefault(); searchTerm = input.value.trim(); draw(); } }, input, h('button', { class: 'primary' }, 'Search'));
    if (searchTerm.length < 2) return [form];
    const hits = await GET(`/api/targets/${targetId}/search?` + q({ q: searchTerm }));
    return [form, h('div', { class: 'panel flush' }, table(flatHeaders(), hits.map(fileRow), 'No matches.')),
      hits.length >= 500 ? h('div', { class: 'muted small' }, 'Showing the first 500 matches.') : null];
  }
  async function draw() {
    tabs.replaceChildren(...[['files', 'Files'], ['missing', 'Not in source'], ['search', 'Search']].map(([k, l]) =>
      h('button', { type: 'button', class: tab === k ? 'active' : '', onclick: () => { tab = k; selected.clear(); draw(); } }, l)));
    content.replaceChildren(h('div', { class: 'spinner' }));
    try {
      const parts = tab === 'files' ? await drawFiles() : tab === 'missing' ? await drawMissing() : await drawSearch();
      content.replaceChildren(h('div', { class: 'stack' }, parts));
    } catch (e) { content.replaceChildren(h('div', { class: 'callout bad' }, e.message)); }
    drawSelbar();
  }
  draw();
  return h('div', null,
    pageHead('Browse backup', [job.name, ' → ', target.node_name, ' ', h('span', { class: 'mono' }, target.path)],
      h('a', { class: 'btn', href: `#/jobs/${job.id}` }, 'Job settings')),
    tabs, content, selbar);
}

function restoreDialog(job, target, paths) {
  const where = h('div', { class: 'choice' },
    h('label', null, h('input', { type: 'radio', name: 'dest', value: 'orig', checked: true }),
      h('span', null, h('strong', null, 'Original location'), h('span', { class: 'muted small mono' }, `${nodeName(job.source_node)}: ${job.source_path}`))),
    h('label', null, h('input', { type: 'radio', name: 'dest', value: 'other' }),
      h('span', null, h('strong', null, 'Somewhere else'), h('span', { class: 'muted small' }, 'Any node and folder'))));
  const node = nodeSelect(job.source_node);
  const path = h('input', { type: 'text', class: 'mono', placeholder: '/mnt/user/Restore' });
  const other = h('div', { class: 'form-grid hidden' },
    h('label', { class: 'field' }, h('span', null, 'Node'), node),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Folder'), h('div', { class: 'input-group' }, path,
      h('button', { type: 'button', onclick: async () => { const r = await pickFolder({ node: node.value, start: path.value }); if (r) path.value = r; } }, 'Browse…'))));
  where.addEventListener('change', () => other.classList.toggle('hidden', $('input[name=dest]:checked', where).value !== 'other'));
  const overwrite = h('input', { type: 'checkbox' });
  modal({
    title: `Restore ${paths.length} item${paths.length === 1 ? '' : 's'}`, wide: true,
    body: h('div', { class: 'stack' },
      h('div', { class: 'small mono muted break' }, paths.slice(0, 8).join(', '), paths.length > 8 ? ` … and ${paths.length - 8} more` : ''),
      where, other,
      h('label', { class: 'check-row' }, overwrite, h('span', null, h('strong', null, 'Also copy files that already look identical'),
        h('div', { class: 'help' }, 'Without this, existing files with the same size and time are skipped (fast, incremental). Existing files that differ are always moved to the trash before they are replaced.')))),
    actions: [{ label: 'Cancel' }, {
      label: 'Start restore', kind: 'primary', onclick: async () => {
        const toOther = $('input[name=dest]:checked', where).value === 'other';
        if (toOther && !path.value.trim()) { toast('Choose a destination folder.', 'bad'); return false; }
        const r = await POST(`/api/targets/${target.id}/restore`, {
          paths, overwrite: overwrite.checked,
          dest_node: toOther ? node.value : '', dest_path: toOther ? path.value.trim() : '',
        });
        toast(`Restore started (run #${r.id})`);
        location.hash = `#/runs/${r.id}`;
      },
    }],
  });
}

function deleteDialog(job, target, paths, missingTab) {
  const scope = h('div', { class: 'choice' },
    h('label', null, h('input', { type: 'radio', name: 'scope', value: 'target', checked: true }),
      h('span', null, h('strong', null, 'From this backup only'), h('span', { class: 'muted small' }, `${target.node_name}: ${target.path}`))),
    job.targets.length > 1 ? h('label', null, h('input', { type: 'radio', name: 'scope', value: 'targets' }),
      h('span', null, h('strong', null, 'From all backups of this job'), h('span', { class: 'muted small' }, `${job.targets.length} targets`))) : null,
    h('label', null, h('input', { type: 'radio', name: 'scope', value: 'all' }),
      h('span', null, h('strong', null, 'Everywhere'), h('span', { class: 'muted small' }, 'Source and all backups of this job'))));
  const word = h('input', { type: 'text', placeholder: 'delete', autocomplete: 'off' });
  const permanent = h('input', { type: 'checkbox' });
  const trashNote = h('div', { class: 'callout warn small' });
  const syncNote = () => {
    trashNote.className = 'callout small ' + (permanent.checked ? 'bad' : 'warn');
    trashNote.textContent = permanent.checked
      ? 'Deleted permanently: the items are removed at once on each side and cannot be restored from the trash.'
      : 'Deleted items are moved to the .bfbackup-trash folder on each side and removed for good after the trash retention period (Settings).';
  };
  permanent.addEventListener('change', syncNote);
  syncNote();
  modal({
    title: `Delete ${paths.length} item${paths.length === 1 ? '' : 's'}`, wide: true,
    body: h('div', { class: 'stack' },
      h('div', { class: 'small mono muted break' }, paths.slice(0, 8).join(', '), paths.length > 8 ? ` … and ${paths.length - 8} more` : ''),
      missingTab ? h('div', { class: 'callout info small' }, 'These files are already gone from the source.') : null,
      scope,
      h('label', { class: 'check-row' }, permanent, h('span', null, h('strong', null, 'Delete permanently (skip the trash)'),
        h('div', { class: 'help' }, 'Frees the space at once. There is no way to get the items back.'))),
      trashNote,
      h('label', { class: 'field' }, h('span', null, 'Type "delete" to confirm'), word)),
    actions: [{ label: 'Cancel' }, {
      label: 'Delete', kind: 'danger solid', onclick: async () => {
        if (word.value.trim() !== 'delete') { toast('Type "delete" to confirm.', 'bad'); return false; }
        const s = $('input[name=scope]:checked', scope).value;
        const r = await POST(`/api/jobs/${job.id}/delete`, {
          paths, confirm: 'delete', from_source: s === 'all', permanent: permanent.checked,
          target_ids: s === 'target' ? [target.id] : [],
        });
        toast(`Delete started (run #${r.id})`);
        location.hash = `#/runs/${r.id}`;
      },
    }],
  });
}

// ---------- history ----------
async function viewRuns() {
  await loadNodes();
  await loadJobs();
  const params = new URLSearchParams(location.hash.split('?')[1] || '');
  const filter = { kind: params.get('kind') || '', status: params.get('status') || '', target: params.get('target') || '', node: params.get('node') || '', offset: 0, limit: 50 };
  const driveSel = state.drives.length && display.features.usb_drives ? h('select', null, h('option', { value: '' }, 'All drives'),
    state.drives.map(d => h('option', { value: d.node_id, selected: filter.node === d.node_id }, 'USB · ' + d.name))) : null;
  const kindSel = h('select', null, h('option', { value: '' }, 'All kinds'), Object.entries(KIND).map(([k, l]) => h('option', { value: k, selected: filter.kind === k }, l)));
  const statusSel = h('select', null, h('option', { value: '' }, 'All statuses'), Object.entries(STATUS).map(([k, [, l]]) => h('option', { value: k, selected: filter.status === k }, l)));
  const targetSel = h('select', null, h('option', { value: '' }, 'All targets'),
    state.jobs.flatMap(j => j.targets.map(t => h('option', { value: String(t.id), selected: filter.target === String(t.id) }, `${j.name} → ${t.node_name}`))));
  const box = h('div');
  const load = async () => {
    const res = await GET('/api/runs?' + q(filter));
    const rows = res.runs.map(r => h('tr', { class: 'click', onclick: () => { location.hash = `#/runs/${r.id}`; } },
      h('td', { class: 'num' }, '#' + r.id), h('td', null, runKind(r)), h('td', null, runLabel(r)), h('td', null, badge(r.status)),
      h('td', { class: 'small nowrap' }, fmtTime(r.started_at)), h('td', { class: 'small nowrap' }, runDuration(r)),
      h('td', { class: 'num' }, r.stats.transferred ? `${fmtNum(r.stats.transferred)} · ${fmtBytes(r.stats.transferred_bytes)}` : ''),
      h('td', { class: 'small muted' }, r.trigger)));
    box.replaceChildren(h('div', { class: 'panel flush cards-sm runs-cards' }, table(['#', 'Kind', 'Job', 'Status', 'Started', 'Duration', h('th', { class: 'num' }, 'Copied'), 'Trigger'], rows, 'No runs match.')),
      h('div', { class: 'row', style: null },
        h('button', { type: 'button', disabled: filter.offset === 0, onclick: () => { filter.offset = Math.max(0, filter.offset - filter.limit); load(); } }, '← Newer'),
        h('span', { class: 'muted' }, res.total ? `${filter.offset + 1}–${Math.min(res.total, filter.offset + filter.limit)} of ${fmtNum(res.total)}` : ''),
        h('button', { type: 'button', disabled: filter.offset + filter.limit >= res.total, onclick: () => { filter.offset += filter.limit; load(); } }, 'Older →')));
  };
  for (const [sel, key] of [[kindSel, 'kind'], [statusSel, 'status'], [targetSel, 'target'], [driveSel, 'node']]) {
    if (sel) sel.addEventListener('change', guard(async () => { filter[key] = sel.value; filter.offset = 0; await load(); }));
  }
  await load();
  every(5000, () => { if (filter.offset === 0) load().catch(() => {}); });
  return h('div', { class: 'stack' }, pageHead('Backup history', 'Every backup, verify, restore and delete.'),
    h('div', { class: 'row filters' }, kindSel, statusSel, targetSel, driveSel), box);
}

const STAT_LABELS = {
  scanned: 'Files scanned', scanned_bytes: ['Size scanned', fmtBytes], new: 'New files', new_bytes: ['New data', fmtBytes],
  changed: 'Changed files', changed_bytes: ['Changed data', fmtBytes], unchanged: 'Unchanged', not_in_source: 'Not in source',
  adopted: 'Already on the target (taken over)', gone: 'Gone from the source since the last backup', kept: 'Kept on the target (not replaced)', removed: 'Removed from the target (sync)',
  transferred: 'Copied files', transferred_bytes: ['Copied data', fmtBytes], failed: 'Failed', skipped: 'Skipped',
  checked: 'Checked', missing: 'Missing', mismatched: 'Damaged / changed', untracked: 'Unknown files on target', repaired: 'Removed from index',
  trashed: 'Moved to trash', deleted: 'Deleted permanently', indexed: 'Index entries', target_free: ['Target free', fmtBytes], target_total: ['Target size', fmtBytes],
};

async function viewRun(id) {
  await loadNodes();
  await loadJobs();
  const body = await live(2000, () => GET(`/api/runs/${id}`), res => {
    const r = res.run;
    const d = r.details || {};
    const out = [];
    const actions = [];
    if (res.progress) actions.push(h('button', { type: 'button', class: 'danger', onclick: guard(async () => { await POST(`/api/runs/${id}/cancel`); toast('Cancelling…'); }) }, 'Cancel run'));
    if (r.status === 'awaiting_approval') actions.push(h('button', { type: 'button', onclick: guard(() => decide(id, false)) }, 'Stop'),
      h('button', { type: 'button', class: 'primary', onclick: guard(() => decide(id, true)) }, 'Continue'));
    actions.push(h('a', { class: 'btn', href: `#/logs?run=${id}` }, 'Log'));
    out.push(pageHead(`${runKind(r)} #${r.id}`, [runLabel(r), ' · ', r.trigger, ' · started ', fmtTime(r.started_at), ' · ', runDuration(r)], badge(r.status), actions));
    if (r.status === 'awaiting_approval') out.push(decisionCard(r));
    else if (r.error) out.push(h('div', { class: 'callout ' + (r.status === 'cancelled' || r.status === 'declined' ? '' : 'bad') }, r.error));
    if (d.anomalies && r.status !== 'awaiting_approval') out.push(panel('Anomaly checks (dry run)', h('ul', { class: 'small anomaly-list' },
      d.anomalies.map(a => h('li', null, h('strong', null, ANOMALY_NAMES[a.check] || a.check), ': ', a.message)))));
    if (d.removals && r.kind === 'dryrun') out.push(h('div', { class: 'callout info' }, `A backup would remove ${fmtNum(d.removals)} files from the target (list below).`));
    if (d.message && d.message !== r.error) out.push(h('div', { class: 'callout warn' }, d.message));
    if (res.progress) out.push(panel('Progress', runCard({ ...r, progress: res.progress }, false)));
    const phases = (res.progress && res.progress.phases) || d.phases;
    if (phases && phases.length) out.push(panel('Steps', phaseTimeline(phases)));
    const kv = Object.entries(STAT_LABELS).filter(([k]) => r.stats[k]).map(([k, l]) => {
      const [label, f] = Array.isArray(l) ? l : [l, fmtNum];
      return h('div', null, h('div', { class: 'k' }, label), h('div', { class: 'v' }, f(r.stats[k])));
    });
    if (kv.length) out.push(panel('Numbers', h('div', { class: 'kv' }, kv)));
    if (d.plan_sample && d.plan_sample.length) {
      out.push(panel(r.kind === 'dryrun' ? 'What a backup would copy' : 'Files that would be copied',
        table(['Path', 'Reason', h('th', { class: 'num' }, 'Size')], d.plan_sample.map(p => h('tr', null,
          h('td', { class: 'mono small break' }, p.path), h('td', null, h('span', { class: 'badge ' + (p.reason === 'changed' ? 'warn' : 'info') }, p.reason)), h('td', { class: 'num' }, fmtBytes(p.size))))),
        d.plan_sample.length >= 500 ? h('span', { class: 'muted small' }, 'first 500 shown') : null));
    }
    if (d.removal_sample && d.removal_sample.length) {
      out.push(panel(`${r.kind === 'dryrun' ? 'What a backup would remove' : 'Files that would be removed'} (${fmtNum(d.removals || d.removal_sample.length)})`,
        table(['Path', 'Reason'], d.removal_sample.map(p => h('tr', null,
          h('td', { class: 'mono small break' }, p.path), h('td', null, h('span', { class: 'badge ' + (p.reason === 'excluded' ? 'plain' : 'warn') }, p.reason === 'excluded' ? 'matches the exclusions' : 'gone from the source'))))),
        d.removal_sample.length < (d.removals || 0) ? h('span', { class: 'muted small' }, `first ${d.removal_sample.length} shown`) : null));
    }
    if (r.stats.untracked) {
      const list = d.untracked_sample || [];
      out.push(panel(`Unknown files on the target (${fmtNum(r.stats.untracked)})`, h('div', { class: 'stack' },
        list.length ? h('div', { class: 'mono small break' }, list.map(p => h('div', null, p))) : null,
        r.stats.untracked > list.length ? h('div', { class: 'muted small' }, `first ${list.length} shown, all of them in the file log below`) : null)));
    }
    if (d.restore) out.push(panel('Restore', h('div', { class: 'small' }, 'To ', d.restore.dest_path ? h('span', { class: 'mono' }, `${nodeName(d.restore.dest_node)}: ${d.restore.dest_path}`) : 'the original location',
      d.restore.overwrite ? ' (identical files copied too)' : '',
      h('div', { class: 'mono muted break' }, (d.restore.paths || []).join(', ')))));
    if (d.delete) out.push(panel('Deleted paths', h('div', { class: 'mono small break' }, (d.delete.paths || []).join(', '))));
    for (const k of ['pre_hook_output', 'post_hook_output']) {
      if (d[k]) out.push(panel(k === 'pre_hook_output' ? 'Pre-hook output' : 'Post-hook output', h('pre', { class: 'output' }, d[k])));
    }
    if (res.errors.length) {
      // Where: only when the errors know their node (e.g. source or target of a delete).
      const where = res.errors.some(e => e.node);
      out.push(panel(`Problems (${fmtNum(res.errors.length)}${res.errors.length >= 1000 ? '+' : ''})`, table(where ? ['Where', 'Path', 'Message'] : ['Path', 'Message'],
        res.errors.map(e => h('tr', null, where ? h('td', { class: 'small nowrap' }, h('strong', null, e.node ? nodeName(e.node) : '–')) : null,
          h('td', { class: 'mono small break' }, e.path), h('td', { class: 'small' }, e.message))))));
    }
    return out;
  });
  const files = runFileLog({ id }, true);
  await files.load();
  return h('div', { class: 'stack' }, body, files.el);
}

// ---------- logs ----------
const LOG_LEVEL = { 1: ['bad', 'Error'], 2: ['info', 'Event'], 4: ['plain', 'Detail'] };
const FILE_LEVEL = { 1: ['bad', 'Error'], 2: ['warn', 'Issue'], 3: ['ok', 'OK'], 4: ['plain', 'Detail'] };
const LOG_LEVEL_SETTING = { 1: 'Errors only', 2: 'Errors, events and file issues', 3: 'Also every file that was changed', 4: 'Every step and decision, also unchanged files' };
const LOG_KIND = { ...KIND, peer: 'Peer', schedule: 'Scheduler', system: 'System', audit: 'Audit', drive: 'USB drive' };

function fmtTimeMs(ms) {
  return fmtDateTimeOf(new Date(ms), true);
}
function logNodes(e) {
  if (!e.node && !e.src_node) return '';
  if (!e.src_node) return nodeName(e.node);
  if (!e.node) return nodeName(e.src_node);
  return `${nodeName(e.src_node)} → ${nodeName(e.node)}`;
}

// logBrowser is a filterable, sortable, paged log table. opt: api, levels,
// cols ([key, label]; key null = message), filters (which selects to show),
// fixed (filter values that cannot change, e.g. a run), live (refresh).
function logBrowser(opt) {
  const params = new URLSearchParams(location.hash.split('?')[1] || '');
  const f = { level: '', kind: '', node: '', job: '', run: '', q: '', hours: '', sort: 'ts', dir: 'desc', offset: 0, limit: 100 };
  for (const k of opt.filters) f[k] = params.get(k) || '';
  if (!opt.fixed) f.run = params.get('run') || '';
  Object.assign(f, opt.fixed || {}, opt.order || {});
  const box = h('div', { class: 'stack' });
  const select = (key, all, opts) => {
    const s = h('select', null, h('option', { value: '' }, all),
      opts.map(([val, l]) => h('option', { value: String(val), selected: f[key] === String(val) }, l)));
    s.addEventListener('change', guard(async () => { f[key] = s.value; f.offset = 0; await load(); }));
    return s;
  };
  const search = h('input', { type: 'search', placeholder: opt.searchText || 'Search message', value: f.q });
  let timer = null;
  search.addEventListener('input', () => {
    clearTimeout(timer);
    timer = setTimeout(guard(async () => { f.q = search.value.trim(); f.offset = 0; await load(); }), 350);
  });
  onLeave(() => clearTimeout(timer));
  const selects = {
    level: () => select('level', opt.levelAll || 'All levels', Object.entries(opt.levels).map(([k, [, l]]) => [k, l === 'OK' || l === 'Detail' ? l : l + 's'])),
    kind: () => select('kind', 'All kinds', Object.entries(LOG_KIND)),
    node: () => select('node', 'All peers', state.nodes.map(n => [n.id, n.name])),
    job: () => select('job', 'All jobs', state.jobs.map(j => [j.id, j.name])),
    hours: () => select('hours', 'Any time', [[1, 'Last hour'], [24, 'Last 24 hours'], [168, 'Last 7 days'], [720, 'Last 30 days']]),
  };
  const filters = h('div', { class: 'row filters' }, opt.filters.map(k => selects[k]()), search);

  const header = () => opt.cols.map(([key, label]) => {
    if (!key) return h('th', null, label);
    const on = f.sort === key;
    return h('th', {
      class: 'sort' + (on ? ' on' : ''), title: 'Sort by ' + label.toLowerCase(), onclick: guard(async () => {
        if (on) f.dir = f.dir === 'asc' ? 'desc' : 'asc';
        else { f.sort = key; f.dir = key === 'ts' ? 'desc' : 'asc'; }
        f.offset = 0;
        await load();
      }),
    }, label, on ? (f.dir === 'asc' ? ' ▲' : ' ▼') : '');
  });
  const jobName = id => { const j = state.jobs.find(j => j.id === id); return j ? j.name : (id ? `job #${id}` : ''); };
  const cell = {
    ts: e => h('td', { class: 'small nowrap' }, fmtTimeMs(e.ts)),
    level: e => { const [cls, label] = opt.levels[e.level] || ['', e.level]; return h('td', null, h('span', { class: 'badge ' + cls }, label)); },
    kind: e => h('td', { class: 'small nowrap' }, LOG_KIND[e.kind] || e.kind),
    node: e => h('td', { class: 'small nowrap' }, logNodes(e)),
    job: e => h('td', { class: 'small' }, jobName(e.job_id)),
    path: e => h('td', { class: 'mono small break wide' }, e.path || ''),
    run: e => h('td', { class: 'small nowrap' }, e.run_id ? h('a', { href: `#/runs/${e.run_id}` }, '#' + e.run_id) : ''),
    msg: e => h('td', { class: 'small break wide' }, e.message),
  };

  const load = async () => {
    const res = await GET(opt.api + '?' + q(f));
    const rows = res.entries.map(e => h('tr', { class: 'lvl-' + e.level }, opt.cols.map(([key]) => cell[key || 'msg'](e))));
    fill(box,
      opt.info ? opt.info(res, f, load) : null,
      h('div', { class: 'panel flush' }, rows.length
        ? h('div', { class: 'table-wrap' }, h('table', { class: 'log-table cards-sm' }, h('thead', null, h('tr', null, header())), h('tbody', null, rows)))
        : h('div', { class: 'empty' }, opt.empty || 'No log entries match.')),
      res.total > f.limit ? h('div', { class: 'row' },
        h('button', { type: 'button', disabled: f.offset === 0, onclick: guard(async () => { f.offset = Math.max(0, f.offset - f.limit); await load(); }) }, '← Previous'),
        h('span', { class: 'muted' }, `${fmtNum(f.offset + 1)}–${fmtNum(Math.min(res.total, f.offset + f.limit))} of ${fmtNum(res.total)}`),
        h('button', { type: 'button', disabled: f.offset + f.limit >= res.total, onclick: guard(async () => { f.offset += f.limit; await load(); }) }, 'Next →'))
        : null);
  };
  // New lines arrive at the top (or the bottom, in time order), so only
  // refresh while the newest are in view.
  if (opt.live) every(opt.live, () => { if (f.offset === 0 && f.sort === 'ts' && (opt.live && f.dir === (opt.order ? opt.order.dir : 'desc'))) load().catch(() => {}); });
  return { el: h('div', { class: 'stack' }, filters, box), load };
}

async function viewLogs() {
  await loadNodes();
  await loadJobs();
  const b = logBrowser({
    api: '/api/logs', levels: LOG_LEVEL, filters: ['level', 'kind', 'node', 'job', 'hours'], live: 5000,
    cols: [['ts', 'Time'], ['level', 'Level'], ['kind', 'Kind'], ['node', 'Peer'], ['job', 'Job'], [null, 'Message'], ['run', 'Run']],
    info: (res, f, load) => h('div', { class: 'row between small muted' },
      h('span', null, 'Logging level: ', h('strong', null, `${res.level} – ${LOG_LEVEL_SETTING[res.level] || ''}`), ' · ', h('a', { href: '#/settings' }, 'change in Settings')),
      f.run ? h('span', null, `Showing run #${f.run} only · `, h('button', { type: 'button', class: 'link', onclick: guard(async () => { f.run = ''; f.offset = 0; await load(); }) }, 'show all runs')) : null),
  });
  await b.load();
  return h('div', { class: 'stack' }, pageHead('Logs', 'Errors and events of runs, peers and the system. Each run’s file log is on its page. Click a column to sort.'), b.el);
}

// runFileLog is the file log of one run, for the bottom of its page.
function runFileLog(run, live) {
  const b = logBrowser({
    api: '/api/filelogs', levels: FILE_LEVEL, filters: ['level'], levelAll: 'All results', fixed: { run: String(run.id) }, order: { sort: 'ts', dir: 'desc' },
    searchText: 'Search path or message', live: live ? 3000 : 0, empty: 'No file lines for this run.',
    cols: [['ts', 'Time'], ['level', 'Result'], ['node', 'Peer'], ['path', 'Path'], [null, 'Message']],
    info: res => res.level < 4 ? h('div', { class: 'small muted' }, `Level ${res.level}: ${res.level < 3 ? 'files that went fine are logged from level 3, ' : ''}unchanged files and each decision from level 4. `, h('a', { href: '#/settings' }, 'Change in Settings')) : null,
  });
  return { el: panel('File log', b.el), load: b.load };
}

// ---------- peers ----------
function editPeer(n, done) {
  const name = h('input', { type: 'text', value: n.name });
  const notify = h('input', { type: 'checkbox', checked: n.offline_notify });
  const hours = h('input', { type: 'number', min: 1, max: 720, value: n.offline_hours || 1 });
  const sync = () => { hours.disabled = !notify.checked; };
  notify.addEventListener('change', sync);
  sync();
  modal({
    title: 'Edit peer',
    body: h('div', { class: 'stack' },
      h('label', { class: 'field' }, h('span', null, 'Name'), name),
      h('label', { class: 'check-row' }, notify, h('span', null, h('strong', null, 'Notify when this peer is offline'),
        h('div', { class: 'help' }, 'One notification per offline period, and another when it is back. Turn it off for backup servers that are often switched off.'))),
      h('label', { class: 'field' }, h('span', null, 'Notify after (hours offline)'), hours, h('div', { class: 'help' }, '1 to 720 hours (30 days).'))),
    actions: [{ label: 'Cancel' }, {
      label: 'Save', kind: 'primary', onclick: async () => {
        await api('PATCH', `/api/nodes/${n.id}`, { name: name.value, offline_notify: notify.checked, offline_hours: +hours.value });
        toast('Peer saved');
        done();
      },
    }],
  });
}

async function viewNodes() {
  const box = h('div', { class: 'stack' });
  const draw = async () => {
    const nodes = await GET('/api/nodes/usage');
    state.nodes = nodes;
    const sum = (f) => nodes.reduce((a, n) => a + n.disks.reduce((b, d) => b + d[f], 0), 0);
    const total = sum('total'), used = sum('used'), free = sum('free');
    const peers = nodes.filter(n => !n.local);
    const stale = nodes.filter(n => n.disks_stale).length;
    box.replaceChildren(
      h('div', { class: 'grid tiles' },
        tile('Online', `${nodes.filter(n => n.online).length} / ${nodes.length}`, `${peers.length} peer${peers.length === 1 ? '' : 's'} + controller`),
        tile('Capacity', fmtBytes(total), stale ? `incl. ${stale} node${stale === 1 ? '' : 's'} last known` : 'all nodes'),
        tile('Used', fmtBytes(used), total ? `${(used / total * 100).toFixed(0)} %` : ''),
        tile('Free', fmtBytes(free), total ? `${(free / total * 100).toFixed(0)} %` : ''),
        tile('Backups stored', fmtBytes(nodes.reduce((a, n) => a + n.backup_bytes, 0)), fmtNum(nodes.reduce((a, n) => a + n.backup_files, 0)) + ' files')),
      h('div', { class: 'panel flush' }, table(['Name', 'Status', 'Disks', 'Backups stored here', 'Version', ''], nodes.map(n => h('tr', null,
        h('td', null, h('strong', null, n.name), n.local ? null : h('div', { class: 'mono small muted' }, n.id),
          n.hostname ? h('div', { class: 'small muted' }, 'host ', n.hostname) : null),
        h('td', null, n.online ? h('span', { class: 'badge ok' }, 'online') : h('span', { class: 'badge bad' }, 'offline'),
          n.addr ? h('div', { class: 'small muted mono' }, n.addr) : null,
          n.transport === 'plain' ? h('div', { class: 'badge warn', title: 'Older peer version or BFB_TRANSPORT=plain' }, 'unencrypted') : null,
          !n.local && !n.online ? h('div', { class: 'small muted' }, 'last seen ', ago(n.last_seen)) : null,
          n.local ? null : h('div', { class: 'small muted' }, n.offline_notify ? `alert after ${n.offline_hours} h offline` : 'no offline alert')),
        h('td', { class: 'disks-cell' }, n.disks.length ? [
          n.disks_stale ? h('div', { class: 'small muted' }, `Last known, ${ago(n.disks_at)}`, n.disk_error ? ` (${n.disk_error})` : '') : null,
          n.disks.map(diskBar)] :
          h('span', { class: 'muted small' }, n.online ? (n.disk_error || 'no disks reported') : 'offline')),
        h('td', { class: 'nowrap' }, n.backup_files ? [fmtBytes(n.backup_bytes), h('div', { class: 'small muted' }, fmtNum(n.backup_files) + ' files')] : h('span', { class: 'muted' }, '–')),
        h('td', { class: 'small' }, n.version || '–', n.version && state.meta && n.version !== state.meta.version ? h('div', { class: 'badge warn' }, 'differs from controller') : null),
        h('td', { class: 'right nowrap' },
          display.features.hooks ? [h('button', { type: 'button', class: 'small', disabled: !n.online, onclick: guard(() => nodeInfo(n)) }, 'Hooks'), ' '] : null,
          n.local ? null : [
            h('button', { type: 'button', class: 'small', onclick: () => editPeer(n, draw) }, 'Edit'), ' ',
            h('button', { type: 'button', class: 'small', disabled: !n.online, title: 'Drop the connection; the peer connects again within seconds. Use it when a peer hangs. Its running transfers fail.', onclick: guard(async () => {
              if (!await confirmBox('Reconnect peer', `Drop the connection to "${n.name}"? It connects again within seconds; runs using it now lose their current transfers.`, { ok: 'Reconnect' })) return;
              await POST(`/api/nodes/${n.id}/reconnect`);
              toast('Connection dropped – the peer reconnects');
              draw();
            }) }, 'Reconnect'), ' ',
            h('button', { type: 'button', class: 'small', onclick: guard(async () => {
              if (!await confirmBox('New token', 'The current token stops working immediately and the peer disconnects. Update BFB_TOKEN on the peer afterwards.', { ok: 'Create new token', danger: true })) return;
              const r = await POST(`/api/nodes/${n.id}/token`);
              showToken(n.name, r.token);
              draw();
            }) }, 'New token'), ' ',
            h('button', { type: 'button', class: 'small danger', onclick: guard(async () => {
              if (!await confirmBox('Remove peer', `Remove "${n.name}"? It must not be used by any job.`, { ok: 'Remove', danger: true })) return;
              await DEL(`/api/nodes/${n.id}`);
              toast('Peer removed');
              draw();
            }) }, 'Remove')]))))));
  };
  await draw();
  every(10000, () => draw().catch(() => {}));
  const addPeer = guard(async () => {
    const name = h('input', { type: 'text', placeholder: 'e.g. colo or storage-server' });
    modal({
      title: 'Add a peer', body: h('label', { class: 'field' }, h('span', null, 'Name'), name, h('div', { class: 'help' }, 'You get a token to put into the peer container.')),
      actions: [{ label: 'Cancel' }, {
        label: 'Add peer', kind: 'primary', onclick: async () => {
          const r = await POST('/api/nodes', { name: name.value });
          showToken(r.node.name, r.token);
          draw();
        },
      }],
    });
  });
  return h('div', { class: 'stack' }, pageHead('Peers', 'Servers that send or receive backups. Peers connect to this controller on port 7701.',
    h('button', { type: 'button', class: 'primary', onclick: addPeer }, '+ Add peer')), box);
}

// ---------- USB drives ----------
async function viewDrives() {
  if (!display.features.usb_drives) {
    return h('div', { class: 'stack' }, pageHead('USB drives'),
      panel(null, h('div', { class: 'empty' }, 'USB drive backups are switched off. Switch them on in ',
        h('a', { href: '#/settings' }, 'Settings → Additional features'), '.')));
  }
  await loadJobs();
  const body = await live(5000, () => GET('/api/drives'), v => {
    state.drives = v.drives;
    const out = [];
    if (v.found.length) out.push(panel('New drives', h('div', { class: 'stack' }, v.found.map(foundRow))));
    if (!v.drives.length) {
      out.push(panel(null, h('div', { class: 'empty' }, v.found.length ? 'Set up a drive above to use it as a backup target.'
        : 'No USB drives yet. Plug a drive in and mount it with Unassigned Devices; it shows up here within a few seconds.')));
    } else out.push(h('div', { class: 'stack' }, v.drives.map(driveCard)));
    return out;
  });
  return h('div', { class: 'stack' }, pageHead('USB drives', 'Offline backups on drives you plug in, back up and store away. Unassigned Devices mounts them below /mnt/disks.'), body);
}

function foundRow(f) {
  const note = f.foreign ? h('span', { class: 'badge warn' }, `belongs to another BFBackup${f.name ? ` (“${f.name}”)` : ''}`)
    : f.problem ? h('span', { class: 'badge bad', title: f.problem }, f.problem)
    : f.known ? h('span', { class: 'badge info' }, `set up here before as “${f.name}”`) : null;
  return h('div', { class: 'callout row between' },
    h('div', null, h('strong', null, f.label), ' ', h('span', { class: 'mono small muted' }, f.path),
      h('div', { class: 'small muted' }, `${f.fstype || 'unknown file system'} · ${fmtBytes(f.free)} free of ${fmtBytes(f.total)}`), note),
    h('button', { type: 'button', class: 'primary', disabled: f.foreign || !!f.problem, onclick: () => setupDrive(f) }, 'Set up…'));
}

// setupDrive initializes a found drive: name, jobs to add, behavior.
function setupDrive(f) {
  const name = h('input', { type: 'text', value: f.name || f.label, maxlength: 60 });
  const jobs = [...state.jobs].filter(j => !isDrive(j.source_node)).sort((a, b) => byName(a.name, b.name));
  const boxes = jobs.map(j => ({ j, box: h('input', { type: 'checkbox', checked: true }) }));
  const start = h('input', { type: 'checkbox', checked: true });
  const onConnect = h('input', { type: 'checkbox', checked: true });
  const verify = h('input', { type: 'checkbox' });
  const overdue = h('input', { type: 'number', min: 0, max: 3650, value: 30 });
  modal({
    title: 'Set up USB drive', wide: true,
    body: h('div', { class: 'stack' },
      h('div', { class: 'muted small' }, h('span', { class: 'mono' }, f.path), ` · ${f.fstype || 'unknown'} · ${fmtBytes(f.total)}`),
      f.fs_warning ? h('div', { class: 'callout warn small' }, f.fs_warning) : null,
      f.known ? h('div', { class: 'callout info small' }, 'This drive was set up here before. It keeps its ID; files already on it are taken over instead of copied again.') : null,
      h('label', { class: 'field' }, h('span', null, 'Name'), name, h('div', { class: 'help' }, 'Write it on the drive, e.g. “Offsite-A”. BFBackup recognizes the drive by a small file (.bfbackup-drive.json) at its root, not by its label.')),
      h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Back up these jobs to the drive'),
        jobs.length ? h('div', { class: 'stack tight' }, boxes.map(({ j, box }) => h('label', { class: 'check-row' }, box,
          h('span', null, h('strong', null, j.name), ' ', h('span', { class: 'small muted' }, `${j.source_name} ${j.source_path} → `, h('span', { class: 'mono' }, driveFolder(j.name)))))))
          : h('div', { class: 'muted small' }, 'No jobs yet; you can add the drive to jobs later.')),
      h('label', { class: 'check-row' }, start, h('span', null, h('strong', null, 'Back up now'))),
      h('label', { class: 'check-row' }, onConnect, h('span', null, h('strong', null, 'Back up automatically when the drive is plugged in'))),
      h('label', { class: 'check-row' }, verify, h('span', null, h('strong', null, 'Quick verify after each backup'))),
      h('label', { class: 'field' }, h('span', null, 'Remind me when the drive was not connected for (days)'), overdue, h('div', { class: 'help' }, '0 = never.'))),
    actions: [{ label: 'Cancel' }, {
      label: 'Set up drive', kind: 'primary', onclick: async () => {
        const r = await POST('/api/drives', { path: f.path, name: name.value, job_ids: boxes.filter(b => b.box.checked).map(b => b.j.id),
          start: start.checked, on_connect: onConnect.checked, verify_after: verify.checked, overdue_days: Number(overdue.value) || 0 });
        if (r.warning) toast(r.warning, 'bad'); else toast(`Drive ${r.drive.name} set up`);
        render();
      },
    }],
  });
}

function driveCard(d) {
  const lastSeen = d.connected ? null : d.last_seen ? `last seen ${ago(d.last_seen)}` : 'never seen since setup';
  const status = d.connected
    ? [h('span', { class: 'badge ok' }, 'connected'), d.busy ? h('span', { class: 'badge info' }, 'in use') : h('span', { class: 'badge plain', title: 'No run uses the drive. Unmount it in Unassigned Devices before you unplug it.' }, 'safe to unmount')]
    : [h('span', { class: 'badge plain' }, 'not connected'), d.overdue ? h('span', { class: 'badge warn' }, 'overdue') : null];
  const frac = d.total ? (d.total - d.free) / d.total : 0;
  const rows = d.targets.map(t => {
    const r = t.last_backup;
    // data-label names the column where phones show the rows as cards.
    return h('tr', null,
      h('td', { class: 'wide' }, h('a', { href: `#/jobs/${t.job_id}` }, t.job_name), t.enabled ? null : h('span', { class: 'badge plain' }, 'disabled'),
        h('div', { class: 'small muted' }, t.source_name, ' ', h('span', { class: 'mono' }, t.source_path))),
      h('td', { class: 'mono small wide', 'data-label': 'Folder' }, t.path),
      h('td', { class: 'nowrap', 'data-label': 'Stored' }, fmtBytes(t.stored_bytes), h('div', { class: 'small muted' }, fmtNum(t.stored_files) + ' files')),
      h('td', { class: 'nowrap', 'data-label': 'Backup' }, t.active_run ? h('a', { href: `#/runs/${t.active_run}` }, 'running…')
        : r ? [badge(r.status), ' ', h('a', { class: 'small muted', href: `#/runs/${r.id}` }, ago(r.finished_at || r.started_at))] : h('span', { class: 'badge bad' }, 'never'),
        t.last_success && r && t.last_success.id !== r.id ? h('div', { class: 'small muted' }, 'last success ', ago(t.last_success.finished_at)) : null),
      h('td', { class: 'nowrap small', 'data-label': 'Verify' }, t.last_verify ? [badge(t.last_verify.status), ' ', ago(t.last_verify.finished_at || t.last_verify.started_at)] : h('span', { class: 'muted' }, '–')),
      h('td', { class: 'right nowrap' }, h('a', { class: 'btn small', href: `#/browse/${t.target_id}` }, 'Browse')));
  });
  const run = (kind, verify) => guard(async () => {
    const r = await POST(`/api/drives/${d.uuid}/run`, { kind, verify });
    toast(r.queued ? `${r.queued} run${r.queued === 1 ? '' : 's'} queued` : 'Nothing to run (no enabled jobs, or their source is offline)');
  });
  return h('section', { class: 'panel drive' + (d.overdue ? ' overdue' : '') },
    h('div', { class: 'panel-head' },
      h('div', null, h('h2', null, d.name), h('div', { class: 'row tight small' }, status,
        h('span', { class: 'muted' }, d.connected ? h('span', { class: 'mono' }, d.mount) : lastSeen))),
      h('div', { class: 'row' },
        h('button', { type: 'button', class: 'primary small', disabled: !d.connected || d.busy || !d.targets.length, onclick: run('backup', d.verify_after) }, 'Back up all now'),
        h('button', { type: 'button', class: 'small', disabled: !d.connected || d.busy || !d.targets.length, onclick: run('verify_quick', false) }, 'Verify all'),
        h('button', { type: 'button', class: 'small', onclick: () => addDriveJobs(d) }, 'Add jobs…'),
        h('a', { class: 'btn small', href: `#/runs?node=${encodeURIComponent(d.node_id)}` }, 'History'),
        h('button', { type: 'button', class: 'small', onclick: () => editDrive(d) }, 'Settings'))),
    h('div', { class: 'stack' },
      d.total ? h('div', { class: 'disk' },
        h('div', { class: 'row between small' }, h('span', { class: 'muted' }, (d.fstype || 'unknown') + (d.connected ? '' : ' · last known')),
          h('span', { class: 'muted nowrap' }, `${fmtBytes(d.free)} free of ${fmtBytes(d.total)}`)),
        progressBar(frac, frac > 0.9 ? 'bad' : frac > 0.8 ? 'warn' : '')) : null,
      d.fs_warning ? h('div', { class: 'callout warn small' }, d.fs_warning) : null,
      h('div', { class: 'small muted' }, `${d.targets.length} job${d.targets.length === 1 ? '' : 's'} · ${fmtNum(d.stored_files)} files · ${fmtBytes(d.stored_bytes)}`,
        d.last_backup ? ` · last backup ${ago(d.last_backup)}` : ' · no backup yet',
        ' · ', d.on_connect ? 'backs up when plugged in' : 'manual backups', d.verify_after ? ', then verifies' : '',
        d.overdue_days ? ` · reminder after ${d.overdue_days} days away` : ''),
      d.targets.length ? h('div', { class: 'table-wrap' }, h('table', { class: 'cards-sm drive-jobs' },
        h('thead', null, h('tr', null, ['Job', 'Folder on the drive', 'Stored', 'Last backup', 'Last verify', ''].map(x => h('th', null, x)))),
        h('tbody', null, rows)))
        : h('div', { class: 'empty' }, 'No jobs on this drive yet. ', h('button', { type: 'button', class: 'link', onclick: () => addDriveJobs(d) }, 'Add jobs'))));
}

// addDriveJobs adds the drive as a target to more jobs.
function addDriveJobs(d) {
  const have = new Set(d.targets.map(t => t.job_id));
  const jobs = state.jobs.filter(j => !have.has(j.id) && !isDrive(j.source_node)).sort((a, b) => byName(a.name, b.name));
  const boxes = jobs.map(j => ({ j, box: h('input', { type: 'checkbox' }) }));
  modal({
    title: `Add jobs to ${d.name}`,
    body: jobs.length ? h('div', { class: 'stack tight' }, boxes.map(({ j, box }) => h('label', { class: 'check-row' }, box,
      h('span', null, h('strong', null, j.name), ' → ', h('span', { class: 'mono small' }, driveFolder(j.name))))))
      : h('div', { class: 'muted' }, 'Every job is on this drive already.'),
    actions: [{ label: 'Cancel' }, jobs.length ? {
      label: 'Add', kind: 'primary', onclick: async () => {
        const sel = boxes.filter(b => b.box.checked);
        for (const { j } of sel) await POST(`/api/jobs/${j.id}/targets`, { node_id: d.node_id, path: driveFolder(j.name), enabled: true });
        toast(`${sel.length} job${sel.length === 1 ? '' : 's'} added`);
        render();
      },
    } : null].filter(Boolean),
  });
}

function editDrive(d) {
  const name = h('input', { type: 'text', value: d.name, maxlength: 60 });
  const onConnect = h('input', { type: 'checkbox', checked: d.on_connect });
  const verify = h('input', { type: 'checkbox', checked: d.verify_after });
  const overdue = h('input', { type: 'number', min: 0, max: 3650, value: d.overdue_days });
  const dlg = modal({
    title: `USB drive ${d.name}`,
    body: h('div', { class: 'stack' },
      h('label', { class: 'field' }, h('span', null, 'Name'), name),
      h('label', { class: 'check-row' }, onConnect, h('span', null, h('strong', null, 'Back up automatically when the drive is plugged in'))),
      h('label', { class: 'check-row' }, verify, h('span', null, h('strong', null, 'Quick verify after each backup'))),
      h('label', { class: 'field' }, h('span', null, 'Remind me when the drive was not connected for (days)'), overdue, h('div', { class: 'help' }, '0 = never.')),
      h('div', { class: 'small muted' }, 'ID ', h('span', { class: 'mono' }, d.uuid), ` · set up ${fmtTime(d.created_at)}`),
      h('hr'),
      h('div', null, h('button', { type: 'button', class: 'danger small', onclick: guard(async () => {
        if (!await confirmBox('Forget drive', `Forget "${d.name}"? Its ${d.targets.length} target(s) and their index are removed from BFBackup. The files on the drive are not touched; plug it in again to set it up anew.`, { ok: 'Forget drive', danger: true })) return;
        await DEL(`/api/drives/${d.uuid}`);
        dlg.close();
        toast('Drive forgotten');
        render();
      }) }, 'Forget drive…'))),
    actions: [{ label: 'Cancel' }, {
      label: 'Save', kind: 'primary', onclick: async () => {
        await api('PATCH', `/api/drives/${d.uuid}`, { name: name.value, on_connect: onConnect.checked, verify_after: verify.checked, overdue_days: Number(overdue.value) || 0 });
        toast('Drive saved');
        render();
      },
    }],
  });
}

function showToken(name, token) {
  const host = location.hostname;
  const envs = `BFB_ROLE=peer\nBFB_CONTROLLER=${host}:7701\nBFB_TOKEN=${token}`;
  modal({
    title: `Token for ${name}`, wide: true,
    body: h('div', { class: 'stack' },
      h('div', { class: 'callout warn' }, 'Shown only once. Anyone with this token can act as this peer.'),
      h('div', null, h('div', { class: 'row between' }, h('strong', null, 'Token'), copyButton(token)), h('div', { class: 'token' }, token)),
      h('div', null, h('div', { class: 'row between' }, h('strong', null, 'Container variables'), copyButton(envs)), h('pre', { class: 'output token' }, envs)),
      h('div', { class: 'help' }, 'On Unraid, use the "bfbackup-peer" template and paste the token. Mount the folders the peer may use below /mnt in the container. Check that the controller address is reachable from the peer.')),
    actions: [{ label: 'Done', kind: 'primary' }],
  });
}

async function nodeInfo(n) {
  const info = await GET(`/api/nodes/${encodeURIComponent(n.id)}/info`);
  modal({
    title: n.name, wide: true,
    body: h('div', { class: 'stack' },
      h('div', { class: 'muted' }, `Host ${info.hostname} · version ${info.version}`),
      h('h3', null, 'Allowed roots'),
      ...info.roots.map(r => h('div', { class: 'stack' },
        h('div', { class: 'row between' }, h('span', { class: 'mono' }, r.path), h('span', { class: 'small muted' }, r.total ? `${fmtBytes(r.free)} free of ${fmtBytes(r.total)}` : '')),
        r.total ? progressBar(1 - r.free / r.total, r.free / r.total < 0.1 ? 'bad' : r.free / r.total < 0.2 ? 'warn' : '') : null)),
      h('h3', null, 'Hook scripts'),
      info.hooks && info.hooks.length ? h('div', { class: 'chips' }, info.hooks.map(x => h('span', { class: 'chip' }, x))) :
        h('div', { class: 'muted small' }, 'None. Put executable scripts into /config/hooks on this node.')),
    actions: [{ label: 'Close' }],
  });
}

// ---------- statistics ----------
async function viewStats() {
  await loadNodes();
  const jobs = await loadJobs();
  const days = await GET('/api/stats?days=30');
  const targets = jobs.flatMap(j => j.targets.map(t => ({ j, t })));
  const forecasts = await Promise.all(targets.map(({ t }) => GET(`/api/targets/${t.id}/forecast`).catch(() => null)));
  const total = days.reduce((a, d) => a + d.bytes, 0);
  return h('div', { class: 'stack' }, pageHead('Statistics', 'Backups and restores of the last 30 days.'),
    h('div', { class: 'grid tiles' },
      tile('Copied (30 days)', fmtBytes(total), fmtNum(days.reduce((a, d) => a + d.files, 0)) + ' files'),
      tile('Runs (30 days)', fmtNum(days.reduce((a, d) => a + d.runs, 0)), `${fmtNum(days.reduce((a, d) => a + d.failed, 0))} failed`),
      tile('Average per day', fmtBytes(total / 30))),
    panel('Data copied per day', barChart(days)),
    panel('Targets and space', targets.length ? h('div', { class: 'grid two' }, targets.map(({ j, t }, i) => {
      const f = forecasts[i];
      const used = f && f.total ? 1 - f.free / f.total : 0;
      return h('div', { class: 'callout stack' },
        h('div', null, h('strong', null, j.name), ' → ', t.node_name, h('div', { class: 'mono small muted break' }, t.path)),
        f && f.total ? [progressBar(used, used > 0.9 ? 'bad' : used > 0.8 ? 'warn' : ''),
          h('div', { class: 'small' }, `${fmtBytes(f.free)} free of ${fmtBytes(f.total)}`, f.live ? '' : ' (from the last backup, node offline)')] :
          h('div', { class: 'small muted' }, 'Free space unknown (node offline, never backed up).'),
        h('div', { class: 'kv' },
          h('div', null, h('div', { class: 'k' }, 'This backup'), h('div', { class: 'v' }, fmtBytes(t.stored_bytes))),
          h('div', null, h('div', { class: 'k' }, 'Files'), h('div', { class: 'v' }, fmtNum(t.stored_files))),
          h('div', null, h('div', { class: 'k' }, 'Copied per day'), h('div', { class: 'v' }, f ? fmtBytes(f.copied_per_day) : '–')),
          h('div', null, h('div', { class: 'k' }, 'Full in'), h('div', { class: 'v' }, f && f.days_until_full >= 0 ? (f.days_until_full > 3650 ? '> 10 years' : `~${Math.round(f.days_until_full)} days`) : '–'))));
    })) : h('div', { class: 'empty' }, 'No targets yet.')));
}

function barChart(days) {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = (tag, attrs, ...kids) => {
    const el = document.createElementNS(NS, tag);
    for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, v);
    kids.forEach(k => el.append(k instanceof Node ? k : document.createTextNode(k)));
    return el;
  };
  const map = new Map(days.map(d => [d.day, d]));
  const list = [];
  for (let i = 29; i >= 0; i--) {
    const d = new Date(Date.now() - i * 86400000);
    const key = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
    list.push(map.get(key) || { day: key, bytes: 0, files: 0, runs: 0 });
  }
  const max = Math.max(1, ...list.map(d => d.bytes));
  const W = matchMedia('(max-width: 800px)').matches ? 380 : 900, H = 180, pad = 24, bw = (W - pad) / list.length;
  const root = svg('svg', { viewBox: `0 0 ${W} ${H}`, class: 'chart', role: 'img', 'aria-label': 'Data copied per day' });
  root.append(svg('line', { x1: pad, x2: W, y1: H - 20, y2: H - 20 }));
  root.append(svg('text', { x: 0, y: 12 }, fmtBytes(max)));
  list.forEach((d, i) => {
    const bh = d.bytes / max * (H - 40);
    const r = svg('rect', { class: 'bar-r', x: pad + i * bw + 2, y: H - 20 - bh, width: Math.max(1, bw - 4), height: Math.max(0, bh), rx: 2 });
    r.append(svg('title', null, `${fmtDay(d.day)}: ${fmtBytes(d.bytes)}, ${fmtNum(d.files)} files, ${d.runs} runs`));
    root.append(r);
    if (i % 5 === 0) root.append(svg('text', { x: pad + i * bw, y: H - 5 }, fmtDayShort(d.day)));
  });
  return root;
}

// ---------- trash ----------
const TRASH_STATUS = { offline: 'Node offline', old_peer: 'Update this peer to see its trash' };
const TRASH_BATCHES_SHOWN = 50;
const plural = (n, one, many) => `${fmtNum(n)} ${n === 1 ? one : many}`;

// trashPanel shows the .bfbackup-trash below each job folder, grouped by
// node: of all jobs, or only the folders of job jobId. Counting walks the
// trash on every node, so it loads after the page and on Refresh; folded, it
// loads when first opened.
function trashPanel(jobId, folded = false) {
  const box = h('div', { class: 'stack' });
  const load = async () => {
    fill(box, h('div', { class: 'spinner' }));
    try {
      if (!state.nodes.length) await loadNodes();
      const t = await GET('/api/trash' + (jobId ? `?job=${jobId}` : ''));
      fill(box, drawTrash(t, jobId, load));
    } catch (e) {
      fill(box, h('div', { class: 'callout bad' }, 'Could not count the trash: ' + e.message));
    }
  };
  const refresh = h('button', { type: 'button', class: 'small', onclick: guard(load) }, 'Refresh');
  if (folded) return foldPanel('Trash', h('div', { class: 'stack' }, box, h('div', null, refresh)), load);
  load();
  return panel('Trash', box, refresh);
}

function drawTrash(t, jobId, reload) {
  const sum = (rs, k) => rs.reduce((n, r) => n + r[k], 0);
  const byNode = new Map();
  for (const r of t.roots) {
    if (!byNode.has(r.node)) byNode.set(r.node, []);
    byNode.get(r.node).push(r);
  }
  const empty = r => guard(async () => {
    if (!await confirmBox('Empty trash', `Remove everything in the trash of ${r.root} on ${nodeName(r.node)} (${plural(r.files, 'file', 'files')}, ${fmtBytes(r.bytes)}) for good? This cannot be undone.`,
      { ok: 'Empty trash', danger: true, word: 'empty' })) return;
    const res = await POST('/api/trash/empty', { node: r.node, root: r.root, confirm: 'empty' });
    toast(`Trash emptied: ${plural(res.batches_removed, 'batch', 'batches')} removed`);
    reload();
  });
  const uses = r => r.uses.map((u, i) => [i ? ', ' : '',
    jobId ? (u.role === 'source' ? 'Source' : 'Target') : [h('a', { href: `#/jobs/${u.job_id}` }, u.job_name), ` (${u.role})`]]);
  const batches = r => {
    const newest = r.batches.slice().reverse();
    return h('details', null, h('summary', { class: 'small' }, plural(r.batches.length, 'batch', 'batches')),
      h('div', { class: 'small stack' }, newest.slice(0, TRASH_BATCHES_SHOWN).map(b => h('div', null,
        fmtTime(b.time), ' · ', b.run_id ? h('a', { href: `#/runs/${b.run_id}` }, `Run #${b.run_id}`) : h('span', { class: 'muted' }, 'no run'),
        ` · ${plural(b.files, 'file', 'files')} · ${fmtBytes(b.bytes)}`)),
        newest.length > TRASH_BATCHES_SHOWN ? h('div', { class: 'muted' }, `… and ${fmtNum(newest.length - TRASH_BATCHES_SHOWN)} older batches`) : null));
  };
  const card = r => {
    const ok = r.status === 'ok';
    const facts = ok
      ? (r.files ? [`${plural(r.files, 'file', 'files')} · ${fmtBytes(r.bytes)}`, h('div', { class: 'small muted' }, `oldest ${fmtTime(r.oldest)}, removed ${fmtTime(r.purge_at)}`)] : h('span', { class: 'muted' }, 'empty'))
      : h('span', { class: r.status === 'error' ? 'text-bad' : 'muted' }, r.status === 'error' ? 'Could not count: ' + r.error : TRASH_STATUS[r.status] || r.status);
    return h('div', { class: 'callout stack' },
      h('div', { class: 'row between' },
        h('div', null, h('div', { class: 'mono break' }, r.root), h('div', { class: 'small muted' }, uses(r))),
        h('div', { class: 'row' }, h('div', { class: 'right' }, facts),
          ok && r.batches.length ? h('button', { type: 'button', class: 'small danger', onclick: empty(r) }, 'Empty') : null)),
      ok && r.batches.length ? batches(r) : null);
  };
  const ok = t.roots.filter(r => r.status === 'ok');
  return [
    h('div', { class: 'help' }, `Removed files wait in .bfbackup-trash below each folder for ${t.retention_days} days; the daily maintenance then removes them. `,
      `Total: ${plural(sum(ok, 'files'), 'file', 'files')}, ${fmtBytes(sum(ok, 'bytes'))}. Counted ${fmtClock(t.checked_at)}.`),
    [...byNode].map(([node, rs]) => {
      const n = state.nodes.find(x => x.id === node);
      const counted = rs.filter(r => r.status === 'ok');
      return h('div', { class: 'stack' },
        h('div', { class: 'row between' }, h('strong', null, nodeStatusDot(n && n.online), ' ', nodeName(node)),
          h('span', { class: 'small muted' }, counted.length ? `${plural(sum(counted, 'files'), 'file', 'files')} · ${fmtBytes(sum(counted, 'bytes'))}` : '–')),
        rs.map(card));
    }),
  ];
}

// ---------- settings ----------
async function viewSettings() {
  const s = await GET('/api/settings');
  const meta = state.meta || await GET('/api/meta');
  const num = (v, min, max) => h('input', { type: 'number', min, max, value: v });
  const f = {
    user: h('input', { type: 'text', value: s.pushover_user, autocomplete: 'off' }),
    token: h('input', { type: 'password', autocomplete: 'new-password', placeholder: s.pushover_token_set ? '•••••••• saved – leave empty to keep' : 'application token' }),
    clear: h('input', { type: 'checkbox' }),
    mailHost: h('input', { type: 'text', value: s.email_host, placeholder: 'smtp.example.com', autocomplete: 'off' }),
    mailPort: num(s.email_port, 1, 65535),
    mailUser: h('input', { type: 'text', value: s.email_user, autocomplete: 'off' }),
    mailPass: h('input', { type: 'password', autocomplete: 'new-password', placeholder: s.email_password_set ? '•••••••• saved – leave empty to keep' : 'password' }),
    mailClear: h('input', { type: 'checkbox' }),
    mailFrom: h('input', { type: 'text', value: s.email_from, placeholder: 'empty = the user name', autocomplete: 'off' }),
    mailTo: h('input', { type: 'text', value: s.email_to.join(', '), placeholder: 'me@example.com, other@example.com', autocomplete: 'off' }),
    trash: num(s.trash_retention_days, 1, 3650), runs: num(s.run_retention_days, 7, 3650), par: num(s.max_parallel_runs, 1, 8),
    low: num(s.low_space_pct, 0, 50), keep: num(s.db_backup_keep, 1, 100), hour: num(s.maintenance_hour, 0, 23),
    stall: num(s.stall_minutes, 0, 1440),
    global: h('textarea', { class: 'tall' }, s.global_excludes.join('\n')),
    logLevel: h('select', null, Object.entries(LOG_LEVEL_SETTING).map(([k, l]) => h('option', { value: k, selected: s.log_level === +k }, `${k} – ${l}`))),
    logDays: num(s.log_retention_days, 1, 3650), logMax: num(s.log_max_entries, 10000, 100000000),
    date: h('select', null, Object.entries(DATE_FORMATS).map(([k, l]) => h('option', { value: k, selected: s.date_format === k }, `${fmtDateTimeOf(new Date(), false, k)}  (${l})`))),
    usb: h('input', { type: 'checkbox', checked: s.features.usb_drives }),
    hooks: h('input', { type: 'checkbox', checked: s.features.hooks }),
  };
  // Appearance: a click shows it at once; leaving without saving goes back.
  let theme = s.theme;
  const themeSeg = h('div', { class: 'seg' });
  const drawTheme = () => fill(themeSeg, [['auto', 'Auto'], ['light', 'Light'], ['dark', 'Dark']].map(([k, l]) =>
    h('button', { type: 'button', class: k === theme ? 'active' : '', onclick: () => { theme = k; setTheme(k); drawTheme(); } }, l)));
  drawTheme();
  onLeave(() => setTheme(state.meta.theme || 'auto'));
  const inUse = s.features_in_use || { usb_drives: [], hooks: [] };
  const inUseNote = (names, what) => names.length ? h('div', { class: 'help' }, `In use: ${names.length} ${what} (${names.join(', ')}).`) : null;
  // Anomaly checks: [key, title, help, [[value field, label, min, max, step]…]]
  const g = { ...s.guards };
  const GUARDS = [
    ['history', 'Compare with earlier backups', 'Learns what is normal for each target from its last 10 backups (needs at least 3). Stops when far more files changed or disappeared than usual.',
      [['history_factor', 'more than … times the usual', 1.5, 100, 0.5], ['history_min', 'and at least … files', 1, 1e9, 1]]],
    ['deletions', 'Many files would be removed', 'Stops before a sync, or removing excluded files, would take many files off a target.',
      [['delete_pct', '… % of the backed-up files', 0, 100, 1], ['delete_min', 'but at least … files', 1, 1e9, 1]]],
    ['delete_count', 'Many files would be removed (count)', 'Stops when more than this many files would be removed from a target, however large it is. With both removal checks on, the lower limit counts.',
      [['delete_count_max', 'more than … files', 1, 1e9, 1]]],
    ['encryption', 'Files look encrypted', 'Reads the start of a sample of changed files. Stops when many no longer match their type (a .jpg that is not a JPEG, a text file full of random bytes): typical for ransomware.',
      [['encrypt_sample', 'files to sample', 5, 500, 1], ['encrypt_pct', 'stop at … % suspicious', 1, 100, 1]]],
    ['extensions', 'Suspicious renames', 'Stops when many files get a new extension (a.docx → a.docx.locked) or a new extension appears on many files.',
      [['ext_min', 'at least … files', 1, 1e9, 1]]],
    ['shrink', 'Source much smaller', 'Stops when the source holds much less data than at the last backup: a disk that is not mounted, a wiped folder.',
      [['shrink_pct', '… % smaller', 1, 99, 1]]],
    ['truncation', 'Files became empty', 'Stops when many changed files are now 0 bytes.',
      [['trunc_min', 'at least … files', 1, 1e9, 1]]],
    ['time_jump', 'Times in the future', 'Stops when many files have a modification time more than a day in the future (wrong clock, tampering).',
      [['time_jump_min', 'at least … files', 1, 1e9, 1]]],
    ['fixed', 'Fixed limit for changed files', 'The old check: stops when more than a fixed share of the backed-up files changed.',
      [['fixed_pct', '… % of the backed-up files', 1, 100, 1], ['fixed_min', 'but at least … files', 1, 1e9, 1]]],
  ];
  const guardInputs = {};
  const guardRows = GUARDS.map(([key, title, help, fields]) => {
    const on = h('input', { type: 'checkbox', checked: !!g[key] });
    guardInputs[key] = on;
    const nums = fields.map(([k, label, min, max, step]) => {
      const inp = h('input', { type: 'number', min, max, step, value: g[k] });
      guardInputs[k] = inp;
      return h('label', { class: 'field inline' }, h('span', { class: 'small' }, label), inp);
    });
    return h('div', { class: 'guard-row' },
      h('label', { class: 'check-row' }, on, h('span', null, h('strong', null, title), h('div', { class: 'help' }, help))),
      h('div', { class: 'row' }, nums));
  });
  const collectGuards = () => {
    const out = {};
    for (const [key, , , fields] of GUARDS) {
      out[key] = guardInputs[key].checked;
      for (const [k] of fields) out[k] = +guardInputs[k].value;
    }
    return out;
  };
  const events = meta.notify_kinds.map(k => h('label', { class: 'check-row' },
    h('input', { type: 'checkbox', value: k.Kind, checked: s.notify_events.includes(k.Kind) }), h('span', null, k.Label)));
  const collect = () => ({
    pushover_user: f.user.value, pushover_token: f.token.value, clear_pushover_token: f.clear.checked,
    email_host: f.mailHost.value, email_port: +f.mailPort.value, email_user: f.mailUser.value,
    email_password: f.mailPass.value, clear_email_password: f.mailClear.checked,
    email_from: f.mailFrom.value, email_to: f.mailTo.value.split(',').map(x => x.trim()).filter(Boolean),
    notify_events: events.map(l => $('input', l)).filter(i => i.checked).map(i => i.value),
    trash_retention_days: +f.trash.value, run_retention_days: +f.runs.value, max_parallel_runs: +f.par.value,
    low_space_pct: +f.low.value, db_backup_keep: +f.keep.value, maintenance_hour: +f.hour.value,
    stall_minutes: +f.stall.value,
    global_excludes: f.global.value.split('\n').map(x => x.trim()).filter(Boolean),
    log_level: +f.logLevel.value, log_retention_days: +f.logDays.value, log_max_entries: +f.logMax.value,
    guards: collectGuards(),
    theme, date_format: f.date.value,
    features: { usb_drives: f.usb.checked, hooks: f.hooks.checked },
  });
  const save = guard(async () => {
    const body = collect();
    // Switching off a feature that is in use asks first and says what stops.
    const off = [];
    if (s.features.usb_drives && !body.features.usb_drives && inUse.usb_drives.length) {
      off.push(`USB drive backups (drives: ${inUse.usb_drives.join(', ')}): plugged in drives are no longer backed up and there are no reminders. The drives and their backups stay.`);
    }
    if (s.features.hooks && !body.features.hooks && inUse.hooks.length) {
      off.push(`Hooks (jobs: ${inUse.hooks.join(', ')}): their scripts no longer run before and after the backup. The script names stay saved in the jobs.`);
    }
    if (off.length && !await confirmBox('Switch off features in use?', off.join(' '), { ok: 'Switch off', danger: true })) return;
    await PUT('/api/settings', body);
    state.meta = await GET('/api/meta');
    applyDisplay(state.meta);
    toast('Settings saved');
    render();
  });
  // testButton saves the form, then sends a test through one channel.
  const testButton = (channel, label, done) => h('div', null, h('button', { type: 'button', class: 'small', onclick: guard(async () => {
    await PUT('/api/settings', collect());
    await POST(`/api/settings/test-notification?channel=${channel}`);
    toast(`Saved – ${done}`);
    render();
  }) }, label));

  const pw = { cur: h('input', { type: 'password', autocomplete: 'current-password' }), next: h('input', { type: 'password', autocomplete: 'new-password' }), again: h('input', { type: 'password', autocomplete: 'new-password' }) };
  const fileInput = h('input', { type: 'file', accept: 'application/json,.json', class: 'hidden' });
  fileInput.addEventListener('change', guard(async () => {
    const file = fileInput.files[0];
    if (!file) return;
    let data;
    try { data = JSON.parse(await file.text()); } catch (_) { throw new Error('This is not a BFBackup export file.'); }
    fileInput.value = '';
    if (!await confirmBox('Import configuration', `Replace all jobs, targets, peers and settings with "${file.name}" (${(data.jobs || []).length} jobs, ${(data.nodes || []).length} peers)? Peers reconnect. Your password stays.`, { ok: 'Import', danger: true, word: 'import' })) return;
    const r = await POST('/api/system/import', data);
    toast(`Imported ${r.jobs} jobs and ${r.nodes} peers`);
  }));
  return h('div', { class: 'stack' }, pageHead('Settings', `BFBackup version ${meta.version}`),
    appPanel(),
    foldPanel('Display', h('div', { class: 'stack' },
      h('div', { class: 'form-grid' },
        h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Appearance'), h('div', null, themeSeg),
          h('div', { class: 'help' }, 'Auto follows the system (light or dark mode of the computer or phone).')),
        h('label', { class: 'field' }, h('span', null, 'Date and time'), f.date,
          h('div', { class: 'help' }, 'For the whole web UI, in every browser.'))),
      h('div', null, h('button', { type: 'button', class: 'primary', onclick: save }, 'Save')))),
    foldPanel('Additional features', h('div', { class: 'stack' },
      h('div', { class: 'help' }, 'Parts of BFBackup you may not need. Switched off, they are hidden and do nothing.'),
      h('label', { class: 'check-row' }, f.usb, h('span', null, h('strong', null, 'USB drive backups'),
        h('div', { class: 'help' }, 'The USB drives page: offline backups on drives plugged into this server, backed up when plugged in.'),
        inUseNote(inUse.usb_drives, inUse.usb_drives.length === 1 ? 'drive' : 'drives'))),
      h('label', { class: 'check-row' }, f.hooks, h('span', null, h('strong', null, 'Scripts before and after a backup (hooks)'),
        h('div', { class: 'help' }, 'Per job, a script from /config/hooks on the source node runs before and after each backup, e.g. to stop and start containers.'),
        inUseNote(inUse.hooks, inUse.hooks.length === 1 ? 'job' : 'jobs'))),
      h('div', null, h('button', { type: 'button', class: 'primary', onclick: save }, 'Save')))),
    foldPanel('Notifications', h('div', { class: 'stack' },
      h('div', { class: 'help' }, 'Fill in Pushover, email or both; every channel that is set up gets each notification.'),
      h('h3', null, 'Pushover'),
      h('div', { class: 'form-grid' },
        h('label', { class: 'field' }, h('span', null, 'User key'), f.user),
        h('label', { class: 'field' }, h('span', null, 'Application token'), f.token,
          s.pushover_token_set ? h('label', { class: 'check-row small' }, f.clear, h('span', null, 'Remove the saved token')) : null)),
      testButton('pushover', 'Save and send test push', 'test push sent'),
      h('h3', null, 'Email'),
      h('div', { class: 'form-grid' },
        h('label', { class: 'field' }, h('span', null, 'SMTP server'), f.mailHost,
          h('div', { class: 'help' }, 'Leave empty for no email.')),
        h('label', { class: 'field' }, h('span', null, 'Port'), f.mailPort,
          h('div', { class: 'help' }, 'SMTP over TLS/SSL, usually 465. STARTTLS (587) is not supported.')),
        h('label', { class: 'field' }, h('span', null, 'User name'), f.mailUser,
          h('div', { class: 'help' }, 'Leave empty if the server needs no login.')),
        h('label', { class: 'field' }, h('span', null, 'Password'), f.mailPass,
          s.email_password_set ? h('label', { class: 'check-row small' }, f.mailClear, h('span', null, 'Remove the saved password')) : null),
        h('label', { class: 'field' }, h('span', null, 'Sender address'), f.mailFrom),
        h('label', { class: 'field' }, h('span', null, 'Recipients'), f.mailTo,
          h('div', { class: 'help' }, 'Separate several addresses with commas.'))),
      testButton('email', 'Save and send test email', 'test email sent'),
      h('div', null, h('div', { class: 'field-label' }, 'Send a notification when…'), h('div', { class: 'stack' }, events)),
      h('div', null, h('button', { type: 'button', class: 'primary', onclick: save }, 'Save')))),
    foldPanel('Global exclusions', h('div', { class: 'stack' },
      h('div', { class: 'help' }, 'Applied to every job that does not switch them off, in addition to the job’s own exclusions. Same syntax: one pattern per line, # starts a comment. Files that are already backed up and now match stay in the backup as "not in source".'),
      f.global,
      h('div', { class: 'row' }, h('button', { type: 'button', class: 'primary', onclick: save }, 'Save'),
        h('button', { type: 'button', onclick: () => { f.global.value = meta.default_global_excludes.join('\n'); toast('Defaults filled in – press Save to keep them'); } }, 'Restore defaults')))),
    foldPanel('Retention and limits', h('div', { class: 'stack' },
      h('div', { class: 'form-grid' },
        h('label', { class: 'field' }, h('span', null, 'Keep trash for (days)'), f.trash, h('div', { class: 'help' }, 'Deleted and overwritten files in .bfbackup-trash.')),
        h('label', { class: 'field' }, h('span', null, 'Keep run history for (days)'), f.runs),
        h('label', { class: 'field' }, h('span', null, 'Backups / verifies at the same time'), f.par, h('div', { class: 'help' }, 'Restores and deletes always start immediately.')),
        h('label', { class: 'field' }, h('span', null, 'Warn when a target has less than (% free)'), f.low),
        h('label', { class: 'field' }, h('span', null, 'Database snapshots to keep'), f.keep),
        h('label', { class: 'field' }, h('span', null, 'Daily maintenance hour (0–23)'), f.hour, h('div', { class: 'help' }, 'Purges old trash and history and snapshots the database.')),
        h('label', { class: 'field' }, h('span', null, 'Stop a run without progress after (minutes)'), f.stall,
          h('div', { class: 'help' }, 'A run that copies, scans or checks nothing for this long is stopped as failed (hung peer, disk or connection). Hooks are not counted. 0 = never. A single file transfer that moves no data for 5 minutes always fails and the run goes on.'))),
      h('div', null, h('button', { type: 'button', class: 'primary', onclick: save }, 'Save')))),
    trashPanel(0, true),
    foldPanel('Activity log', h('div', { class: 'stack' },
      h('div', { class: 'form-grid' },
        h('label', { class: 'field' }, h('span', null, 'Log level'), f.logLevel,
          h('div', { class: 'help' }, '1: only errors. 2: also runs, peers, settings (the audit trail) and maintenance, and file issues (unknown files on a target, files gone from the source, skipped files). 3: also every file that was copied, restored or removed. 4: every step and decision, also each unchanged file and why it was not copied – a large backup writes one line per scanned file. File lines are in each run’s file log.')),
        h('label', { class: 'field' }, h('span', null, 'Keep log entries for (days)'), f.logDays),
        h('label', { class: 'field' }, h('span', null, 'Keep at most (entries)'), f.logMax, h('div', { class: 'help' }, 'Oldest entries are removed first, checked every hour. The log has its own file, /config/logs.db.'))),
      h('div', { class: 'row' }, h('button', { type: 'button', class: 'primary', onclick: save }, 'Save'), h('a', { class: 'btn', href: '#/logs' }, 'Open logs')))),
    foldPanel('Anomaly checks', h('div', { class: 'stack' },
      h('div', { class: 'help' }, 'Run before every backup changes anything on a target, for all jobs. When one fires, the backup stops and waits: continue or stop it on the dashboard or the jobs page (and you get a notification). Dry runs only report.'),
      guardRows,
      h('div', null, h('button', { type: 'button', class: 'primary', onclick: save }, 'Save')))),
    foldPanel('Password', h('div', { class: 'stack' },
      h('div', { class: 'form-grid' },
        h('label', { class: 'field' }, h('span', null, 'Current password'), pw.cur),
        h('label', { class: 'field' }, h('span', null, 'New password'), pw.next),
        h('label', { class: 'field' }, h('span', null, 'New password again'), pw.again)),
      h('div', null, h('button', { type: 'button', onclick: guard(async () => {
        if (pw.next.value !== pw.again.value) throw new Error('The new passwords differ.');
        await POST('/api/auth/password', { current: pw.cur.value, password: pw.next.value });
        toast('Password changed. Other sessions were logged out.');
        render();
      }) }, 'Change password')))),
    foldPanel('Controller backup', h('div', { class: 'stack' },
      h('div', { class: 'help' }, 'The export contains jobs, peers (with their tokens) and settings – keep it safe. The database snapshot also has the file index and history. Snapshots are also written daily to /config/db-backups.'),
      h('div', { class: 'row' },
        h('a', { class: 'btn', href: '/api/system/export', download: '' }, 'Export configuration'),
        h('button', { type: 'button', onclick: () => fileInput.click() }, 'Import configuration…'), fileInput,
        h('a', { class: 'btn', href: '/api/system/db', download: '' }, 'Download database snapshot'),
        h('button', { type: 'button', onclick: guard(async () => {
          const r = await POST('/api/maintenance');
          toast(`Maintenance done: ${r.trash_batches_purged} trash batches purged, ${r.runs_pruned} old runs and ${r.logs_pruned} log entries removed` + (r.errors.length ? `, ${r.errors.length} errors` : ''), r.errors.length ? 'bad' : '');
        }) }, 'Run maintenance now')))));
}

// appPanel offers to install the web UI as an app (PWA) on the phone or desktop.
function appPanel() {
  const what = h('div', { class: 'help' }, 'The app opens this controller in its own window with the BFBackup icon, without the browser bar. You stay logged in until you have not used it for 30 days.');
  let body;
  if (standalone() || pwa.installed) {
    body = h('div', { class: 'callout info' }, 'The app is installed. Remove it like any other app on your phone.');
  } else if (!window.isSecureContext) {
    body = h('div', { class: 'callout warn' }, 'Installing needs HTTPS. Open BFBackup through your HTTPS reverse proxy (for example https://backup.example.com) and come back to this page. Over plain http:// you can only add a bookmark to the home screen.');
  } else if (pwa.prompt) {
    body = h('div', null, h('button', { type: 'button', class: 'primary', onclick: guard(async () => {
      const p = pwa.prompt;
      pwa.prompt = null;
      await p.prompt();
      const { outcome } = await p.userChoice;
      if (outcome !== 'accepted') pwa.prompt = p;
      render();
    }) }, 'Install app'));
  } else {
    body = h('div', { class: 'stack' },
      h('div', null, h('strong', null, 'Android (Chrome): '), 'menu ⋮ → ', h('em', null, 'Add to home screen'), ' → ', h('em', null, 'Install'), '. If BFBackup is already installed, Chrome shows ', h('em', null, 'Open in app'), ' there instead.'),
      h('div', null, h('strong', null, 'iPhone (Safari): '), 'share button → ', h('em', null, 'Add to Home Screen'), '.'),
      h('div', { class: 'help' }, 'An Install app button appears here as soon as the browser offers it.'));
  }
  return panel('App for your phone', h('div', { class: 'stack' }, what, body));
}

// ---------- display settings ----------
// applyDisplay sets appearance, date format and the optional parts from the
// controller's settings (state.meta).
function applyDisplay(m) {
  display.date = DATE_FORMATS[m.date_format] ? m.date_format : 'iso';
  display.features = m.features || { usb_drives: true, hooks: true };
  setTheme(m.theme || 'auto');
  $$('#nav a[data-route="drives"]').forEach(a => a.classList.toggle('hidden', !display.features.usb_drives));
  $('#more').dataset.routes = (display.features.usb_drives ? 'drives ' : '') + 'stats settings logs';
}
const THEME_COLORS = { light: '#ffffff', dark: '#171b23' };
// setTheme switches between auto (follow the system), light and dark. The
// choice is also kept in the browser so theme.js applies it before drawing.
function setTheme(t) {
  const root = document.documentElement;
  if (t === 'light' || t === 'dark') root.dataset.theme = t; else delete root.dataset.theme;
  try { if (t === 'light' || t === 'dark') localStorage.setItem('bfb-theme', t); else localStorage.removeItem('bfb-theme'); } catch (_) { /* blocked */ }
  $$('meta[name="theme-color"]').forEach(m => m.setAttribute('content',
    THEME_COLORS[t === 'light' || t === 'dark' ? t : (m.media || '').includes('dark') ? 'dark' : 'light']));
}

// ---------- login and start ----------
async function showLogin() {
  cleanups.forEach(f => f());
  cleanups = [];
  $$('.sidebar, #tabbar').forEach(el => el.classList.add('hidden'));
  const st = await api('GET', '/api/auth/state');
  const pw = h('input', { type: 'password', autocomplete: st.setup_required ? 'new-password' : 'current-password', required: true });
  const pw2 = h('input', { type: 'password', autocomplete: 'new-password' });
  const err = h('div', { class: 'callout bad hidden' });
  const form = h('form', { class: 'stack', onsubmit: async e => {
    e.preventDefault();
    err.classList.add('hidden');
    try {
      if (st.setup_required) {
        if (pw.value !== pw2.value) throw new Error('The passwords differ.');
        await api('POST', '/api/auth/setup', { password: pw.value });
      } else {
        await api('POST', '/api/auth/login', { password: pw.value });
      }
      start();
    } catch (ex) { err.textContent = ex.message; err.classList.remove('hidden'); }
  } },
  h('div', { class: 'brand' }, h('img', { src: 'favicon.svg', alt: '', width: 40, height: 40 }), 'BFBackup'),
  st.setup_required ? h('div', { class: 'callout info' }, 'Welcome! Choose the admin password for this controller.') : null,
  h('input', { type: 'text', name: 'username', value: 'admin', autocomplete: 'username', class: 'hidden', 'aria-hidden': 'true' }),
  h('label', { class: 'field' }, h('span', null, st.setup_required ? 'New password (8+ characters)' : 'Password'), pw),
  st.setup_required ? h('label', { class: 'field' }, h('span', null, 'Password again'), pw2) : null,
  err,
  h('button', { class: 'primary' }, st.setup_required ? 'Set password and continue' : 'Log in'));
  $('#main').replaceChildren(h('div', { class: 'login' }, h('div', { class: 'panel' }, form)));
  pw.focus();
}

async function start() {
  try {
    const st = await api('GET', '/api/auth/state');
    if (!st.authenticated) return showLogin();
    $$('.sidebar, #tabbar').forEach(el => el.classList.remove('hidden'));
    state.meta = await GET('/api/meta');
    $('#version').textContent = 'v' + state.meta.version;
    applyDisplay(state.meta);
    await loadNodes();
    render();
  } catch (e) {
    if (e.status !== 401) $('#main').replaceChildren(h('div', { class: 'callout bad' }, 'Cannot reach the controller: ' + e.message));
  }
}

const logout = guard(async () => { await POST('/api/auth/logout'); showLogin(); });
$('#logout').addEventListener('click', logout);

// "More" on the phone tab bar: the pages that do not fit as tabs.
function showMoreSheet() {
  const back = h('div', { class: 'sheet-back' });
  const close = () => { back.remove(); document.removeEventListener('keydown', esc); };
  const esc = e => { if (e.key === 'Escape') close(); };
  document.addEventListener('keydown', esc);
  back.addEventListener('click', e => { if (e.target === back || e.target.closest('a, button')) close(); });
  const link = (href, label) => h('a', { href }, label);
  back.append(h('div', { class: 'sheet', role: 'dialog', 'aria-modal': 'true', 'aria-label': 'More' },
    display.features.usb_drives ? link('#/drives', 'USB drives') : null, link('#/stats', 'Statistics'), link('#/settings', 'Settings'), link('#/logs', 'Logs'),
    h('hr'),
    h('div', { class: 'sheet-foot' }, h('span', { class: 'foot-info muted small' }, h('span', null, state.meta ? 'v' + state.meta.version : ''), h('span', { class: 'credit' }, 'Created by DelSol')),
      h('button', { type: 'button', onclick: logout }, 'Log out'))));
  document.body.append(back);
}
$('#more').addEventListener('click', showMoreSheet);

// ---------- installed app (PWA) ----------
// Chrome fires beforeinstallprompt when the app can be installed; the
// Settings page offers it then.
const pwa = { prompt: null, installed: false };
const standalone = () => matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
window.addEventListener('beforeinstallprompt', e => { e.preventDefault(); pwa.prompt = e; if (location.hash.startsWith('#/settings')) render(); });
window.addEventListener('appinstalled', () => { pwa.prompt = null; pwa.installed = true; toast('App installed – find BFBackup on your home screen'); });
if ('serviceWorker' in navigator && window.isSecureContext) {
  navigator.serviceWorker.register('/sw.js').catch(e => console.warn('service worker:', e));
}

window.addEventListener('hashchange', () => { if (!$('.sidebar').classList.contains('hidden')) render(); });
start();
