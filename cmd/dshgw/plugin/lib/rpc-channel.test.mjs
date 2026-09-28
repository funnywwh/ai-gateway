// Tests for the shared browser RPC channel mount (lib/rpc-channel.js).
//
// The transport is exercised over a real socket, because that is the part the tenant plugins cannot
// afford to get wrong: the browser caller treats a non-2xx answer as
// `transport failure for /<channel>/<endpoint>: HTTP <status>` (that message is what a tenant sees
// when the channel is not mounted at all, and the 405 the SPA fallback answers used to be), and it
// validates the 200 body before handing anything to a panel. So the statuses and the envelope are
// asserted here rather than left to a tenant's browser.

import { strict as assert } from 'node:assert'
import { createServer } from 'node:http'
import test from 'node:test'

import { callRpcRoute, createRpcRoute, endpointOf, registerRpcChannel } from './rpc-channel.js'

const CHANNEL = '/probe'

/** One route with an admission decision the test picks, and a handler the test supplies. */
function routeWith({ handler, admission = undefined, maxRequestBodyBytes } = {}) {
  return createRpcRoute({
    channel: CHANNEL,
    label: 'probe',
    admit: () => admission ?? { peer: {} },
    handler: handler ?? (async (endpoint, payload) => ({ ok: true, value: { endpoint, payload } })),
    ...(maxRequestBodyBytes === undefined ? {} : { maxRequestBodyBytes }),
  })
}

/**
 * Serve one route and a fallback the way dsh's web server does: the prefix route wins, everything
 * else reaches the fallback seat — which is the SPA server, whose non-GET/HEAD answer is 405.
 */
