/* godl front end.
 *
 * The list mirrors IDM's: a name over its URL, size, a status that carries the
 * progress bar, transfer rate, time left and connection count. Rows are built
 * once and updated in place, so the half-second refresh never steals focus or
 * restarts a hover animation - a list that rebuilds itself reads as flicker
 * even when the data is correct.
 *
 * Every piece of text that arrived over the network is written with
 * textContent. File names and server errors never go near innerHTML.
 *
 * The page talks to the manager over its own origin with a bearer token held
 * in <meta>. A hostile page can neither read these responses nor set that
 * header, so it can neither see nor move a download.
 */
'use strict';

const TOKEN = document.querySelector('meta[name="godl-token"]').content;
const $ = (id) => document.getElementById(id);

const ui = {
  grid: document.querySelector('.grid'),
  rows: $('rows'),
  empty: $('empty'),
  dir: $('dir'),
  count: $('s-count'),
  active: $('s-active'),
  speed: $('s-speed'),
  add: $('t-add'),
  pause: $('t-pause'),
  resume: $('t-resume'),
  stop: $('t-stop'),
  reveal: $('t-reveal'),
  remove: $('t-remove'),
  modal: $('modal'),
  url: $('m-url'),
  name: $('m-name'),
  seg: $('m-seg'),
  mErr: $('m-error'),
  mCancel: $('m-cancel'),
  mOk: $('m-ok'),
  toast: $('toast'),
};

const LABEL = {
  queued: 'Queued',
  running: 'Running',
  paused: 'Paused',
  done: 'Done',
  failed: 'Failed',
  cancelled: 'Stopped',
};

const rows = new Map();   // id -> { el, ...cells }
let items = [];           // last snapshot from the manager
let selected = null;
let sortKey = null;
let sortDir = 1;
let lastOrder = '';
let offline = false;
let toastTimer = null;

/* ── transport ─────────────────────────────────────────────────────────── */

async function api(path, opts) {
  opts = opts || {};
  const res = await fetch(path, Object.assign({}, opts, {
    headers: Object.assign({ Authorization: 'Bearer ' + TOKEN }, opts.headers || {}),
  }));
  let body = null;
  try { body = await res.json(); } catch (_) { /* non-JSON still has a status */ }
  if (!res.ok) throw new Error((body && body.error) || res.statusText || 'Request failed');
  return body;
}

function toast(message, bad) {
  ui.toast.textContent = message;
  ui.toast.classList.toggle('bad', !!bad);
  ui.toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { ui.toast.hidden = true; }, bad ? 5200 : 3000);
}

/* ── formatting ────────────────────────────────────────────────────────── */

function size(n) {
  if (!n || n <= 0) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  const decimals = i === 0 ? 0 : v >= 100 ? 0 : 1;
  return v.toFixed(decimals) + ' ' + units[i];
}

function duration(seconds) {
  let s = Math.max(0, Math.round(seconds));
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  const r = s % 60;
  if (m < 60) return m + 'm ' + String(r).padStart(2, '0') + 's';
  const h = Math.floor(m / 60);
  return h + 'h ' + String(m % 60).padStart(2, '0') + 'm';
}

function etaFor(d) {
  const p = d.progress || {};
  const total = d.total_size || p.total || 0;
  if (d.status !== 'running' || !(p.speed > 0) || total <= 0) return '—';
  const remain = (total - (p.done || 0)) / p.speed;
  if (!isFinite(remain)) return '—';
  return duration(remain);
}

function baseName(path) {
  if (!path) return '';
  const parts = String(path).split(/[\\/]/);
  return parts[parts.length - 1] || String(path);
}

const enc = (s) => encodeURIComponent(s);

/* ── rows ──────────────────────────────────────────────────────────────── */

function createRow(id) {
  const el = document.createElement('div');
  el.className = 'row';
  el.setAttribute('role', 'row');
  el.dataset.id = id;
  // Static skeleton only; every dynamic string below is set via textContent.
  el.innerHTML =
    '<div class="cell name" role="cell"><div class="fname"></div><div class="furl"></div></div>' +
    '<div class="cell size" role="cell"></div>' +
    '<div class="cell status" role="cell"><span class="st-text"></span>' +
      '<span class="track"><i></i></span></div>' +
    '<div class="cell rate" role="cell"></div>' +
    '<div class="cell eta" role="cell"></div>' +
    '<div class="cell segs" role="cell"></div>';

  return {
    el,
    fname: el.querySelector('.fname'),
    furl: el.querySelector('.furl'),
    size: el.querySelector('.cell.size'),
    st: el.querySelector('.st-text'),
    fill: el.querySelector('.track i'),
    rate: el.querySelector('.cell.rate'),
    eta: el.querySelector('.cell.eta'),
    segs: el.querySelector('.cell.segs'),
  };
}

