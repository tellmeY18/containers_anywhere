import { h, clear, jsonKey } from './dom.js';
import { call } from './api.js';
import { confirmSheet } from './sheet.js';
import { copyButton, guard, toast, card } from './ui.js';
import { HOST_ASK, STATES, stateKey } from './strings.js';

function askText(r) {
  try {
    return (HOST_ASK[r.spec.type] || (() => String(r.spec.type)))(r.spec);
  } catch (e) {
    return String(r.spec && r.spec.type);
  }
}

function requestRow(r, reload) {
  const what = askText(r);
  const decide = verb => async e => {
    if (verb === 'approve') {
      const ok = await confirmSheet({
        title: 'Approve this request?',
        body: [h('p', null, h('b', null, r.namespace + '/' + r.name), ' asks to:'), h('p', null, what)],
        confirm: 'Approve',
      });
      if (!ok) return;
    }
    await guard(e.target, async () => {
      await call('host/' + verb, 'POST', { uid: r.uid });
      toast(verb === 'approve' ? 'Approved' : 'Rejected');
    });
    reload();
  };
  return h('li', { class: 'ca-row' },
    h('p', null, h('b', null, r.namespace + '/' + r.name), ' asks: ', what),
    r.spec.reason ? h('p', { class: 'ca-meta' }, 'Reason given: ' + r.spec.reason) : null,
    h('div', { class: 'ca-actions' },
      h('button', { class: 'ls-btn', type: 'button', onclick: decide('approve') }, 'Approve'),
      h('button', { class: 'ls-btn', type: 'button', onclick: decide('reject') }, 'Reject')));
}

export function mount(root, app, sc) {
  const facts = h('dl', { class: 'ca-kv' });
  const requests = h('ul', { class: 'ca-list' });
  let factsKey = '', reqKey = '';
  root.append(h('div', { class: 'ca-grid' },
    card('Cluster',
    facts,
    h('div', { class: 'ca-actions' },
      h('button', {
        class: 'ls-btn', type: 'button',
        onclick: e => guard(e.target, async () => { await call('stop', 'POST'); toast('Cluster stopped'); }),
      }, 'Stop cluster'),
      h('button', {
        class: 'ls-btn', type: 'button',
        onclick: async e => {
          const ok = await confirmSheet({
            title: 'Quit Containers Anywhere?',
            body: h('p', null, 'This stops your cluster, then closes this panel. Your data stays on this computer.'),
            confirm: 'Stop and quit',
          });
          if (ok) guard(e.target, async () => { await call('quit', 'POST'); document.title = 'Closed'; toast('Closed. You can close this tab.'); });
        },
      }, 'Quit'))),
    card('Requests from workloads',
    h('p', { class: 'ca-meta' }, 'Workloads can ask this computer for things. Nothing happens until you approve it here.'),
    requests)));

  sc.on(state => {
    const s = state.status;
    if (s) {
      const key = jsonKey([s.phase, s.state, s.appliance && s.appliance.current]);
      if (key !== factsKey) {
        factsKey = key;
        clear(facts).append(
          h('dt', null, 'State'), h('dd', null, STATES[stateKey(s)].word),
          h('dt', null, 'Appliance'), h('dd', null, (s.appliance && s.appliance.current) || 'not installed yet'),
          h('dt', null, 'Data folder'), h('dd', { class: 'ca-mono' }, s.state || '', ' ', s.state ? copyButton(s.state) : null));
      }
    }
    const k = jsonKey(state.requests);
    if (k !== reqKey) {
      reqKey = k;
      clear(requests);
      if (!state.requests.length) requests.append(h('li', { class: 'ca-row' }, 'No requests are waiting.'));
      for (const r of state.requests) requests.append(requestRow(r, app.refreshRequests));
    }
  });
}
