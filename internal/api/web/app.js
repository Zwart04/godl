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
  limit: $('t-limit'),
  up: $('t-up'),
  down: $('t-down'),
  schedule: $('t-schedule'),
  reveal: $('t-reveal'),
  remove: $('t-remove'),
  modal: $('modal'),
  url: $('m-url'),
  name: $('m-name'),
  seg: $('m-seg'),
  mLimit: $('m-limit'),
  mLimitCustom: $('m-limit-custom'),
  mErr: $('m-error'),
  mSize: $('m-size'),
  mCancel: $('m-cancel'),
  mOk: $('m-ok'),
  limitModal: $('limit'),
  lWho: $('l-who'),
  lSpeed: $('l-speed'),
  lSpeedCustom: $('l-speed-custom'),
  lErr: $('l-error'),
  lCancel: $('l-cancel'),
  lOk: $('l-ok'),
  schedModal: $('sched'),
  scWho: $('sc-who'),
  scWhen: $('sc-when'),
  scErr: $('sc-error'),
  scCancel: $('sc-cancel'),
  scOk: $('sc-ok'),
  mCat: $('m-cat'),
  mCatRow: $('m-cat-row'),
  catModal: $('cat'),
  ctWho: $('ct-who'),
  ctWhere: $('ct-where'),
  ctErr: $('ct-error'),
  ctCancel: $('ct-cancel'),
  ctOk: $('ct-ok'),
  clipModal: $('clip'),
  clUrl: $('cl-url'),
  clErr: $('cl-error'),
  clNo: $('cl-no'),
  clYes: $('cl-yes'),
  toast: $('toast'),
};

/* The offer, in bytes per second. Both menus are built from this one list so
 * they cannot drift into offering different speeds for the same button. */
const LIMITS = [
  [0, 'No limit'],
  [128 * 1024, '128 KB/s'],
  [512 * 1024, '512 KB/s'],
  [1024 * 1024, '1 MB/s'],
  [5 * 1024 * 1024, '5 MB/s'],
  [10 * 1024 * 1024, '10 MB/s'],
];
/* Sentinel for the "type your own" row. Kept negative so it can never be
 * mistaken for a real ceiling, which the server would reject anyway. */
const CUSTOM = -1;

const LABEL = {
  queued: 'Queued',
  running: 'Running',
  paused: 'Paused',
  done: 'Done',
  failed: 'Failed',
  cancelled: 'Stopped',
};

/* The categories in force, read once at start-up. An empty list is how the
 * manager says it files everything into one folder, and the interface then
 * offers no control that would only come back refused. */
let CATEGORIES = [];

/* A record written before categories existed answers with nothing, and reads
 * as the catch-all rather than as a gap - which is what the manager does with
 * it too. */
const categoryName = (d) => (d && d.category) || 'Other';

/* Rebuilds both category menus from the one list, so what the add sheet offers
 * and what the change sheet offers cannot drift apart. */
function fillCategorySelects() {
  ui.mCatRow.hidden = CATEGORIES.length === 0;

  ui.mCat.textContent = '';
  ui.mCat.add(new Option('Automatic (by extension)', ''));
  for (const c of CATEGORIES) ui.mCat.add(new Option(c.name, c.name));

  ui.ctWhere.textContent = '';
  for (const c of CATEGORIES) ui.ctWhere.add(new Option(c.name, c.name));
}

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

/* ── speed limits ──────────────────────────────────────────────────────── */

function fillLimitSelect(sel) {
  sel.textContent = '';
  for (const [bps, label] of LIMITS) {
    const opt = document.createElement('option');
    opt.value = String(bps);
    opt.textContent = label;
    sel.appendChild(opt);
  }
  const custom = document.createElement('option');
  custom.value = String(CUSTOM);
  custom.textContent = 'Custom…';
  sel.appendChild(custom);
}

/* Shows one ceiling in a menu, reaching for "Custom" when it is not a preset.
 * Without that branch a limit typed last week would snap silently to the
 * nearest preset the next time anything about the download was saved. */