function updateRow(row, d) {
  const status = d.status || 'queued';
  const p = d.progress || {};
  const total = d.total_size || p.total || 0;
  const pct = status === 'done' ? 100 : Math.max(0, Math.min(100, p.percent || 0));

  row.el.className = 'row ' + status + (selected === d.id ? ' selected' : '');

  row.fname.textContent = baseName(d.dest);
  row.fname.title = d.dest || '';
  row.furl.textContent = d.url || '';
  row.furl.title = d.url || '';

  row.size.textContent = total > 0 ? size(total) : '—';
  row.fill.style.width = pct.toFixed(1) + '%';

  if (d.error) {
    row.st.className = 'st-text err';
    row.st.textContent = d.error;
    row.st.title = d.error;
  } else {
    const text = status === 'running'
      ? Math.round(pct) + '% · ' + (LABEL[status] || status)
      : (LABEL[status] || status);
    row.st.className = 'st-text' + (status === 'done' ? ' done' : status === 'running' ? ' run' : '');
    row.st.textContent = text;
    row.st.title = '';
  }

  const live = status === 'running' && p.speed > 0;
  row.rate.className = 'cell rate' + (live ? ' live' : '');
  row.rate.textContent = live ? size(p.speed) + '/s' : '—';
  row.eta.textContent = etaFor(d);
  row.segs.textContent = d.connections > 0 ? String(d.connections) : '—';
}

/* ── ordering ──────────────────────────────────────────────────────────── */

function sortList(list) {
  if (!sortKey) return list;
  const cmp = {
    name: (a, b) => baseName(a.dest).localeCompare(baseName(b.dest), undefined, { numeric: true }),
    size: (a, b) => (a.total_size || 0) - (b.total_size || 0),
    rate: (a, b) => speedOf(a) - speedOf(b),
    segs: (a, b) => (a.connections || 0) - (b.connections || 0),
  }[sortKey];
  if (!cmp) return list;
  return list.slice().sort((a, b) => cmp(a, b) * sortDir);
}

const speedOf = (d) => (d.progress && d.progress.speed) || 0;

/* ── render ────────────────────────────────────────────────────────────── */

function render(list) {
  const ordered = sortList(list);
  const seen = new Set();
  let anyNew = false;

  for (const d of ordered) {
    if (!d || !d.id) continue;
    seen.add(d.id);
    let row = rows.get(d.id);
    if (!row) {
      row = createRow(d.id);
      rows.set(d.id, row);
      anyNew = true;
    }
    updateRow(row, d);
  }

  for (const [id, row] of rows) {
    if (!seen.has(id)) {
      row.el.remove();
      rows.delete(id);
    }
  }

  if (selected && !seen.has(selected)) selected = null;

  const order = ordered.map((d) => d.id).join(',');
  if (anyNew || order !== lastOrder) {
    for (const d of ordered) ui.rows.appendChild(rows.get(d.id).el);
    lastOrder = order;
  }

  const empty = seen.size === 0;
  ui.grid.hidden = empty;
  ui.empty.hidden = !empty;

  syncToolbar();
  updateStatusBar(list);
}

function updateStatusBar(list) {
  let active = 0;
  let total = 0;
  for (const d of list) {
    if (d.status === 'running') {
      active++;
      total += speedOf(d);
    }
  }
  ui.count.textContent = list.length === 1 ? '1 item' : list.length + ' items';
  ui.active.textContent = active === 0 ? 'Idle'
    : active === 1 ? '1 downloading'
    : active + ' downloading';
  ui.speed.textContent = (total > 0 ? size(total) + '/s' : '0 B/s');
}

/* ── selection and controls ────────────────────────────────────────────── */

function current() {
  return items.find((d) => d.id === selected) || null;
}

function select(id) {
  selected = id;
  for (const [rowId, row] of rows) {
    row.el.classList.toggle('selected', rowId === id);
  }
  syncToolbar();
}

function syncToolbar() {
  const d = current();
  const st = d ? d.status : null;
  // Each control is enabled only in the states the server would accept, so a
  // click can never produce a 409 from an otherwise valid action.
  ui.pause.disabled = !(st === 'running' || st === 'queued');
  ui.resume.disabled = !(st === 'paused' || st === 'failed');
  ui.stop.disabled = !(st !== null && st !== 'done' && st !== 'cancelled');
  ui.reveal.disabled = !d;
  ui.remove.disabled = !d;
}

