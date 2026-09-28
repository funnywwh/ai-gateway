// The browser-facing RPC channel every tenant plugin mounts, spoken on the web server directly.
//
// Why not `ctx.connection.rpc.handle`: on dsh-0.1.7-alpha.2 that API cannot mount a channel for
// anyone. `HostConnectionService`'s `rpc` getter closes over the *service's own* context
// (`const owner = this.ctx`) and `register()` then reads `owner.webServer` — a service that
// context never injected — so every call throws
//   cannot get property "webServer" without inject
// and no route is ever mounted. Wrapping the call in `ctx.inject(['connection', 'webServer'], …)`
// does not help either, because the owner is the connection plugin's context and not the caller's
// (both shapes cost a measured debugging session on 0.1.7-alpha.2; 0.1.2-rc.1 read a context that
// did resolve `webServer`, which is why this only broke on the upgrade). The browser then POSTs to
// a path no route owns, the frontend-static fallback seat answers `405 Method Not Allowed` to
// anything that is not GET/HEAD, and every panel dies with
//   transport failure for /<channel>/<endpoint>: HTTP 405.
//
// So this module mounts the channel itself, on exactly the primitives the harness uses for its own
// browser routes:
//
//   * `webServer.register({ kind: 'prefix', path: channel, handler })` — the same route table that
//     serves `/api` and whose fallback seat otherwise answers 405.
//   * `connection.admit(request)` — the Host/Origin fence plus browser authentication, documented as
//     the call to make from "another Web route"; it answers 401/403 exactly like `/api` does.
//   * the connection envelope itself: `POST <channel>/<endpoint>` carrying
//     `{ type: 'client-request', rpcId, method, payload }` and answered with
//     `{ type: 'server-response', rpcId, result }` where `result` is `{ ok: true, value }` or
//     `{ ok: false, error: { code, message, details } }`.
//
// Nothing on the browser side changes: `ctx.connection.rpc.call(channel, endpoint, payload)` builds
// that URL from the page's own base, correlates `rpcId`, and validates the envelope in
// `parseConnectionResponse` — the status codes and bodies below are the ones it already accepts.

/** Channel shape the browser caller accepts (`/^\/[A-Za-z0-9._~-]+$/` there). */
const CHANNEL_PATTERN = /^\/[A-Za-z0-9._~-]+$/

/** One endpoint path segment; the browser caller refuses anything else before sending. */
const ENDPOINT_SEGMENT_PATTERN = /^[A-Za-z0-9_$.-]+$/

/** The only request content type the transport answers. */
const CONTENT_TYPE = 'application/json'

/** Envelope id the harness uses when a request carries no usable `rpcId`. */
const INVALID_REQUEST_RPC_ID = 'invalid-request'

/**
 * Default body cap. The panels send paths, terminal keystrokes and bounded base64 file chunks, so
 * this is headroom rather than a working limit; a request above it is answered 413 and dropped
 * instead of being buffered without bound.
 */
const DEFAULT_MAX_REQUEST_BYTES = 8 * 1024 * 1024

/** A failure the caller reads as an error envelope rather than a transport failure. */
function errorEnvelope(rpcId, code, message, details = {}) {
  return { type: 'server-response', rpcId, result: { ok: false, error: { code, message, details } } }
}

/** One JSON response with the given status. */
function sendJson(res, status, body) {
  const text = JSON.stringify(body)
  res.writeHead(status, { 'content-type': CONTENT_TYPE, 'content-length': Buffer.byteLength(text) })
  res.end(text)
}

/** The endpoint a path names below the channel, or undefined when the path is not one. */
export function endpointOf(channel, pathname) {
  if (!pathname.startsWith(`${channel}/`)) return undefined
  const endpoint = pathname.slice(channel.length + 1)
  if (endpoint.split('/').some((segment) => segment === '' || segment === '.' || segment === '..' || !ENDPOINT_SEGMENT_PATTERN.test(segment))) return undefined
  return endpoint
}

/**
 * Read one buffered request body, refusing anything above `limit` the way the harness's own bridge
 * does (413, close, destroy) rather than buffering it.
 *
 * @returns the body text, or undefined when the request was refused and answered already.
 */