function showLimit(sel, custom, bps) {
  const preset = LIMITS.some(([v]) => v === bps);
  sel.value = preset ? String(bps) : String(CUSTOM);
  custom.hidden = sel.value !== String(CUSTOM);
  custom.value = preset ? '' : String(Math.max(1, Math.round((bps || 0) / 1024)));
}

/* Reads a menu back into bytes per second, or null when "Custom" is chosen
 * with nothing usable in the box. Returning null instead of zero keeps "the
 * user has not said" apart from "no limit", which is what zero means. */
function readLimit(sel, custom) {
  const v = Number(sel.value);
  if (v !== CUSTOM) return v;
  const kbps = Number(custom.value);
  if (!isFinite(kbps) || kbps <= 0) return null;
  return Math.round(kbps * 1024);
}

/* ── scheduling ─────────────────────────────────────────────────────────── */

const two = (n) => String(n).padStart(2, '0');

// Reads a datetime-local box into a Date on this machine's clock.
//
// Parsing the string with Date() would do the wrong thing: a date-time with
// no offset is read as UTC, so 22:00 in the box would mean 22:00 in London
// and the download would start hours before the user asked for.
function localWhen(value) {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(value || '');
  if (!m) return null;
  const at = new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5]);
  return isNaN(at.getTime()) ? null : at;
}

// Puts a stored instant back in the box in this machine's own time, so what
// was set is what is shown.
function boxWhen(iso) {
  const at = new Date(iso);
  if (isNaN(at.getTime())) return '';
  return at.getFullYear() + '-' + two(at.getMonth() + 1) + '-' + two(at.getDate()) +
    'T' + two(at.getHours()) + ':' + two(at.getMinutes());
}

// The local time a download is held until, as text, or null when it is not
// waiting on a clock. A schedule already in the past is one the scheduler is
// free to act on, and "Starts yesterday" beside a queued file would only be
// confusing.
function scheduledFor(d) {
  if (!d.starts_at) return null;
  const at = new Date(d.starts_at);
  if (isNaN(at.getTime()) || at.getTime() <= Date.now()) return null;
  return at.toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  });
}

/* ── rows ──────────────────────────────────────────────────────────────── */

