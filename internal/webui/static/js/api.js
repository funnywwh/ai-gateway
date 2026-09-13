// Thin fetch wrapper for the management API. Every call is same-origin and relies
// on the HttpOnly session cookie; nothing here stores credentials.

import { apiRoot, serverRoot } from './base.js';

export class ApiError extends Error {
  constructor(message, status, code) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

// Derived from where this module was loaded, so the console works unchanged behind a
// reverse-proxy prefix (/aigw/admin/api/v1) and at the root (/admin/api/v1).
const BASE = apiRoot();

async function request(method, path, body) {
  const init = { method, credentials: 'same-origin', headers: {} };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(BASE + path, init);
  const text = await resp.text();
  let payload = null;
  if (text) {
    try { payload = JSON.parse(text); } catch (err) { payload = null; }
  }
  if (resp.status === 401) {
    window.dispatchEvent(new CustomEvent('aigw:unauthorized'));
  }
  if (!resp.ok) {
    const err = payload && payload.error ? payload.error : {};
    throw new ApiError(err.message || ('请求失败：HTTP ' + resp.status), resp.status, err.code || '');
  }
  return payload;
}

const qs = (params) => {
  if (!params) return '';
  const usable = Object.entries(params).filter(([, v]) => v !== undefined && v !== null && v !== '');
  if (!usable.length) return '';
  return '?' + usable.map(([k, v]) => encodeURIComponent(k) + '=' + encodeURIComponent(v)).join('&');
};

export const api = {
  get: (path, params) => request('GET', path + qs(params)),
  post: (path, body) => request('POST', path, body === undefined ? {} : body),
  patch: (path, body) => request('PATCH', path, body),
  put: (path, body) => request('PUT', path, body),
  del: (path) => request('DELETE', path),
  errorMessage: (err) => (err instanceof ApiError ? err.message : String(err && err.message ? err.message : err)),
};

// streamPost posts a JSON body and feeds every SSE frame to onEvent. EventSource cannot be
// used here because the console needs a POST body and the session cookie, so the frames are
// decoded by hand from the fetch body stream.
export async function streamPost(path, body, { signal, onEvent } = {}) {
  const resp = await fetch(BASE + path, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body === undefined ? {} : body),
    signal,
  });
  if (resp.status === 401) {
    window.dispatchEvent(new CustomEvent('aigw:unauthorized'));
  }
  const isStream = (resp.headers.get('content-type') || '').includes('text/event-stream');
  if (!resp.ok || !isStream) {
    const text = await resp.text();
    let payload = null;
    try { payload = JSON.parse(text); } catch (err) { payload = null; }
    const err = payload && payload.error ? payload.error : {};
    throw new ApiError(err.message || ('请求失败：HTTP ' + resp.status), resp.status, err.code || '');
  }
  if (!resp.body || typeof resp.body.getReader !== 'function') {
    throw new ApiError('当前浏览器不支持流式响应', 0, '');
  }
  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let index = buffer.indexOf('\n\n');
    while (index >= 0) {
      const frame = buffer.slice(0, index);
      buffer = buffer.slice(index + 2);
      const event = parseFrame(frame);
      if (event && onEvent) onEvent(event);
      index = buffer.indexOf('\n\n');
    }
  }
}

function parseFrame(frame) {
  let type = 'message';
  const data = [];
  for (const line of frame.split('\n')) {
    if (line.startsWith('event:')) type = line.slice(6).trim();
    else if (line.startsWith('data:')) data.push(line.slice(5).trim());
  }
  if (!data.length) return null;
  try {
    return Object.assign({ type }, JSON.parse(data.join('\n')));
  } catch (err) {
    return { type, raw: data.join('\n') };
  }
}

export function login(username, password) {
  return request('POST', '/auth/login', { username, password });
}

export function logout() {
  return request('POST', '/auth/logout', {});
}

export function me() {
  return request('GET', '/auth/me');
}

// The build identity of the server this console is talking to. It is a public endpoint
// (no session needed), which is why it is fetched with plain fetch instead of the
// management request helper: the badge must render on the login screen too, before
// there is any session to carry.
let versionPromise = null;
export function version() {
  if (!versionPromise) {
    versionPromise = fetch(serverRoot() + '/version', { credentials: 'same-origin' })
      .then((resp) => (resp.ok ? resp.json() : null))
      .catch(() => null);
  }
  return versionPromise;
}