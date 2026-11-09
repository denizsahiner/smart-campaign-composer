// Shared fakes for the frontend tests. Nothing here talks to a real server.
import { vi } from 'vitest'

export function jsonResponse(status, body) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

// A text/event-stream response that arrives in exactly these chunks, so tests can split
// an event across reads or pack several into one.
export function sseResponse(chunks) {
  const enc = new TextEncoder()
  const body = new ReadableStream({
    start(controller) {
      for (const c of chunks) controller.enqueue(enc.encode(c))
      controller.close()
    },
  })
  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

export const sse = (type, data) => `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`

// Routes fetch calls by "METHOD path" to handlers returning a Response; records each call.
// An unrouted call fails the test loudly.
export function mockFetch(routes) {
  const calls = []
  const fn = vi.fn(async (path, init = {}) => {
    const key = `${init.method || 'GET'} ${path}`
    calls.push({ key, body: init.body ? JSON.parse(init.body) : undefined })
    const handler = routes[key]
    if (!handler) throw new Error(`unexpected fetch: ${key}`)
    return handler(init)
  })
  vi.stubGlobal('fetch', fn)
  return calls
}

// A controllable EventSource: tests push named events and connection errors into it.
export class FakeEventSource {
  static CONNECTING = 0
  static OPEN = 1
  static CLOSED = 2
  static instances = []

  constructor(url) {
    this.url = url
    this.readyState = FakeEventSource.OPEN
    this.listeners = {}
    FakeEventSource.instances.push(this)
  }

  addEventListener(type, fn) {
    ;(this.listeners[type] ||= []).push(fn)
  }

  close() {
    this.readyState = FakeEventSource.CLOSED
  }

  // A named event from the server; data is JSON-encoded like the Go API does.
  emit(type, data) {
    for (const fn of this.listeners[type] || []) fn({ data: JSON.stringify(data) })
  }

  // A connection-level error (no data). The browser either retries (CONNECTING) or gives up (CLOSED).
  fail(readyState) {
    this.readyState = readyState
    for (const fn of this.listeners.error || []) fn({})
  }

  static install() {
    FakeEventSource.instances = []
    vi.stubGlobal('EventSource', FakeEventSource)
  }

  static last() {
    return FakeEventSource.instances.at(-1)
  }
}

// A campaign as GET /api/campaigns/{id} returns it, with an audience counted today.
// Pass audience: null for a campaign without one, or a partial audience to override fields.
export function campaign({ audience, ...rest } = {}) {
  const now = new Date().toISOString()
  const defaultAudience = {
    rules: [{ field: 'city', op: 'eq', value: 'İstanbul' }],
    description: ['Şehir: İstanbul'],
    reach: { total: 1200, sms: 900, email: 1000, longest_first_name: 10 },
    as_of: now,
    unsupported_criteria: [],
    unsupported_acknowledged: false,
  }
  return {
    id: 7,
    brief: "İstanbul'da sepeti terk edenlere %30 indirim",
    status: 'ready',
    audience: audience === null ? null : { ...defaultAudience, ...audience },
    content: null,
    created_at: now,
    updated_at: now,
    approved_at: null,
    ...rest,
  }
}

export function progress(channels, extra = {}) {
  const zero = { pending: 0, sending: 0, retry_scheduled: 0, sent: 0, failed: 0, skipped: 0 }
  const ch = { sms: { ...zero, ...channels.sms }, email: { ...zero, ...channels.email } }
  const total = (c) => Object.values(c).reduce((a, b) => a + b, 0)
  return {
    campaign_id: 7,
    status: 'approved',
    channels: ch,
    total: { sms: total(ch.sms), email: total(ch.email) },
    rate_per_second: { sms: 0, email: 0 },
    ...extra,
  }
}
