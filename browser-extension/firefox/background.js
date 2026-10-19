// godl browser bridge.
//
// A browser cannot hand a file to a download manager on its own. It launches
// a small helper - the native messaging host - and speaks to it over a pipe.
// This file is the browser half of that pipe.
//
// Three jobs, in order of how carefully they are written:
//
//   1. Catch the download the person actually wanted, before the browser
//      finishes writing it somewhere they did not choose.
//   2. Describe it well enough for godl to fetch it identically - the same
//      cookies, the same referer, the same user agent - because a server that
//      said yes to the browser must not say no to godl.
//   3. Give it straight back to the browser if anything goes wrong. A
//      download manager that loses a file is worse than one that was not
//      asked in the first place.
//
// The responses from godl-host arrive in the order they were asked for: it
// reads one message, waits for the manager, writes the answer, and only then
// reads the next. So a plain queue of waiters is correct here, and adding
// request IDs to the protocol would be ceremony.

'use strict';

// Firefox exposes the same extensions under `browser`, and only that spelling
// returns promises. Chrome returns promises from `chrome` when no callback is
// passed, so `await` works on either - but only if the right object is picked
// up first. One name for the rest of the file is what keeps Chrome and
// Firefox running the identical code below rather than two near-copies.
const api = typeof browser !== 'undefined' ? browser : chrome;

const HOST = 'com.godl.host';

// Long enough for a cold start - godl-host launches godl.exe and waits for
// it - and short enough that a click does not look like nothing happened.
const ANSWER_TIMEOUT_MS = 30000;

const MENU_ID = 'godl-download-link';

const DEFAULTS = {
  // Hand every download in the browser to godl. Turning this off leaves the
  // right-click command as the only route, which is the quieter way to use it.
  intercept: true,
  // Comma, space or newline separated. Empty means every file type.
  extensions: '',
  // How many connections godl opens per file. 1 behaves like a plain browser.
  connections: 8,
};

/* ── The pipe ───────────────────────────────────────────────────────────── */

let port = null;
let waiters = [];

function drop(waiter) {
  const i = waiters.indexOf(waiter);
  if (i >= 0) waiters.splice(i, 1);
}

function connect() {
  if (port) return port;
  let opened;
  try {
    opened = api.runtime.connectNative(HOST);
  } catch (e) {
    // The host is registered but missing, or not registered at all. Both look
    // the same from here and are reported the same way.
    return null;
  }
  port = opened;

  port.onMessage.addListener((msg) => {
    const waiter = waiters.shift();
    if (waiter) waiter.resolve(msg);
  });

  port.onDisconnect.addListener(() => {
    // Chrome kills the service worker after thirty idle seconds and takes the
    // port with it. That is normal, so it clears the handle and lets the next
    // message open a fresh one rather than treating it as a failure.
    const why = api.runtime.lastError
      ? api.runtime.lastError.message
      : 'the connection to godl closed';
    port = null;
    for (const w of waiters.splice(0)) w.reject(new Error(why));
  });

  return port;
}

function ask(message, timeoutMs) {
  const open = connect();
  if (!open) {
    return Promise.reject(
      new Error('godl could not be reached. Open godl.exe once to finish setting it up.')
    );
  }
  return new Promise((resolve, reject) => {
    const waiter = { resolve: null, reject: null };
    const timer = setTimeout(() => {
      drop(waiter);
      reject(new Error('godl did not answer in time.'));
    }, timeoutMs || ANSWER_TIMEOUT_MS);

    waiter.resolve = (value) => { clearTimeout(timer); resolve(value); };
    waiter.reject = (err) => { clearTimeout(timer); reject(err); };

    waiters.push(waiter);
    try {
      open.postMessage(message);
    } catch (e) {
      drop(waiter);
      waiter.reject(e);
    }
  });
}

/* ── Turning a browser download into a godl request ─────────────────────── */

function schemeOf(url) {
  const text = String(url || '');
  const i = text.indexOf('://');
  return i > 0 ? text.slice(0, i + 1).toLowerCase() : '';
}

