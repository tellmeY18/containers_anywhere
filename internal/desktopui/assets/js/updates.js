import { h, jsonKey, clear } from './dom.js';
import { call } from './api.js';
import { confirmSheet } from './sheet.js';
import { callout, guard, toast, card } from './ui.js';
import { WARN } from './strings.js';

const BUSY = ['starting', 'updating', 'setup', 'preparing update'];

export function mount(root, app, sc) {
  const simple = h('div');
  const status = h('p', { role: 'status' });
  const buttons = {};
  let key = '';

  const advanced = op => async e => {
    const ok = await confirmSheet({
      title: { prepare: 'Prepare an update?', activate: 'Activate this candidate?', commit: 'Commit this update?', cancel: 'Cancel preparation?' }[op],
      body: callout(op === 'activate' || op === 'commit' ? 'warning' : 'info', h('p', null, WARN[op])),
      confirm: { prepare: 'Prepare', activate: 'Activate', commit: 'Commit', cancel: 'Cancel preparation' }[op],
      danger: op === 'activate' || op === 'cancel', keep: 'Keep as is',
    });
    if (!ok) return;
    await guard(e.target, async () => {
      const out = await call('update/' + op, 'POST', { external_effects: op === 'activate', workloads_validated: op === 'commit' });
      toast(out && out !== 'Done' ? out : 'Done');
    });
  };
  for (const [op, label] of [['prepare', 'Prepare appliance update'], ['activate', 'Activate or retry candidate'], ['commit', 'Commit validated update'], ['cancel', 'Cancel preparation']]) {
    buttons[op] = h('button', { class: 'ls-btn', type: 'button', onclick: advanced(op) }, label);
  }

  root.append(h('div', { class: 'ca-grid' },
    card('Update', simple),
    card('Advanced updates',
      h('p', { class: 'ca-meta' }, 'Prepare, try and commit an update step by step. This makes a safety copy of your data first.'),
      h('details', { class: 'ca-more' },
        h('summary', null, 'Show the advanced steps'),
        h('div', null, status, h('div', { class: 'ca-actions' }, Object.values(buttons)))))));

  sc.on(state => {
    const s = state.status;
    if (!s) return;
    const k = jsonKey([s.phase, s.appliance, s.update, s.updateError, s.updateNote]);
    if (k === key) return;
    key = k;
    const a = s.appliance || {};
    clear(simple);
    if (s.updateNote) simple.append(callout('warning', h('p', null, s.updateNote)));
    if (a.available && a.lastSwap && a.lastSwap.phase === 'rolled-back' && a.lastSwap.version === a.available) {
      simple.append(callout('info', h('p', null, 'The last update did not work and was undone. Your cluster is running as before.')));
    }
    if (a.available) {
      const busy = BUSY.includes(s.phase);
      simple.append(
        h('p', null, 'A new version is ready: ' + a.available + ' (you have ' + a.current + '). Your data stays where it is.'),
        h('div', { class: 'ca-actions' }, h('button', {
          class: 'ls-btn ls-btn--primary', type: 'button', disabled: busy ? true : null,
          onclick: async e => {
            const ok = await confirmSheet({ title: 'Update now?', body: h('p', null, WARN.swap), confirm: 'Update now' });
            if (ok) guard(e.target, () => call('update/swap', 'POST'));
          },
        }, 'Update now')));
    } else {
      simple.append(h('p', null, a.current ? 'You are on the version this app recommends: ' + a.current + '.' : 'No appliance is installed yet.'));
    }
    const p = s.update && s.update.phase;
    status.textContent = s.updateError || (p
      ? 'Update: ' + p + '. Recovery is local only. Candidate execution can write to external systems; there is no automatic disk rollback.'
      : 'App replacement keeps the selected appliance. Appliance updates need explicit preparation and activation.');
    buttons.activate.hidden = !['prepared', 'activating', 'candidate-running'].includes(p);
    buttons.commit.hidden = p !== 'candidate-running';
    buttons.cancel.hidden = !['preparing', 'prepared'].includes(p);
  });
}
