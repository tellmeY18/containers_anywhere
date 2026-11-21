// Every node is built with textContent and attributes. Names, reasons and URLs
// from workloads are never parsed as markup.
export function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  el.append(...kids.flat(Infinity).filter(k => k != null && k !== false));
  return el;
}

const SVG = 'http://www.w3.org/2000/svg';
const PATHS = {
  alert: ['M12 3 22 21H2Z', 'M12 10v5M12 17.5v1'],
  check: ['M4 12.5 9.5 18 20 6.5'],
  clock: ['M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Z', 'M12 7v5.5l3.5 2'],
  info: ['M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Z', 'M12 11v6M12 7.5v1'],
};

export function icon(name, small) {
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('class', small ? 'ls-icon ls-icon--sm' : 'ls-icon');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  for (const d of PATHS[name]) {
    const p = document.createElementNS(SVG, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}

export function hidden(text) {
  return h('span', { class: 'ls-vh' }, text);
}

export function safeURL(u) {
  try {
    const x = new URL(u);
    return x.protocol === 'https:' || x.protocol === 'http:' ? x.href : null;
  } catch (e) {
    return null;
  }
}

export function clear(el) {
  el.replaceChildren();
  return el;
}

export function jsonKey(v) {
  return JSON.stringify(v);
}
