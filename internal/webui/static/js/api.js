// Thin fetch wrapper for the management API. Every call is same-origin and relies
// on the HttpOnly session cookie; nothing here stores credentials.

export class ApiError extends Error {
  constructor(message, status, code) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

const BASE = '/admin/api/v1';

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

export function login(username, password) {
  return request('POST', '/auth/login', { username, password });
}

export function logout() {
  return request('POST', '/auth/logout', {});
}

export function me() {
  return request('GET', '/auth/me');
}