async function readBody(req, res, limit) {
  const declared = req.headers['content-length']
  if (declared !== undefined && Number(declared) > limit) {
    res.writeHead(413, { connection: 'close' })
    res.end()
    req.destroy?.()
    return undefined
  }
  const chunks = []
  let received = 0
  for await (const chunk of req) {
    received += chunk.length
    if (received > limit) {
      res.writeHead(413, { connection: 'close' })
      res.end()
      req.destroy?.()
      return undefined
    }
    chunks.push(chunk)
  }
  return Buffer.concat(chunks).toString('utf8')
}

/**
 * Build the web-server route of one plugin channel.
 *
 * @param options - the channel path, the endpoint handler, and the admission call.
 * @param options.channel - absolute channel such as `/ssh-workspace`.
 * @param options.handler - `(endpoint, payload, signal) => { ok, value } | { ok: false, error }`.
 * @param options.admit - `connection.admit`, the fence plus browser authentication for one request.
 * @param options.label - plugin name used in logs and route diagnostics.
 * @param options.logger - optional cordis logger; a handler bug is warned here.
 * @param options.maxRequestBodyBytes - body cap, see {@link DEFAULT_MAX_REQUEST_BYTES}.
 * @returns the route to register on the web server.
 */
export function createRpcRoute({ channel, handler, admit, label = channel, logger, maxRequestBodyBytes = DEFAULT_MAX_REQUEST_BYTES }) {
  if (!CHANNEL_PATTERN.test(channel)) throw new Error(`${label}: ${JSON.stringify(channel)} is not an absolute RPC channel`)
  if (typeof handler !== 'function') throw new Error(`${label}: the channel handler must be a function`)
  if (typeof admit !== 'function') throw new Error(`${label}: the channel needs the connection service's admit()`)

  return {
    kind: 'prefix',
    path: channel,
    handler: async (req, res) => {
      const admission = admit(req)
      if ('rejection' in admission) {
        res.writeHead(admission.rejection)
        res.end(admission.rejection === 401 ? 'unauthorized' : 'forbidden')
        return
      }

      /* v8 ignore next -- node:http always sets url/method on server requests. */
      const endpoint = endpointOf(channel, new URL(req.url ?? '/', 'http://dsh.internal').pathname)
      if (req.method !== 'POST' || endpoint === undefined) {
        res.writeHead(404)
        res.end('not found')
        return
      }
      if ((req.headers['content-type'] ?? '').split(';', 1)[0].trim().toLowerCase() !== CONTENT_TYPE) {
        res.writeHead(415)
        res.end(`content type must be ${CONTENT_TYPE}`)
        return
      }

      const text = await readBody(req, res, maxRequestBodyBytes)
      if (text === undefined) return
      let message
      try {
        message = JSON.parse(text)
      } catch {
        res.writeHead(400)
        res.end('body is not JSON')
        return
      }
      const rpcId = message !== null && typeof message === 'object' && typeof message.rpcId === 'string' ? message.rpcId : INVALID_REQUEST_RPC_ID
      if (message === null || typeof message !== 'object' || message.type !== 'client-request' || typeof message.method !== 'string') {
        sendJson(res, 400, errorEnvelope(rpcId, 'gateway/bad-request', 'invalid client-request message', { issues: [`${label}: expected { type: "client-request", rpcId, method, payload }`] }))
        return
      }
      if (message.method !== endpoint) {
        sendJson(res, 200, errorEnvelope(rpcId, 'gateway/bad-request', `method ${JSON.stringify(message.method)} does not match endpoint ${JSON.stringify(endpoint)}`))
        return
      }

      // A closed connection aborts the call, so an endpoint that streams or batches can stop;
      // the same signal contract the harness's own channel gives its handlers.
      const abort = new AbortController()
      res.on('close', () => {
        if (!res.writableEnded) abort.abort()
      })

      let result
      try {
        result = await handler(endpoint, message.payload, abort.signal)
      } catch (error) {
        logger?.warn?.(`${label}: ${endpoint} threw: ${String(error?.stack ?? error)}`)
        res.writeHead(500)
        res.end(`handler failure: ${String(error)}`)
        return
      }
      if (result === null || typeof result !== 'object' || typeof result.ok !== 'boolean') {
        logger?.warn?.(`${label}: ${endpoint} returned something that is not an RPC result`)
        res.writeHead(500)
        res.end(`handler failure: ${label} ${endpoint} returned an invalid result`)
        return
      }
      sendJson(res, 200, { type: 'server-response', rpcId, result })
    },
  }
}

