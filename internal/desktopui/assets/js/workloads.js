import { h, clear, jsonKey, safeURL } from './dom.js';
import { tag, callout, card } from './ui.js';

function row(endpoint) {
  const href = endpoint.ready ? safeURL(endpoint.url) : null;
  if (endpoint.ready) {
    return h('li', { class: 'ca-row' },
      href ? h('a', { class: 'ls-out ca-mono', href, target: '_blank', rel: 'noopener noreferrer' }, endpoint.url) : h('span', { class: 'ca-mono' }, endpoint.url),
      h('p', { class: 'ca-meta' }, tag('success', 'check', 'responding'), ' ',
        endpoint.advertised
          ? 'Announced on your network (mDNS). Client reachability and certificate trust still need checking on each device.'
          : 'Responding on the configured addresses. Client DNS and certificate trust still need checking on each device.'));
  }
  return h('li', { class: 'ca-row' },
    h('span', { class: 'ca-mono' }, endpoint.url),
    h('p', { class: 'ca-meta' }, tag('warning', 'clock', 'not ready'), ' ', endpoint.error || 'Checking workload'));
}

export function mount(root, app, sc) {
  const problems = h('div');
  const list = h('ul', { class: 'ca-list' });
  let key = '';
  root.append(card('Workloads', problems, list));
  sc.on(state => {
    const gateway = state.status && state.status.gateway;
    const k = jsonKey(gateway || null);
    if (k === key) return;
    key = k;
    clear(problems);
    const issues = gateway ? [gateway.error, gateway.processError, gateway.discoveryError].filter(Boolean) : [];
    if (issues.length) problems.append(callout('warning', h('p', null, 'Gateway: ' + issues.join(' · '))));
    clear(list);
    const endpoints = (gateway && gateway.endpoints) || [];
    if (!endpoints.length) {
      list.append(h('li', null, h('div', { class: 'ls-slot' }, h('b', null, 'No workloads published yet'),
        'Install one from the Store, or open Kite once the cluster is ready.')));
    }
    for (const endpoint of endpoints) list.append(row(endpoint));
  });
}