function createRow(id) {
  const el = document.createElement('div');
  el.className = 'row';
  el.setAttribute('role', 'row');
  el.dataset.id = id;
  // Static skeleton only; every dynamic string below is set via textContent.
  el.innerHTML =
    '<div class="cell name" role="cell"><div class="fname"></div><div class="furl"></div></div>' +
    '<div class="cell cat" role="cell"><button class="chip" type="button"></button></div>' +
    '<div class="cell size" role="cell"></div>' +
    '<div class="cell status" role="cell"><span class="st-line">' +
      '<span class="st-text"></span><span class="st-cap" hidden></span></span>' +
      '<span class="track"><i></i></span></div>' +
    '<div class="cell rate" role="cell"></div>' +
    '<div class="cell eta" role="cell"></div>' +
    '<div class="cell segs" role="cell"></div>';

  return {
    el,
    fname: el.querySelector('.fname'),
    furl: el.querySelector('.furl'),
    cat: el.querySelector('.chip'),
    size: el.querySelector('.cell.size'),
    st: el.querySelector('.st-text'),
    cap: el.querySelector('.st-cap'),
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

  row.cat.textContent = categoryName(d);
  row.cat.title = 'Filed under ' + categoryName(d) + '. Choose another to move the file.';
  // A transfer under way has the file open, so the manager will refuse to move
  // it; and with no categories configured there is nowhere to move it to.
  // Both are facts already known here, so the control is simply off rather
  // than being a click that only comes back refused.
  row.cat.disabled = status === 'running' || CATEGORIES.length === 0;

  row.size.textContent = total > 0 ? size(total) : '—';
  row.fill.style.width = pct.toFixed(1) + '%';

  if (d.error) {
    row.st.className = 'st-text err';
    row.st.textContent = d.error;
    row.st.title = d.error;
  } else {
    // A download waiting on a clock has to name it. "Queued" beside nothing
    // happening reads as a queue that has quietly stalled, when the truth is
    // that it is doing exactly what it was told.
    const waiting = scheduledFor(d);
    const text = status === 'running'
      ? Math.round(pct) + '% · ' + (LABEL[status] || status)
      : waiting
        ? 'Starts ' + waiting
        : (LABEL[status] || status);
    row.st.className = 'st-text' +
      (status === 'done' ? ' done' : status === 'running' ? ' run' : waiting ? ' wait' : '');
    row.st.textContent = text;
    row.st.title = waiting ? 'Held until ' + waiting : '';
  }

  const live = status === 'running' && p.speed > 0;
  row.rate.className = 'cell rate' + (live ? ' live' : '');
  row.rate.textContent = live ? size(p.speed) + '/s' : '—';
  row.eta.textContent = etaFor(d);
  row.segs.textContent = d.connections > 0 ? String(d.connections) : '—';

  // A cap is worth seeing even when the transfer is stopped, because that is
  // when the user goes looking for why it was slow last time.
  const capped = (d.speed_limit || 0) > 0;
  row.cap.hidden = !capped;
  if (capped) {
    row.cap.textContent = size(d.speed_limit) + '/s cap';
    row.cap.title = 'Capped at ' + size(d.speed_limit) + ' per second. Change it with Limit.';
  }
}

/* ── ordering ──────────────────────────────────────────────────────────── */

function sortList(list) {
  if (!sortKey) return list;
  const cmp = {
    name: (a, b) => baseName(a.dest).localeCompare(baseName(b.dest), undefined, { numeric: true }),
    cat: (a, b) => categoryName(a).localeCompare(categoryName(b)),
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
  // A finished or discarded download has no transfer left to throttle; the
  // server would accept the change and it would mean nothing.
  ui.limit.disabled = !(st !== null && st !== 'done' && st !== 'cancelled');
  // The arrows answer against queue order, which the list is already in unless
  // a column sort is showing over it - so the ends are known here, and the
  // buttons say so instead of waiting to be refused by the server.
  const idx = selected === null ? -1 : items.findIndex((x) => x.id === selected);
  ui.up.disabled = idx <= 0;
  ui.down.disabled = idx < 0 || idx >= items.length - 1;
  // Only records with a start left to postpone may be given one: a transfer
  // already under way has begun, and a finished one has nothing to begin.
  ui.schedule.disabled = !(st === 'queued' || st === 'paused' ||
    st === 'failed' || st === 'cancelled');
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
  ui.mSize.hidden = true;
  ui.mSize.textContent = '';
  ui.url.value = '';
  ui.name.value = '';
  ui.mCat.value = '';
  showLimit(ui.mLimit, ui.mLimitCustom, 0);
  setTimeout(() => ui.url.focus(), 40);
}

function closeModal() {
  ui.modal.hidden = true;

  // Closing while answering a browser capture is a real answer, and it is
  // handed back so the next poll does not put the same download straight in
  // front of them again.
  if (captureID) {
    const id = captureID;
    captureID = '';
    api(`/api/captures/${id}/ignore`, { method: 'POST' }).catch(() => {});
  }
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

  const limit = readLimit(ui.mLimit, ui.mLimitCustom);
  if (limit === null) {
    sheetError('Enter a custom speed in kilobytes per second, or pick a preset.');
    ui.mLimitCustom.focus();
    return;
  }

  ui.mOk.disabled = true;
  ui.mOk.textContent = captureID ? 'Starting…' : 'Adding…';
  try {
    // Answering a browser capture starts the one the manager is holding, with
    // whatever was changed on the way - another folder, another category,
    // another connection count, another address. Adding by hand posts to the
    // list instead, and the two share every check below this point.
    const target = captureID ? `/api/captures/${captureID}/accept` : '/api/downloads';
    const res = await api(target, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        url,
        filename: ui.name.value.trim(),
        connections: Number(ui.seg.value) || 0,
        speed_limit: limit,
        // Empty means "decide from the extension", which is the case for every
        // link a browser hands over and the reason this is a choice rather
        // than a step.
        category: ui.mCat.value,
      }),
    });
    // Cleared before the sheet is put down, so closing it does not answer
    // "not now" to a download that has already started.
    captureID = '';
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

/* ── limit sheet ───────────────────────────────────────────────────────── */

function openLimit() {
  const d = current();
  if (!d) return;
  ui.limitModal.hidden = false;
  ui.lErr.hidden = true;
  ui.lErr.textContent = '';
  ui.lWho.textContent = d.dest || d.url || '';
  ui.lWho.title = d.dest || '';
  showLimit(ui.lSpeed, ui.lSpeedCustom, d.speed_limit || 0);
  setTimeout(() => ui.lSpeed.focus(), 40);
}

function closeLimit() {
  ui.limitModal.hidden = true;
}

function limitError(message) {
  ui.lErr.textContent = message;
  ui.lErr.hidden = false;
}

async function submitLimit() {
  if (!selected) return;

  const limit = readLimit(ui.lSpeed, ui.lSpeedCustom);
  if (limit === null) {
    limitError('Enter a custom speed in kilobytes per second, or pick a preset.');
    ui.lSpeedCustom.focus();
    return;
  }

  ui.lOk.disabled = true;
  ui.lOk.textContent = 'Applying…';
  try {
    // POSTed rather than sent as an action, because this carries a value the
    // server has to read and refuse if it cannot honour it.
    await api('/api/downloads/' + enc(selected) + '/speed-limit', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ speed_limit: limit }),
    });
    closeLimit();
    toast(limit > 0 ? 'Capped at ' + size(limit) + ' per second.' : 'Speed limit removed.', false);
    await refresh();
  } catch (err) {
    limitError(err.message);
  } finally {
    ui.lOk.disabled = false;
    ui.lOk.textContent = 'Apply';
  }
}

