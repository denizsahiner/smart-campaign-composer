// Thin client for the Go API. Errors from the API arrive as {error:{code,message,errors}}.
export class ApiError extends Error {
  constructor(status, body) {
    super(body?.error?.message || `Request failed (${status})`)
    this.status = status
    this.code = body?.error?.code
    this.fields = body?.error?.errors || []
  }
}

async function call(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  })
  const data = await res.json().catch(() => null)
  if (!res.ok) throw new ApiError(res.status, data)
  return data
}

export const api = {
  list: () => call('GET', '/api/campaigns').then((d) => d.campaigns),
  get: (id) => call('GET', `/api/campaigns/${id}`),
  create: (brief) => call('POST', '/api/campaigns', { brief }),
  rerunAudience: (id, brief) => call('POST', `/api/campaigns/${id}/audience`, { brief }),
  acknowledge: (id) => call('POST', `/api/campaigns/${id}/acknowledge`),
  refreshAudience: (id) => call('POST', `/api/campaigns/${id}/audience/refresh`),
  approve: (id, content) => call('POST', `/api/campaigns/${id}/approve`, { content }),
  metrics: () => call('GET', '/api/metrics'),
}

// Health of the API and its database. Never throws: an unreachable API is a status too.
// Returns {status: 'ok'|'unavailable'|'unreachable', db?}.
export async function health() {
  try {
    const res = await fetch('/healthz')
    const data = await res.json().catch(() => null)
    if (res.ok) return { status: 'ok', db: data?.db }
    return { status: 'unavailable', db: data?.db }
  } catch {
    return { status: 'unreachable' }
  }
}

// Follows an approved campaign's delivery (GET /api/campaigns/{id}/events, SSE).
// The server sends progress at once and on every change, then done and closes; a
// server-side failure arrives as a named "error" event with {message}.
// onError(message, retrying): retrying is true while the browser reconnects by itself.
// Returns a function that stops watching.
export function watchDelivery(id, { onProgress, onDone, onError }) {
  const es = new EventSource(`/api/campaigns/${id}/events`)
  es.addEventListener('progress', (e) => onProgress(JSON.parse(e.data)))
  es.addEventListener('done', (e) => {
    es.close()
    onDone(JSON.parse(e.data))
  })
  es.addEventListener('error', (e) => {
    if (e.data) {
      // The server's own error event; it closes the stream after sending it.
      es.close()
      onError(JSON.parse(e.data).message, false)
    } else {
      // Connection-level error. CONNECTING means the browser is retrying on its own;
      // CLOSED means it gave up (e.g. the API answered with a non-stream error).
      onError(null, es.readyState === EventSource.CONNECTING)
    }
  })
  return () => es.close()
}

// Streams the messages step (Server-Sent Events over POST). onEvent gets {type, ...}.
// Events: delta, retry, then one of validated | invalid | error.
export async function generateMessages(id, feedback, onEvent, signal) {
  const res = await fetch(`/api/campaigns/${id}/messages`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ feedback: feedback || '' }),
    signal,
  })
  if (!res.ok) throw new ApiError(res.status, await res.json().catch(() => null))
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    let i
    while ((i = buf.indexOf('\n\n')) >= 0) {
      const block = buf.slice(0, i)
      buf = buf.slice(i + 2)
      const data = block.split('\n').find((l) => l.startsWith('data: '))
      if (data) onEvent(JSON.parse(data.slice(6)))
    }
  }
}
