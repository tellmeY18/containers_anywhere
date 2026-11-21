import { h } from './dom.js';

export function receiptRow(name, value) {
  return h('div', { class: 'ls-receipt__row' },
    h('span', { class: 'name' }, name),
    h('span', { class: 'lead', 'aria-hidden': 'true' }),
    h('span', null, value));
}

export function receipt(title, rows, extra) {
  return h('div', { class: 'ls-receipt' }, h('h3', null, title), rows.map(([n, v]) => receiptRow(n, v)), extra);
}