/* ── queue order ────────────────────────────────────────────────────────── */

// Drops a column sort so the list is in queue order again, which is the order
// the arrows act on. Left in place, an arrow would reorder against a list the
// user cannot see - and a move that changes nothing on screen reads as a
// button that does not work.
function clearSort() {
  if (sortKey === null) return;
  sortKey = null;
  sortDir = 1;
  document.querySelectorAll('.th').forEach((h) => h.classList.remove('sorted'));
  lastOrder = '';
}

async function move(step) {
  if (selected === null) return;
  clearSort();
  await act(step < 0 ? 'move-up' : 'move-down');
}

/* ── schedule sheet ─────────────────────────────────────────────────────── */

function openSchedule() {
  const d = current();
  if (!d) return;
  ui.schedModal.hidden = false;
  ui.scErr.hidden = true;
  ui.scErr.textContent = '';
  ui.scWho.textContent = d.dest || d.url || '';
  ui.scWho.title = d.dest || '';
  ui.scWhen.value = d.starts_at ? boxWhen(d.starts_at) : '';
  setTimeout(() => ui.scWhen.focus(), 40);
}

function closeSchedule() {
  ui.schedModal.hidden = true;
}

function scheduleError(message) {
  ui.scErr.textContent = message;
  ui.scErr.hidden = false;
}

async function submitSchedule() {
  if (!selected) return;

  const at = localWhen(ui.scWhen.value);
  if (ui.scWhen.value && !at) {
    scheduleError('Pick a date and time, or clear the box to start it now.');
    ui.scWhen.focus();
    return;
  }
  // A time already behind us would be stored, read as runnable, and start the
  // transfer immediately - which is not what someone typing it asked for.
  if (at && at.getTime() <= Date.now()) {
    scheduleError('That time has already passed. Clear the box to start it now.');
    ui.scWhen.focus();
    return;
  }

  ui.scOk.disabled = true;
  ui.scOk.textContent = 'Applying…';
  try {
    await api('/api/downloads/' + enc(selected) + '/schedule', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ starts_at: at ? at.toISOString() : '' }),
    });
    closeSchedule();
    toast(at
      ? 'Starts ' + at.toLocaleString(undefined, {
        month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
      }) + '.'
      : 'Starts as soon as a slot is free.', false);
    await refresh();
  } catch (err) {
    scheduleError(err.message);
  } finally {
    ui.scOk.disabled = false;
    ui.scOk.textContent = 'Apply';
  }
}

/* ── category sheet ─────────────────────────────────────────────────────── */

