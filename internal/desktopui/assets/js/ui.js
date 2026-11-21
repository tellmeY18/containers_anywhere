import { h, icon, hidden, clear } from './dom.js';

const alerts = () => document.getElementById('alerts');
const toasts = () => document.getElementById('toasts');

// alert shows a persistent callout. The same key replaces the earlier one.
export function alert(key, message, kind = 'error', dismissible = true) {
  clearAlert(key);
  const word = { error: 'Error', warning: 'Warning', info: 'Note' }[kind];
  const ic = { error: 'alert', warning: 'alert', info: 'info' }[kind];
  alerts().append(h('div', { class: 'ls-callout ls-callout--' + kind, 'data-key': key, role: kind === 'error' ? 'alert' : 'status' },
    icon(ic),
    h('div', null, h('p', null, hidden(word + ': '), message),
      dismissible ? h('button', { class: 'ls-btn ls-btn--small', type: 'button', onclick: () => clearAlert(key) }, 'Dismiss') : null)));
}

export function clearAlert(key) {
  for (const el of alerts().querySelectorAll('[data-key]')) if (el.dataset.key === key) el.remove();
}

export function toast(message) {
  const t = h('div', { class: 'ls-toast' }, icon('check', true), message);
  toasts().append(t);
  setTimeout(() => t.remove(), 4000);
}

export function callout(kind, ...kids) {
  const ic = { error: 'alert', warning: 'alert', info: 'info', success: 'check' }[kind];
  return h('div', { class: 'ls-callout ls-callout--' + kind }, icon(ic), h('div', null, kids));
}

export function tag(kind, ic, text) {
  return h('span', { class: 'ls-tag ls-tag--' + kind }, ic ? icon(ic, true) : null, text);
}

export function copyButton(text, label = 'Copy') {
  return h('button', {
    class: 'ls-btn ls-btn--small', type: 'button',
    onclick: async e => {
      try {
        await navigator.clipboard.writeText(text);
        toast('Copied');
      } catch (x) {
        alert('copy', 'Copying is not available here. Select the text and copy it yourself.', 'warning');
      }
    },
  }, label);
}

export function fail(e) {
  alert('action', e && e.message ? e.message : String(e));
}

// guard runs an action, shows any failure, and keeps the button from being pressed twice.
export async function guard(button, fn) {
  if (button) button.disabled = true;
  try {
    return await fn();
  } catch (e) {
    fail(e);
  } finally {
    if (button) button.disabled = false;
  }
}

export { clear };

export function card(title, ...kids) {
  return h('section', { class: 'ls-card' }, title ? h('h2', null, title) : null, kids);
}

export function wide(el) {
  el.classList.add('ca-wide');
  return el;
}
