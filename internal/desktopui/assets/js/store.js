import { h, clear, jsonKey, icon } from './dom.js';
import { call, getJSON } from './api.js';
import { confirmSheet } from './sheet.js';
import { callout, guard, toast, tag, fail, card } from './ui.js';
import { receipt } from './receipt.js';

const RISK = { high: 'danger', medium: 'warning', low: 'info', blocked: 'danger' };

async function review(entry, reload) {
  let r;
  try {
    r = await getJSON('store/review?repo=' + encodeURIComponent(entry.repo));
  } catch (e) {
    fail(e);
    return;
  }
  const s = r.summary || {};
  const rows = [
    ['Risk', r.risk],
    ['Resources', (s.kinds || []).join(', ') || 'none'],
    ['Namespaces', (s.namespaces || []).join(', ') || 'none'],
    ['Images', (s.images || []).join(', ') || 'none'],
    ['Manifest', r.manifestURL],
    ['SHA-256', r.sha256],
  ];
  const ok = await confirmSheet({
    title: 'Install ' + r.repo + '?',
    body: [
      callout('warning', h('p', null, 'Unverified. Installing gives its publisher the access listed below. Read the manifest first.')),
      receipt('Review', rows),
      h('ul', null, (s.risks || []).map(k => h('li', null, (k.level === 'high' ? 'High: ' : 'Note: ') + k.text))),
    ],
    confirm: 'Install', danger: r.risk === 'high', type: r.risk === 'high' ? r.repo : null, keep: 'Do not install',
  });
  if (!ok) return;
  try {
    await call('store/install', 'POST', { repo: r.repo, sha256: r.sha256 });
    toast('Installed ' + r.repo + '. It may take a few minutes to start; follow it in Kite.');
  } catch (e) {
    fail(e);
  }
  reload();
}

function entryRow(e, installed, reload) {
  const have = installed[e.repo];
  return h('li', null,
    h('p', null, h('b', null, e.repo)),
    h('p', { class: 'ca-meta' }, e.description || 'No description'),
    h('p', null,
      h('span', { class: 'ls-chip ls-chip--unverified' }, icon('alert', true), 'Unverified'), ' ',
      tag(RISK[e.risk] || 'info', e.risk === 'low' ? 'info' : 'alert', 'risk: ' + e.risk),
      e.stars ? ' ' + e.stars + ' stars' : '', e.archived ? ' archived' : '', e.fork ? ' fork' : ''),
    have
      ? h('p', null, tag('success', 'check', 'Installed'), ' ', h('span', { class: 'ca-meta' }, have.installedAt))
      : e.risk === 'blocked'
        ? h('p', { class: 'ca-meta' }, 'Cannot be installed: ' + ((e.summary && e.summary.blocked && e.summary.blocked[0]) || 'blocked'))
        : h('div', { class: 'ca-actions' }, h('button', { class: 'ls-btn', type: 'button', onclick: () => review(e, reload) }, 'Review and install')));
}

export function mount(root, app, sc) {
  const list = h('ul', { class: 'ca-cards' });
  let key = '';
  const load = async () => {
    let d;
    try { d = await getJSON('store'); } catch (e) { return; }
    const k = jsonKey(d);
    if (k === key) return;
    key = k;
    const have = {};
    for (const i of d.installed || []) have[i.repo] = i;
    const entries = (d.catalog && d.catalog.entries) || [];
    clear(list);
    if (!entries.length) {
      list.append(h('li', null, h('div', { class: 'ls-slot' },
        h('b', null, d.loading ? 'Looking for deployments on GitHub…' : 'No deployments found yet'),
        (d.catalog && d.catalog.error) || '')));
    } else if (d.catalog.error) {
      list.append(h('li', null, callout('warning', h('p', null, d.catalog.error))));
    }
    for (const e of entries) list.append(entryRow(e, have, () => { key = ''; load(); }));
  };
  root.append(card('Store',
    callout('warning', h('p', null, 'Community deployments found on GitHub (topic containers-anywhere). Nothing here is verified: installing one runs its publisher\'s code on your cluster.')),
    list,
    h('div', { class: 'ca-actions' }, h('button', {
      class: 'ls-btn', type: 'button',
      onclick: e => guard(e.target, async () => { await call('store/refresh', 'POST'); setTimeout(() => { key = ''; load(); }, 1500); }),
    }, 'Refresh list'))));
  sc.every(15000, load);
}