function openCategory() {
  const d = current();
  if (!d) return;
  ui.catModal.hidden = false;
  ui.ctErr.hidden = true;
  ui.ctErr.textContent = '';
  ui.ctWho.textContent = d.dest || d.url || '';
  ui.ctWho.title = d.dest || '';
  // Offered rather than pre-selected, so a change is made on purpose. A menu
  // already showing the current category would let Apply be pressed and then
  // report that nothing had happened, which is a button that does not work.
  ui.ctWhere.value = '';
  setTimeout(() => ui.ctWhere.focus(), 40);
}

function closeCategory() {
  ui.catModal.hidden = true;
}

function categoryError(message) {
  ui.ctErr.textContent = message;
  ui.ctErr.hidden = false;
}

async function submitCategory() {
  if (!selected) return;

  if (!ui.ctWhere.value) {
    categoryError('Choose the category this download belongs in.');
    ui.ctWhere.focus();
    return;
  }
  // The name is sent as the list spells it, so the record never ends up
  // carrying two spellings of the same category.
  const picked = ui.ctWhere.value;

  ui.ctOk.disabled = true;
  ui.ctOk.textContent = 'Moving…';
  try {
    await api('/api/downloads/' + enc(selected) + '/category', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ category: picked }),
    });
    closeCategory();
    toast('Moved to ' + picked + '.', false);
    await refresh();
  } catch (err) {
    categoryError(err.message);
  } finally {
    ui.ctOk.disabled = false;
    ui.ctOk.textContent = 'Move';
  }
}

/* ── copied-link sheet ──────────────────────────────────────────────────── */

/* The address the manager is holding out. It is watched rather than assumed:
 * a window is opened by a person, and the thing they were doing when they
 * copied the link might have been closing a sheet. */
let clipLink = '';

function anySheetOpen() {
  return !ui.clipModal.hidden || !ui.catModal.hidden || !ui.schedModal.hidden ||
    !ui.limitModal.hidden || !ui.modal.hidden;
}

function openClipboard(url) {
  clipLink = url;
  ui.clUrl.textContent = url;
  ui.clUrl.title = url;
  ui.clErr.hidden = true;
  ui.clErr.textContent = '';
  ui.clipModal.hidden = false;
  setTimeout(() => ui.clYes.focus(), 40);
}

/* Not now is a real answer, and it is handed back so the same link is not put
 * forward again on the next tick. The sheet is hidden only once the manager has
 * taken it: otherwise a poll already on its way would find it still waiting and
 * open it again before the dismissal had landed. */
async function closeClipboard() {
  try {
    await api('/api/clipboard/ignore', { method: 'POST' });
  } catch (_) {
    /* the next copy replaces the offer regardless */
  }
  ui.clipModal.hidden = true;
  clipLink = '';
}

function clipboardError(message) {
  ui.clErr.textContent = message;
  ui.clErr.hidden = false;
}

async function submitClipboard() {
  const url = clipLink;
  if (!url) return;

  ui.clYes.disabled = true;
  ui.clYes.textContent = 'Adding…';
  try {
    const res = await api('/api/clipboard/accept', { method: 'POST' });
    ui.clipModal.hidden = true;
    clipLink = '';
    const d = res && res.download;
    toast(res && res.duplicate
      ? 'Already in the list.'
      : 'Added ' + (d && d.name ? d.name : url) + '.', false);
    await refresh();
  } catch (err) {
    // The offer is left standing on purpose. The manager only lets go of a
    // captured link once a record exists, so a server that could not be
    // reached gets a message with a working button under it rather than a
    // dead end that only another copy could restart.
    clipboardError(err.message);
  } finally {
    ui.clYes.disabled = false;
    ui.clYes.textContent = 'Download';
  }
}

/* Asked once a second, on its own rhythm rather than the list's: a captured
 * link is a human-speed event, and it is only ever opened when nothing else is
 * being asked of the window. A dialog appearing over a dialog is how someone
 * presses the wrong button. */
async function pollClipboard() {
  if (anySheetOpen()) return;
  let res;
  try {
    res = await api('/api/clipboard');
  } catch (_) {
    // refresh() already reports connectivity once per break; this must not
    // add a second complaint about the same thing.
    return;
  }
  const url = res && typeof res.url === 'string' ? res.url : '';
  // Checked again after the await: a sheet opened while this was in flight is
  // a request the user is already answering, and this would take it over.
  if (!url || url === clipLink || anySheetOpen()) return;
  openClipboard(url);
}