async function act(action) {
  if (!selected) return;
  try {
    if (action === 'remove') {
      await api('/api/downloads/' + enc(selected), { method: 'DELETE' });
    } else {
      await api('/api/downloads/' + enc(selected) + '/' + action, { method: 'POST' });
    }
    await refresh();
  } catch (err) {
    toast(err.message, true);
  }
}

/* ── add sheet ─────────────────────────────────────────────────────────── */

function openModal() {
  ui.modal.hidden = false;
  ui.mErr.hidden = true;
  ui.mErr.textContent = '';
  ui.url.value = '';
  ui.name.value = '';
  setTimeout(() => ui.url.focus(), 40);
}

function closeModal() {
  ui.modal.hidden = true;
}

function sheetError(message) {
  ui.mErr.textContent = message;
  ui.mErr.hidden = false;
}

async function submitAdd() {
  const url = ui.url.value.trim();
  if (!url) {
    sheetError('Enter an address.');
    ui.url.focus();
    return;
  }

  ui.mOk.disabled = true;
  ui.mOk.textContent = 'Adding…';
  try {
    const res = await api('/api/downloads', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        url,
        filename: ui.name.value.trim(),
        connections: Number(ui.seg.value) || 0,
      }),
    });
    closeModal();
    const dest = res.download && res.download.dest;
    toast(res.duplicate ? 'That address is already in the list.' : 'Added ' + baseName(dest), false);
    await refresh();
  } catch (err) {
    sheetError(err.message);
  } finally {
    ui.mOk.disabled = false;
    ui.mOk.textContent = 'Download';
  }
}

/* ── wiring ────────────────────────────────────────────────────────────── */

ui.rows.addEventListener('click', (e) => {
  const row = e.target.closest('.row');
  if (row) select(row.dataset.id);
});

ui.add.addEventListener('click', openModal);
ui.mCancel.addEventListener('click', closeModal);
ui.modal.addEventListener('click', (e) => { if (e.target === ui.modal) closeModal(); });
ui.mOk.addEventListener('click', submitAdd);
ui.url.addEventListener('keydown', (e) => { if (e.key === 'Enter') submitAdd(); });
ui.name.addEventListener('keydown', (e) => { if (e.key === 'Enter') submitAdd(); });

ui.pause.addEventListener('click', () => act('pause'));
ui.resume.addEventListener('click', () => {
  const d = current();
  act(d && d.status === 'failed' ? 'retry' : 'resume');
});
ui.stop.addEventListener('click', () => act('cancel'));
ui.reveal.addEventListener('click', () => act('reveal'));
ui.remove.addEventListener('click', () => act('remove'));

document.addEventListener('keydown', (e) => {
  if (!ui.modal.hidden) {
    if (e.key === 'Escape') closeModal();
    return;
  }
  if ((e.ctrlKey || e.metaKey) && (e.key === 'n' || e.key === 'N')) {
    e.preventDefault();
    openModal();
    return;
  }
  if (e.key === 'Delete' && selected) act('remove');
});

document.querySelectorAll('.th[data-sort]').forEach((th) => {
  th.addEventListener('click', () => {
    const key = th.dataset.sort;
    if (sortKey === key) sortDir = -sortDir;
    else { sortKey = key; sortDir = 1; }
    document.querySelectorAll('.th').forEach((h) => h.classList.remove('sorted'));
    th.classList.add('sorted');
    lastOrder = '';           // force the DOM back into the new order
    render(items);
  });
});

/* ── polling ───────────────────────────────────────────────────────────── */

async function refresh() {
  try {
    const res = await api('/api/downloads');
    if (offline) {
      offline = false;
      toast('Reconnected.', false);
    }
    items = Array.isArray(res.downloads) ? res.downloads : [];
    render(items);
  } catch (err) {
    // Report the break once instead of once per tick, and keep the last good
    // view on screen rather than blanking it.
    if (!offline) {
      offline = true;
      toast('Lost touch with the manager: ' + err.message, true);
    }
  }
}

async function bootstrap() {
  try {
    const health = await api('/api/health');
    ui.dir.textContent = health.download_dir || '';
    ui.dir.title = health.download_dir || '';
  } catch (_) {
    /* the list below reports connectivity more usefully than this would */
  }
  await refresh();
}

bootstrap();
setInterval(refresh, 700);