/**
 * Mount one plugin's browser RPC channel, owned by the calling plugin's fiber.
 *
 * @param ctx - the plugin's context.
 * @param options - see {@link createRpcRoute}, minus `admit`: the injected child supplies it.
 * @returns a disposer that unmounts the channel (also run when the plugin unloads).
 */
export function registerRpcChannel(ctx, options) {
  // `webServer` is a sibling service of this row, so it has to be injected into a child context
  // before either it or `connection.admit` can be read (cordis resolves services through the
  // context that names them). This is also the shape the harness's own browser routes register in.
  return ctx.inject(['connection', 'webServer'], (child) => child.effect(() => {
    const route = createRpcRoute({
      ...options,
      logger: options.logger ?? ctx.logger,
      admit: (request) => child.connection.admit(request),
    })
    return child.webServer.register(route)
  }, `${options.label ?? options.channel}: browser RPC channel`))
}

/**
 * Drive one registered channel the way the browser does, without a socket: build the envelope, feed
 * it to the route, and parse the response with the browser's own rules
 * (`parseConnectionResponse` in @deepseek-ai/dsh-client-connection/lib/client.js).
 *
 * Tests use this so a plugin row can be exercised end to end — routing, admission, envelope — from
 * a fake context, and so a change in the wire shape fails here instead of in a tenant's browser.
 *
 * @param route - the route object `createRpcRoute` returned.
 * @param endpoint - endpoint such as `hosts`.
 * @param payload - endpoint payload.
 * @param options - optional `rpcId`, HTTP `method`, request `headers`, and an AbortSignal that
 *   stands in for the browser dropping the connection (it closes the response, as the real socket
 *   does, so a long-polling endpoint sees its signal abort).
 * @returns the parsed `{ ok, value }` / `{ ok: false, error }` the browser caller would return.
 */
export async function callRpcRoute(route, endpoint, payload, { rpcId = '00000000-0000-4000-8000-000000000000', method = 'POST', headers = {}, signal } = {}) {
  const text = JSON.stringify({ type: 'client-request', rpcId, method: endpoint, payload })
  const request = {
    method,
    url: `${route.path}/${endpoint}`,
    headers: { 'content-type': CONTENT_TYPE, 'content-length': String(Buffer.byteLength(text)), ...headers },
    destroy: () => {},
    async *[Symbol.asyncIterator]() {
      yield Buffer.from(text)
    },
  }
  const response = await new Promise((resolve, reject) => {
    let status
    let responseHeaders = {}
    let body = ''
    let settled = false
    const closeListeners = new Set()
    const res = {
      writableEnded: false,
      writeHead(code, head) {
        status = code
        responseHeaders = head ?? {}
        return this
      },
      end(chunk = '') {
        this.writableEnded = true
        body += typeof chunk === 'string' ? chunk : Buffer.from(chunk).toString('utf8')
        if (!settled) {
          settled = true
          resolve({ status, headers: responseHeaders, body })
        }
      },
      on(event, listener) {
        if (event === 'close') closeListeners.add(listener)
        return this
      },
      once(event, listener) {
        return this.on(event, listener)
      },
    }
    signal?.addEventListener('abort', () => {
      if (res.writableEnded) return
      for (const listener of [...closeListeners]) listener()
    }, { once: true })
    Promise.resolve()
      .then(() => route.handler(request, res))
      .catch(reject)
  })
  if (response.status !== 200) throw new Error(`transport failure for ${route.path}/${endpoint}: HTTP ${response.status}`)
  const envelope = JSON.parse(response.body)
  if (envelope.type !== 'server-response' || typeof envelope.rpcId !== 'string') throw new TypeError('connection: invalid server-response envelope')
  if (envelope.rpcId !== rpcId) throw new Error(`rpcId mismatch for ${endpoint}`)
  const result = envelope.result
  if (result === null || typeof result !== 'object') throw new TypeError('connection: invalid server-response result')
  if (result.ok === true) return { ok: true, value: result.value }
  return { ok: false, error: result.error }
}

export { DEFAULT_MAX_REQUEST_BYTES }