/* Shows the custom box only when "Custom" is chosen, and focuses it so the
 * next keystroke lands somewhere useful. */
function wireLimitSelect(sel, custom) {
  sel.addEventListener('change', () => {
    const isCustom = sel.value === String(CUSTOM);
    custom.hidden = !isCustom;
    if (isCustom) setTimeout(() => custom.focus(), 0);
  });
}

/* ── browser capture sheet ──────────────────────────────────────────────── */

/* The download the browser handed over and nobody has answered yet. It opens
 * in the Add sheet: that sheet already asks for the name, the folder, the
 * category and the connection count, which is the whole of what a download
 * dialog is for. Answering one means the Download button calls accept rather
 * than add - see submitAdd. */
let captureID = '';

/* Brings the window forward. A dialog that opens behind the browser is the
 * same as no dialog at all, which is exactly how this looked before the
 * wizard existed: the browser had already erased its own row, and nothing on
 * screen said a transfer had begun. */
function claimAttention() {
  const rt = window.runtime;
  if (!rt) return;
  try {
    if (rt.WindowUnminimise) rt.WindowUnminimise();
    if (rt.WindowShow) rt.WindowShow();
    if (rt.WindowSetForeground) rt.WindowSetForeground();
  } catch (_) {
    /* the sheet still opens; it is simply somewhere behind everything */
  }
}

/* The size line. It fills in once the server has asked the remote end, which
 * happens after the sheet is already up - so a slow server costs this line
 * and never the dialog itself. */
function showCaptureSize(c) {
  if (!c || !(c.size > 0)) return;
  ui.mSize.textContent = size(c.size);
  ui.mSize.hidden = false;
}

function openCapture(c) {
  captureID = c.id;
  openModal();
  ui.url.value = c.url || '';
  ui.name.value = c.filename || '';
  if (c.category) ui.mCat.value = c.category;
  if (c.connections > 0) ui.seg.value = String(c.connections);
  showCaptureSize(c);
}

/* Asked on its own rhythm, like the captured link's. A browser capture is a
 * human-speed event, and it is only ever opened when nothing else is being
 * asked of the window: a dialog appearing over a dialog is how someone
 * presses the wrong button. */
async function pollCaptures() {
  let res;
  try {
    res = await api('/api/captures');
  } catch (_) {
    // refresh() already reports connectivity once per break; this must not
    // add a second complaint about the same thing.
    return;
  }
  const list = res && Array.isArray(res.captures) ? res.captures : [];

  if (captureID) {
    const open = list.find((c) => c.id === captureID);
    if (!open) {
      // Answered elsewhere, or given up on: put the sheet down rather than
      // leave it asking about a download that no longer exists.
      captureID = '';
      if (!ui.modal.hidden) closeModal();
      return;
    }
    showCaptureSize(open);
    return;
  }

  if (anySheetOpen() || !list.length) return;
  // Oldest first. Someone who clicked three links meant all three, and the
  // one they clicked first is the one they expect to be asked about.
  openCapture(list[0]);
  claimAttention();
}

/* ── wiring ────────────────────────────────────────────────────────────── */

ui.rows.addEventListener('click', (e) => {
  const row = e.target.closest('.row');
  if (!row) return;
  select(row.dataset.id);
  // Changing a category is done from the chip that shows it, so the control
  // sits with the value it changes rather than in a bar that has no room left
  // for an eleventh button.
  const chip = e.target.closest('.chip');
  if (chip && !chip.disabled) openCategory();
});

ui.add.addEventListener('click', openModal);
ui.mCancel.addEventListener('click', closeModal);
ui.modal.addEventListener('click', (e) => { if (e.target === ui.modal) closeModal(); });
ui.mOk.addEventListener('click', submitAdd);
ui.url.addEventListener('keydown', (e) => { if (e.key === 'Enter') submitAdd(); });
ui.name.addEventListener('keydown', (e) => { if (e.key === 'Enter') submitAdd(); });

