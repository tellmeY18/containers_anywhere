import { h } from './dom.js';
import { infoSheet } from './sheet.js';

const KEY = 'ca-display';
const FIELDS = [
  { name: 'theme', label: 'Theme', options: [['', 'Follow this computer'], ['light', 'Light'], ['dark', 'Dark']] },
  { name: 'text', label: 'Text size', options: [['', 'Normal'], ['large', 'Large']] },
  { name: 'contrast', label: 'Contrast', options: [['', 'Normal'], ['high', 'High']] },
  { name: 'colour', label: 'Status colours', options: [['', 'Normal'], ['reduced', 'Reduced (words and icons only)']] },
];

function load() {
  try { return JSON.parse(localStorage.getItem(KEY)) || {}; } catch (e) { return {}; }
}

export function applyDisplay() {
  const v = load();
  for (const f of FIELDS) {
    const attr = 'data-' + f.name;
    if (v[f.name]) document.documentElement.setAttribute(attr, v[f.name]);
    else document.documentElement.removeAttribute(attr);
  }
}

export function openDisplay() {
  const v = load();
  const body = FIELDS.map(f => h('div', { class: 'ls-field' },
    h('label', { for: 'display-' + f.name }, f.label),
    h('select', {
      class: 'ls-select', id: 'display-' + f.name,
      onchange: e => {
        const cur = load();
        cur[f.name] = e.target.value;
        localStorage.setItem(KEY, JSON.stringify(cur));
        applyDisplay();
      },
    }, f.options.map(([value, label]) => h('option', { value, selected: (v[f.name] || '') === value ? true : null }, label)))));
  return infoSheet({ title: 'Display', body });
}
