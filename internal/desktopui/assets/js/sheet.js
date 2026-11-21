import { h, icon, clear } from './dom.js';

const dialog = () => document.getElementById('sheet');

function frame(title, body, actions, close) {
  const d = dialog();
  clear(d);
  d.append(
    h('div', { class: 'ls-sheet__handle' }),
    h('div', { class: 'ls-sheet__head' },
      h('h2', { id: 'sheet-title' }, title),
      h('button', { class: 'ls-btn ls-btn--small', type: 'button', onclick: close }, 'Close')),
    h('div', { class: 'ca-sheet-body' }, body),
    h('div', { class: 'ca-sheet-actions' }, actions));
  d.onclick = e => {
    const r = d.getBoundingClientRect();
    if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) close();
  };
  if (!d.open) d.showModal();
}

// confirmSheet resolves true only when the person chooses the action. Keeping
// things as they are is the solid button; a risky action is dashed with an alert
// icon. With `type`, the action stays disabled until that text is typed.
export function confirmSheet({ title, body, confirm = 'Confirm', keep = 'Keep as is', danger = false, type = null }) {
  return new Promise(resolve => {
    const d = dialog();
    let done = false;
    const finish = v => {
      if (done) return;
      done = true;
      if (d.open) d.close();
      resolve(v);
    };
    d.onclose = () => finish(false);
    d.oncancel = () => finish(false);
    const go = h('button', { class: danger ? 'ls-btn ls-btn--danger' : 'ls-btn ls-btn--primary', type: 'button', disabled: type ? true : null, onclick: () => finish(true) },
      danger ? icon('alert', true) : null, confirm);
    const stay = h('button', { class: danger ? 'ls-btn ls-btn--primary' : 'ls-btn', type: 'button', onclick: () => finish(false) }, danger ? keep : 'Cancel');
    const kids = [].concat(body);
    if (type) {
      const input = h('input', { class: 'ls-input', id: 'sheet-type', autocomplete: 'off', spellcheck: 'false', oninput: e => { go.disabled = e.target.value !== type; } });
      kids.push(h('div', { class: 'ls-field' },
        h('label', { for: 'sheet-type' }, 'Type ' + type + ' to continue'),
        input));
    }
    frame(title, kids, danger ? [stay, go] : [go, stay], () => finish(false));
    d.querySelector(danger ? '.ls-btn--primary' : '.ls-btn--primary')?.focus();
  });
}

export function infoSheet({ title, body, actions = [] }) {
  return new Promise(resolve => {
    const d = dialog();
    const close = () => { if (d.open) d.close(); };
    d.onclose = () => resolve();
    d.oncancel = () => resolve();
    frame(title, body, [...actions, h('button', { class: 'ls-btn ls-btn--primary', type: 'button', onclick: close }, 'Done')], close);
  });
}