async function serve(route) {
  const server = createServer((req, res) => {
    const pathname = new URL(req.url ?? '/', 'http://dsh.internal').pathname
    if (pathname === route.path || pathname.startsWith(`${route.path}/`)) {
      route.handler(req, res).catch(() => res.destroy())
      return
    }
    res.writeHead(405)
    res.end()
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const base = `http://127.0.0.1:${server.address().port}`
  return {
    base,
    close: () => new Promise((resolve) => server.close(resolve)),
  }
}

/** POST one envelope the way the browser caller builds it. */
const post = (base, endpoint, body, { method = 'POST', contentType = 'application/json', rpcId = 'rpc-1', payload = {} } = {}) => fetch(
  `${base}${CHANNEL}/${endpoint}`,
  {
    method,
    headers: contentType === null ? {} : { 'content-type': contentType },
    body: method === 'POST' ? (body ?? JSON.stringify({ type: 'client-request', rpcId, method: endpoint, payload })) : undefined,
  },
)

test('endpoints: read like the browser names them, and nothing else', () => {
  assert.equal(endpointOf('/ssh-workspace', '/ssh-workspace/hosts'), 'hosts')
  assert.equal(endpointOf('/ssh-workspace', '/ssh-workspace/write-config.json'), 'write-config.json')
  // Multi-segment endpoints are legal on both sides (the harness's own channels use them, e.g.
  // `goals/create`); every segment still has to match the caller's pattern.
  assert.equal(endpointOf('/ssh-workspace', '/ssh-workspace/goals/create'), 'goals/create')
  assert.equal(endpointOf('/ssh-workspace', '/ssh-workspace'), undefined)
  assert.equal(endpointOf('/ssh-workspace', '/ssh-workspace/'), undefined)
  assert.equal(endpointOf('/ssh-workspace', '/ssh-workspace/a//b'), undefined)
  assert.equal(endpointOf('/ssh-workspace', '/ssh-workspace/%2e%2e/etc'), undefined)
  assert.equal(endpointOf('/ssh-workspace', '/ssh-workspace/a b'), undefined)
  assert.equal(endpointOf('/ssh-workspace', '/ssh-workspace-other/hosts'), undefined)
  assert.equal(endpointOf('/ssh-workspace', '/dshgw-git-diff/status'), undefined)
})

test('mount: registers a prefix route on the injected web server and unmounts with the plugin', async () => {
  const routes = new Map()
  const effects = []
  const child = {
    connection: { admit: (req) => ({ peer: { request: req } }) },
    webServer: { register: (route) => { routes.set(route.path, route); return () => routes.delete(route.path) } },
    effect: (callback, label) => { const dispose = callback(); effects.push({ label, dispose }); return dispose },
  }
  const ctx = {
    logger: { warn: () => {} },
    inject: (deps, callback) => { assert.deepEqual(deps, ['connection', 'webServer']); return callback(child) },
  }

  registerRpcChannel(ctx, { channel: CHANNEL, label: 'probe', handler: async () => ({ ok: true, value: 1 }) })

  const route = routes.get(CHANNEL)
  assert.ok(route !== undefined, 'the channel is mounted under its own path')
  assert.equal(route.kind, 'prefix', 'a prefix route, so every endpoint below it is served')
  assert.equal(effects[0].label, 'probe: browser RPC channel', 'the route is owned by one labelled effect')

  await effects[0].dispose()
  assert.equal(routes.has(CHANNEL), false, 'unloading the row takes the channel with it')
})

test('route: the browser caller gets the envelope it parses, over a real socket', async () => {
  const served = await serve(routeWith({ handler: async (endpoint, payload) => ({ ok: true, value: { endpoint, payload, cursor: 42 } }) }))
  try {
    const response = await post(served.base, 'status', null, { rpcId: 'rpc-42', payload: { workspace: '/w' } })
    assert.equal(response.status, 200)
    assert.equal(response.headers.get('content-type'), 'application/json')
    const envelope = await response.json()
    // Exactly what parseConnectionResponse (dsh-client-connection/lib/client.js) accepts:
    // type, a string rpcId matching the request, and a result the caller unwraps.
    assert.equal(envelope.type, 'server-response')
    assert.equal(envelope.rpcId, 'rpc-42')
    assert.deepEqual(envelope.result, { ok: true, value: { endpoint: 'status', payload: { workspace: '/w' }, cursor: 42 } })
  } finally {
    await served.close()
  }
})

test('route: an endpoint failure is a 200 with a coded error envelope, not a transport failure', async () => {
  const served = await serve(routeWith({ handler: async () => ({ ok: false, error: { code: 'git/no-repo', message: 'not a repository', details: {} } }) }))
  try {
    const response = await post(served.base, 'status')
    assert.equal(response.status, 200, 'the panel reads the code; only a broken transport is non-2xx')
    const envelope = await response.json()
    assert.equal(envelope.result.ok, false)
    assert.equal(envelope.result.error.code, 'git/no-repo')
    assert.equal(envelope.result.error.details !== null && typeof envelope.result.error.details === 'object', true,
      'the browser parses details as a record')
  } finally {
    await served.close()
  }
})

test('route: refusals keep the statuses the harness uses, and never reach a handler', async () => {
  let calls = 0
  const counting = (handler) => async (endpoint, payload) => { calls += 1; return await handler(endpoint, payload) }

  const rejected401 = await serve(routeWith({ admission: { rejection: 401 }, handler: counting(async () => ({ ok: true, value: 1 })) }))
  try {
    const response = await post(rejected401.base, 'status')
    assert.equal(response.status, 401)
    assert.equal(await response.text(), 'unauthorized')
  } finally {
    await rejected401.close()
  }

  const rejected403 = await serve(routeWith({ admission: { rejection: 403 }, handler: counting(async () => ({ ok: true, value: 1 })) }))
  try {
    assert.equal((await fetch(`${rejected403.base}${CHANNEL}/status`, { method: 'GET' })).status, 403)
  } finally {
    await rejected403.close()
  }

  const open = await serve(routeWith({ handler: counting(async () => ({ ok: true, value: 1 })) }))
  try {
    assert.equal((await post(open.base, 'status', null, { method: 'GET' })).status, 404, 'a non-POST is not a call')
    assert.equal((await post(open.base, 'status', null, { contentType: 'text/plain' })).status, 415)
    assert.equal((await post(open.base, 'status', 'not json')).status, 400)
    assert.equal(await (await post(open.base, 'status', 'not json')).text(), 'body is not JSON')
    assert.equal((await post(open.base, 'a//b', null)).status, 404, 'a path that is not endpoints stays unmounted')
    assert.equal((await post(open.base, 'a%20b', null)).status, 404, 'and a segment the caller would never build is not one either')
    // An encoded traversal never reaches here at all: `new URL().pathname` collapses `%2e%2e` into
    // `..` and normalizes it away, so the request is a different path entirely (the harness's own
    // route matching does the same, which is why the request below lands on the fallback seat).
    assert.equal((await post(open.base, '%2e%2e/secret', null)).status, 405)
    assert.equal(calls, 0, 'no refusal ever reached the plugin')
  } finally {
    await open.close()
  }
})

test('route: a mismatched method is answered with the harness\'s own bad-request envelope', async () => {
  const served = await serve(routeWith())
  try {
    const response = await post(served.base, 'status', JSON.stringify({ type: 'client-request', rpcId: 'rpc-7', method: 'other', payload: {} }))
    assert.equal(response.status, 200)
    const envelope = await response.json()
    assert.equal(envelope.rpcId, 'rpc-7')
    assert.equal(envelope.result.ok, false)
    assert.equal(envelope.result.error.code, 'gateway/bad-request')
  } finally {
    await served.close()
  }
})

test('route: a malformed envelope is a 400 the caller can still read', async () => {
  const served = await serve(routeWith())
  try {
    const named = await (await post(served.base, 'status', JSON.stringify({ type: 'not-a-request', rpcId: 'rpc-9' }))).json()
    assert.equal(named.rpcId, 'rpc-9', 'a usable id is echoed back')
    assert.equal(named.result.ok, false)
    assert.equal(named.result.error.code, 'gateway/bad-request')

    const anonymous = await (await post(served.base, 'status', JSON.stringify({ type: 'not-a-request' }))).json()
    assert.equal(anonymous.rpcId, 'invalid-request', 'an unusable one falls back to the harness id')
  } finally {
    await served.close()
  }
})

test('route: a plugin bug is a logged 500, not a silent hang', async () => {
  const warnings = []
  const served = await serve(createRpcRoute({
    channel: CHANNEL,
    label: 'probe',
    admit: () => ({ peer: {} }),
    logger: { warn: (message) => warnings.push(message) },
    handler: async (endpoint) => {
      if (endpoint === 'boom') throw new Error('kaboom')
      return { value: 1 }
    },
  }))
  try {
    assert.equal((await post(served.base, 'boom')).status, 500)
    assert.equal((await post(served.base, 'nonsense')).status, 500)
    assert.equal(warnings.length, 2, 'both bugs are reported where an operator can read them')
  } finally {
    await served.close()
  }
})

test('route: the body cap refuses instead of buffering', async () => {
  const served = await serve(routeWith({ maxRequestBodyBytes: 512 }))
  try {
    const big = JSON.stringify({ type: 'client-request', rpcId: 'rpc-1', method: 'status', payload: { blob: 'x'.repeat(4096) } })
    assert.equal((await post(served.base, 'status', big)).status, 413)
    const small = JSON.stringify({ type: 'client-request', rpcId: 'rpc-1', method: 'status', payload: {} })
    assert.equal((await post(served.base, 'status', small)).status, 200)
  } finally {
    await served.close()
  }
})

test('route: an aborted call reaches the endpoint\'s signal', async () => {
  const route = createRpcRoute({
    channel: CHANNEL,
    label: 'probe',
    admit: () => ({ peer: {} }),
    handler: async (endpoint, payload, signal) => await new Promise((resolve) => {
      signal.addEventListener('abort', () => resolve({ ok: true, value: { aborted: true } }), { once: true })
    }),
  })
  const controller = new AbortController()
  const pending = callRpcRoute(route, 'read', {}, { signal: controller.signal })
  setTimeout(() => controller.abort(), 20)
  assert.deepEqual(await pending, { ok: true, value: { aborted: true } })
})

test('route: refuses to be built without the pieces the transport needs', () => {
  assert.throws(() => createRpcRoute({ channel: 'probe', handler: async () => ({ ok: true, value: 1 }), admit: () => ({ peer: {} }) }), /not an absolute RPC channel/)
  assert.throws(() => createRpcRoute({ channel: CHANNEL, handler: 'nope', admit: () => ({ peer: {} }) }), /handler must be a function/)
  assert.throws(() => createRpcRoute({ channel: CHANNEL, handler: async () => ({ ok: true, value: 1 }) }), /needs the connection service's admit/)
})