ui.limit.addEventListener('click', openLimit);
ui.lCancel.addEventListener('click', closeLimit);
ui.limitModal.addEventListener('click', (e) => { if (e.target === ui.limitModal) closeLimit(); });
ui.lOk.addEventListener('click', submitLimit);
ui.lSpeed.addEventListener('keydown', (e) => { if (e.key === 'Enter') submitLimit(); });
ui.lSpeedCustom.addEventListener('keydown', (e) => { if (e.key === 'Enter') submitLimit(); });

ui.up.addEventListener('click', () => move(-1));
ui.down.addEventListener('click', () => move(1));
ui.schedule.addEventListener('click', openSchedule);
ui.scCancel.addEventListener('click', closeSchedule);
ui.schedModal.addEventListener('click', (e) => { if (e.target === ui.schedModal) closeSchedule(); });
ui.scOk.addEventListener('click', submitSchedule);
ui.scWhen.addEventListener('keydown', (e) => { if (e.key === 'Enter') submitSchedule(); });

ui.catModal.addEventListener('click', (e) => { if (e.target === ui.catModal) closeCategory(); });
ui.ctCancel.addEventListener('click', closeCategory);
ui.ctOk.addEventListener('click', submitCategory);
ui.ctWhere.addEventListener('keydown', (e) => { if (e.key === 'Enter') submitCategory(); });

ui.clipModal.addEventListener('click', (e) => { if (e.target === ui.clipModal) closeClipboard(); });
ui.clNo.addEventListener('click', closeClipboard);
ui.clYes.addEventListener('click', submitClipboard);

wireLimitSelect(ui.mLimit, ui.mLimitCustom);
wireLimitSelect(ui.lSpeed, ui.lSpeedCustom);

ui.pause.addEventListener('click', () => act('pause'));
ui.resume.addEventListener('click', () => {
  const d = current();
  act(d && d.status === 'failed' ? 'retry' : 'resume');
});
ui.stop.addEventListener('click', () => act('cancel'));
ui.reveal.addEventListener('click', () => act('reveal'));
ui.remove.addEventListener('click', () => act('remove'));

document.addEventListener('keydown', (e) => {
  // The sheets stack, so the one on top answers first - otherwise Escape
  // would close two of them at once and leave the caller unsure what changed.
  // A copied link is drawn over everything, because it is a question with
  // nothing behind it the person meant to answer.
  if (!ui.clipModal.hidden) {
    if (e.key === 'Escape') closeClipboard();
    return;
  }
  if (!ui.catModal.hidden) {
    if (e.key === 'Escape') closeCategory();
    return;
  }
  if (!ui.schedModal.hidden) {
    if (e.key === 'Escape') closeSchedule();
    return;
  }
  if (!ui.limitModal.hidden) {
    if (e.key === 'Escape') closeLimit();
    return;
  }
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
    if (sortKey === key) {
      // Ascending, then descending, then the queue's own order again.
      // Without the third step a sort could never be undone, and the list
      // would have no way to show the order the arrows actually move things
      // in - a reorder that nothing on screen reflects looks like no-op.
      if (sortDir === -1) sortKey = null;
      else sortDir = -sortDir;
    } else {
      sortKey = key;
      sortDir = 1;
    }
    document.querySelectorAll('.th').forEach((h) => h.classList.remove('sorted'));
    if (sortKey !== null) th.classList.add('sorted');
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
  fillLimitSelect(ui.mLimit);
  fillLimitSelect(ui.lSpeed);
  showLimit(ui.mLimit, ui.mLimitCustom, 0);

  try {
    const health = await api('/api/health');
    ui.dir.textContent = health.download_dir || '';
    ui.dir.title = health.download_dir || '';
  } catch (_) {
    /* the list below reports connectivity more usefully than this would */
  }

  // Once, not on every tick: the categories are a setting, and a file edited
  // while the app runs is read again on the next launch. Failing here leaves
  // the list empty, which is the same as a build that files everything into
  // one folder - the controls are then simply not offered.
  try {
    const res = await api('/api/categories');
    CATEGORIES = Array.isArray(res.categories) ? res.categories : [];
  } catch (_) {
    CATEGORIES = [];
  }
  fillCategorySelects();

  await refresh();
}

bootstrap();
setInterval(refresh, 700);
setInterval(pollClipboard, 1000);
setInterval(pollCaptures, 600);