function baseName(pathOrUrl) {
  if (!pathOrUrl) return '';
  let s = String(pathOrUrl);
  const cut = s.search(/[?#]/);
  if (cut >= 0) s = s.slice(0, cut);
  s = s.split(/[/\\]/).pop() || '';
  try {
    return decodeURIComponent(s);
  } catch (e) {
    return s;
  }
}

function typeIsWanted(config, filename) {
  const list = String(config.extensions || '')
    .toLowerCase()
    .split(/[\s,;]+/)
    .filter(Boolean);
  if (list.length === 0) return true;

  const dot = String(filename).lastIndexOf('.');
  const ext = dot >= 0 ? String(filename).slice(dot + 1).toLowerCase() : '';
  return list.indexOf(ext) >= 0;
}

function clampConnections(value) {
  const n = Math.round(Number(value));
  if (!Number.isFinite(n)) return DEFAULTS.connections;
  return Math.min(32, Math.max(1, n));
}

// The browser sends a bare Cookie header, never the jar. Rebuilding it here
// is what keeps a signed-in or expiring download working once godl fetches
// it a second later with no session of its own.
async function cookieHeaderFor(url) {
  let cookies;
  try {
    cookies = await api.cookies.getAll({ url });
  } catch (e) {
    return '';
  }
  return cookies.map((c) => c.name + '=' + c.value).join('; ');
}

async function handToGodl(item, config) {
  const url = item.finalUrl || item.url;
  const filename = baseName(item.filename) || baseName(url);

  return ask({
    cmd: 'add',
    url,
    filename,
    referer: item.referrer || '',
    userAgent: navigator.userAgent,
    cookie: await cookieHeaderFor(url),
    connections: clampConnections(config.connections),
  });
}

/* ── Catching downloads ─────────────────────────────────────────────────── */

function report(err) {
  // The native host writes to the browser's extension log, which nobody reads
  // unless something is already wrong. A console line is the cheap half of
  // diagnosing a handover that did not happen.
  console.warn('[godl]', err && err.message ? err.message : err);
}

api.downloads.onCreated.addListener((item) => {
  handleNewDownload(item).catch(report);
});

async function handleNewDownload(item) {
  if (!item || item.byExtensionId === api.runtime.id) return;

  const config = await api.storage.sync.get(DEFAULTS);
  if (!config.intercept) return;
  if (item.state !== 'in_progress') return;

  const url = item.finalUrl || item.url;
  const scheme = schemeOf(url);
  if (scheme !== 'http:' && scheme !== 'https:') return;

  const filename = baseName(item.filename) || baseName(url);
  if (!filename || !typeIsWanted(config, filename)) return;

  // Cancel first, then ask. Waiting would let the browser finish writing a
  // duplicate of the file godl is about to fetch, and the two would race for
  // the same disk. If godl refuses, resume puts the browser's copy straight
  // back - it is interrupted, not deleted, until erase is called below.
  try {
    await api.downloads.cancel(item.id);
  } catch (e) {
    // Already finished, or already cancelled by the person. Either way there
    // is nothing left to take over.
    return;
  }

  // A small file on a fast connection can finish between the event above and
  // the cancel landing. cancel() on a finished download succeeds without
  // stopping anything, and taking it over from there would leave the
  // browser's finished copy on disk with no record of it. Check, because the
  // only reliable answer is the item's state after the fact.
  const still = await api.downloads.search({ id: item.id });
  if (!still.length || still[0].state !== 'interrupted') return;

  let reply;
  try {
    reply = await handToGodl(item, config);
  } catch (e) {
    await putBackInBrowser(item, e);
    return;
  }

  if (!reply || !reply.ok) {
    await putBackInBrowser(
      item,
      new Error((reply && reply.error) || 'godl refused this download.')
    );
    return;
  }

  // Only now is the browser's row removed. Erasing before a successful handover
  // is how a failed download disappears from everywhere at once.
  try {
    await api.downloads.erase({ id: item.id });
  } catch (e) {
    // The file is safely with godl; a leftover line in the download list is a
    // cosmetic problem, not a lost transfer.
  }
}

async function putBackInBrowser(item, why) {
  report(why);
  if (!item) return;
  try {
    await api.downloads.resume(item.id);
  } catch (e) {
    // resume only works on an interrupted download. If the browser already
    // moved on there is nothing to restore, and nothing lost either.
  }
}

/* ── The manual route ───────────────────────────────────────────────────── */

async function refreshMenu() {
  const config = await api.storage.sync.get(DEFAULTS);
  // With interception on, right-clicking a link would start the same download
  // twice. The command is the fallback for when interception is off, so it is
  // only offered when it is not already happening by itself.
  try {
    await api.contextMenus.update(MENU_ID, { visible: !config.intercept });
  } catch (e) {
    // Not created yet - onInstalled below creates it.
  }
}

api.runtime.onInstalled.addListener(() => {
  api.contextMenus.create({
    id: MENU_ID,
    title: 'Download with godl',
    contexts: ['link'],
  });
  refreshMenu();
});

api.contextMenus.onClicked.addListener((info, tab) => {
  if (info.menuItemId !== MENU_ID || !info.linkUrl) return;
  downloadLinkNow(info.linkUrl, tab && tab.url ? tab.url : '');
});

// A link clicked by hand is offered to godl without cancelling anything,
// because there is nothing of the download to cancel yet.
async function downloadLinkNow(url, pageUrl) {
  if (schemeOf(url) !== 'http:' && schemeOf(url) !== 'https:') return;
  const config = await api.storage.sync.get(DEFAULTS);
  const reply = await ask({
    cmd: 'add',
    url,
    filename: baseName(url),
    referer: pageUrl,
    userAgent: navigator.userAgent,
    cookie: await cookieHeaderFor(url),
    connections: clampConnections(config.connections),
  }).catch((e) => ({ ok: false, error: e.message }));

  if (!reply || !reply.ok) {
    report(new Error((reply && reply.error) || 'godl did not take the link.'));
  }
}

/* ── Options ────────────────────────────────────────────────────────────── */

api.storage.onChanged.addListener((changes, area) => {
  if (area !== 'sync') return;
  if (Object.prototype.hasOwnProperty.call(changes, 'intercept')) refreshMenu();
});

// The options page runs in its own document and cannot hold the port, so it
// reaches the same two functions through the messaging below.
api.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (!msg || msg.cmd !== 'godl-ping') return false;

  ask({ cmd: 'ping' })
    .then((reply) => sendResponse({ ok: true, reply }))
    .catch((e) => sendResponse({ ok: false, error: e.message }));
  return true; // the answer is asynchronous
});
