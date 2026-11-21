import { h, icon, clear, jsonKey } from './dom.js';
import { call, getJSON } from './api.js';
import { SECTIONS, ROUTES } from './routes.js';
import { STATES, stateKey } from './strings.js';
import { confirmSheet } from './sheet.js';
import { alert, clearAlert, callout, tag, guard } from './ui.js';
import { applyDisplay, openDisplay } from './display.js';

import * as cluster from './cluster.js';
import * as workloads from './workloads.js';
import * as recovery from './recovery.js';
import * as store from './store.js';
import * as updates from './updates.js';
import * as diagnostics from './diagnostics.js';

const modules = { cluster, workloads, recovery, store, updates, diagnostics };

applyDisplay();

const listeners = new Set();
const app = {
  state: { status: null, requests: [] },
  subscribe(fn) { listeners.add(fn); return () => listeners.delete(fn); },
  refreshRequests: () => pollRequests(true),
};
const notify = () => listeners.forEach(fn => fn(app.state));

// ---- sections ----
let current = null;
let currentId = null;

function scope() {
  const cleanup = [];
  return {
    on(fn) { cleanup.push(app.subscribe(fn)); fn(app.state); },
    every(ms, fn) { fn(); const t = setInterval(fn, ms); cleanup.push(() => clearInterval(t)); },
    dispose() { cleanup.forEach(f => f()); },
  };
}

function route(focus) {
  const id = ROUTES.includes(location.hash.slice(1)) ? location.hash.slice(1) : 'cluster';
  if (id === currentId) return;
  if (current) current.dispose();
  currentId = id;
  const main = document.getElementById('main');
  clear(main);
  current = scope();
  clearAlert('action');
  modules[id].mount(main, app, current);
  renderNav();
  const diag = document.getElementById('nav-diagnostics');
  if (id === 'diagnostics') diag.setAttribute('aria-current', 'page'); else diag.removeAttribute('aria-current');
  if (focus) main.focus();
}

let navKey = '';
function renderNav() {
  const s = app.state.status;
  const badges = {
    cluster: app.state.requests.length,
    workloads: ((s && s.gateway && s.gateway.endpoints) || []).filter(e => !e.ready).length,
    updates: s && s.appliance && s.appliance.available ? '!' : 0,
  };
  const key = jsonKey([currentId, badges]);
  if (key === navKey) return;
  navKey = key;
  const nav = document.getElementById('nav');
  clear(nav).append(h('ul', null, SECTIONS.map(sec => h('li', null,
    h('a', { href: '#' + sec.id, 'aria-current': sec.id === currentId ? 'page' : null }, sec.label,
      badges[sec.id] ? h('span', { class: 'ca-badge' }, String(badges[sec.id])) : null)))));
}

// ---- status block ----
let statusKey = '';
function renderStatus() {
  const s = app.state.status;
  const el = document.getElementById('status');
  if (!s) return;
  const key = stateKey(s);
  const st = STATES[key];
  const busy = ['starting', 'updating'].includes(key);
  const rk = jsonKey([key, s.detail, s.kiteURL, s.appliance && s.appliance.available, s.updateNote]);
  if (rk === statusKey) return;
  statusKey = rk;
  document.title = st.word + ' · containers anywhere';

  const actions = h('div', { class: 'ca-actions' });
  if (key === 'ready' && s.kiteURL) {
    actions.append(h('a', { class: 'ls-btn ls-btn--primary ls-btn--outbound', href: s.kiteURL, target: '_blank', rel: 'noopener noreferrer' }, 'Open Kite'));
  } else if (key === 'stopped') {
    actions.append(h('button', { class: 'ls-btn ls-btn--primary', type: 'button', onclick: e => guard(e.target, () => call('start', 'POST')) }, 'Start'));
  } else if (key === 'error') {
    actions.append(h('button', { class: 'ls-btn ls-btn--primary', type: 'button', onclick: e => guard(e.target, () => call('start', 'POST')) }, 'Retry'));
  } else if (key === 'setup') {
    actions.append(h('button', { class: 'ls-btn ls-btn--primary', type: 'button', onclick: e => guard(e.target, () => call('setup', 'POST')) }, 'Set up this computer'));
  } else if (key === 'restart') {
    actions.append(h('button', {
      class: 'ls-btn ls-btn--primary', type: 'button',
      onclick: async e => {
        const ok = await confirmSheet({ title: 'Restart Windows now?', body: h('p', null, 'Save your work first. Setup resumes after you sign in.'), confirm: 'Restart now' });
        if (ok) guard(e.target, () => call('restart', 'POST'));
      },
    }, 'Restart Windows'));
  }

  const a = s.appliance || {};
  clear(el).append(
    h('h1', { id: 'status-headline' }, st.word, ' ',
      h('span', { class: st.tag ? 'ls-tag ls-tag--' + st.tag : 'ls-tag' }, st.icon ? icon(st.icon, true) : null, st.label)),
    h('p', { role: 'status' }, s.detail || ''),
    actions,
    ...(a.available && !busy ? [callout('info', h('p', null, 'A new version is ready: ' + a.available + '. '), h('a', { href: '#updates' }, 'Review the update'))] : []));
}

function renderFooter() {
  const s = app.state.status;
  const a = (s && s.appliance && s.appliance.current) || 'not installed';
  document.getElementById('versions').textContent = 'beta · app ' + (s && s.app ? s.app : 'unknown') + ' · appliance ' + a;
}

// ---- polling ----
let failures = 0;
async function pollStatus() {
  try {
    app.state.status = await getJSON('status');
    failures = 0;
    clearAlert('connection');
    renderStatus();
    renderFooter();
    renderNav();
    notify();
  } catch (e) {
    if (++failures >= 2) alert('connection', 'Lost contact with the launcher: ' + e.message + '. Reopen Containers Anywhere to reconnect.', 'error', false);
  }
  setTimeout(pollStatus, 2500);
}

async function pollRequests(once) {
  try {
    const d = await getJSON('host/requests');
    const list = d.pending || [];
    if (jsonKey(list) !== jsonKey(app.state.requests)) {
      app.state.requests = list;
      renderNav();
      notify();
    }
  } catch (e) { /* the status poll reports a lost connection */ }
  if (once !== true) setTimeout(pollRequests, 4000);
}

document.getElementById('open-display').addEventListener('click', openDisplay);
window.addEventListener('hashchange', () => route(true));
route(false);
await pollStatus();
pollRequests();
