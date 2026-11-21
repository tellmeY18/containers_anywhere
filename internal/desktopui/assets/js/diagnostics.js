import { h } from './dom.js';
import { call } from './api.js';
import { guard, card } from './ui.js';

export function mount(root, app, sc) {
  const log = h('pre', { class: 'ca-log', tabindex: '0', 'aria-label': 'Logs' }, 'Loading…');
  const load = async () => {
    try { log.textContent = await call('logs'); } catch (e) { log.textContent = e.message; }
  };
  root.append(card('Diagnostics',
    h('p', { class: 'ca-meta' }, 'The last part of the launcher, console and gateway logs. Share them when you ask for help, after checking them for anything private.'),
    h('div', { class: 'ca-actions' },
      h('button', { class: 'ls-btn', type: 'button', onclick: e => guard(e.target, load) }, 'Refresh'),
      h('button', { class: 'ls-btn', type: 'button', onclick: async e => { await navigator.clipboard.writeText(log.textContent).catch(() => {}); } }, 'Copy')),
    log));
  load();
}
