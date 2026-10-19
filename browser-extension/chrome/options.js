'use strict';

// Firefox's `browser` namespace is the one that returns promises; Chrome's
// `chrome` does too when no callback is given. Picking one name up front lets
// the rest of this file be identical in both browsers.
const api = typeof browser !== 'undefined' ? browser : chrome;

const DEFAULTS = { intercept: true, extensions: '', connections: 8 };

const interceptBox = document.getElementById('intercept');
const extensionsBox = document.getElementById('extensions');
const connectionsBox = document.getElementById('connections');
const testButton = document.getElementById('test');
const status = document.getElementById('status');
const errorLine = document.getElementById('error');

let saveTimer = null;

async function load() {
  const config = await api.storage.sync.get(DEFAULTS);
  interceptBox.checked = config.intercept !== false;
  extensionsBox.value = config.extensions || '';
  connectionsBox.value = String(config.connections || DEFAULTS.connections);
  await ping();
}

// Storage writes are debounced because typing into the file-types box would
// otherwise fire one write per keystroke, each of which wakes the service
// worker just to notice nothing has really changed.
function save() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(async () => {
    await api.storage.sync.set({
      intercept: interceptBox.checked,
      extensions: extensionsBox.value.trim(),
      connections: Number(connectionsBox.value),
    });
  }, 250);
}

async function ping() {
  status.dataset.state = 'slow';
  status.textContent = 'Checking…';

  let reply = null;
  let failure = null;
  try {
    const res = await api.runtime.sendMessage({ cmd: 'godl-ping' });
    if (!res || !res.ok) failure = res && res.error ? res.error : 'godl did not answer.';
    else if (!res.reply || !res.reply.ok) {
      failure = res.reply && res.reply.error ? res.reply.error : 'godl did not answer.';
    } else {
      reply = res.reply;
    }
  } catch (e) {
    failure = e && e.message ? e.message : String(e);
  }

  if (reply) {
    status.dataset.state = 'ok';
    status.textContent = 'godl is running.';
    errorLine.hidden = true;
    errorLine.textContent = '';
    return;
  }

  status.dataset.state = 'bad';
  status.textContent = 'Not connected.';
  // The host's own wording is the most useful thing to show here: it is the
  // message a person would otherwise have to go hunting for in a log.
  errorLine.textContent = failure || 'godl did not answer.';
  errorLine.hidden = false;
}

for (const el of [interceptBox, extensionsBox, connectionsBox]) {
  el.addEventListener('change', save);
  if (el.tagName === 'INPUT' && el.type === 'text') el.addEventListener('input', save);
}

testButton.addEventListener('click', () => { void ping(); });

void load();
