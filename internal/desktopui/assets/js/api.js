import { ROUTES } from './routes.js';

// The launcher puts its capability in the URL fragment. Take it once, keep it for
// this tab only, and clear it so it is not left in history. Anything that is not a
// known section name is the token.
let token = sessionStorage.getItem('token');
const fragment = location.hash.slice(1);
if (fragment && !ROUTES.includes(fragment)) {
  token = fragment;
  sessionStorage.setItem('token', token);
  history.replaceState(null, '', location.pathname);
}

export async function call(path, method = 'GET', body) {
  const r = await fetch('/' + path, {
    method,
    headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await r.text();
  if (!r.ok) throw Error(text || r.statusText);
  return text;
}

export async function getJSON(path) {
  return JSON.parse(await call(path));
}
