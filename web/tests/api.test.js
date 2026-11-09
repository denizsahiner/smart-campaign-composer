import { describe, it, expect, beforeEach, vi } from 'vitest'
import { api, ApiError, generateMessages, health, watchDelivery } from '../src/api'
import { jsonResponse, sseResponse, sse, mockFetch, FakeEventSource } from './helpers'

describe('ApiError', () => {
  it('carries the code, message and field errors of the API error body', () => {
    const e = new ApiError(422, {
      error: { code: 'invalid_content', message: "The messages don't pass validation.", errors: [{ path: 'sms', code: 'too_long', message: 'too long' }] },
    })
    expect(e.status).toBe(422)
    expect(e.code).toBe('invalid_content')
    expect(e.message).toBe("The messages don't pass validation.")
    expect(e.fields).toEqual([{ path: 'sms', code: 'too_long', message: 'too long' }])
  })

  it('falls back to a generic message when the body is not an API error', () => {
    const e = new ApiError(502, null)
    expect(e.message).toBe('Request failed (502)')
    expect(e.code).toBeUndefined()
    expect(e.fields).toEqual([])
  })
})

describe('api calls', () => {
  it('sends JSON and returns the decoded body', async () => {
    const calls = mockFetch({ 'POST /api/campaigns': () => jsonResponse(201, { campaign: { id: 3 } }) })
    await expect(api.create('brief')).resolves.toEqual({ campaign: { id: 3 } })
    expect(calls).toEqual([{ key: 'POST /api/campaigns', body: { brief: 'brief' } }])
  })

  it('unwraps the campaign list', async () => {
    mockFetch({ 'GET /api/campaigns': () => jsonResponse(200, { campaigns: [{ id: 1 }] }) })
    await expect(api.list()).resolves.toEqual([{ id: 1 }])
  })

  it('throws ApiError on a non-2xx answer', async () => {
    mockFetch({ 'POST /api/campaigns/7/approve': () => jsonResponse(409, { error: { code: 'stale_audience', message: 'Refresh it.' } }) })
    const err = await api.approve(7, {}).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.status).toBe(409)
    expect(err.code).toBe('stale_audience')
  })

  it('throws ApiError even when the error body is not JSON', async () => {
    mockFetch({ 'GET /api/metrics': () => new Response('bad gateway', { status: 502 }) })
    const err = await api.metrics().catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.message).toBe('Request failed (502)')
  })
})

describe('generateMessages (SSE over POST)', () => {
  const content = { sms: 'Merhaba {{first_name}}', email_subject: 'İndirim', email_body: 'Gövde' }

  async function collect(chunks) {
    const calls = mockFetch({ 'POST /api/campaigns/7/messages': () => sseResponse(chunks) })
    const events = []
    await generateMessages(7, 'daha kısa', (e) => events.push(e))
    return { events, calls }
  }

  it('sends the feedback and delivers events in order', async () => {
    const { events, calls } = await collect([sse('delta', { delta: '{"sms"' }), sse('delta', { delta: ':"x"}' }), sse('validated', { content })])
    expect(calls[0].body).toEqual({ feedback: 'daha kısa' })
    expect(events.map((e) => e.type)).toEqual(['delta', 'delta', 'validated'])
    expect(events[2].content).toEqual(content)
  })

  it('reassembles an event split across reads, even inside a multi-byte character', async () => {
    const bytes = new TextEncoder().encode(sse('delta', { delta: 'Şişli' }))
    const cut = bytes.indexOf(0xc5) + 1 // between the two bytes of "Ş"
    // sseResponse chunks strings; split the raw bytes here instead.
    mockFetch({
      'POST /api/campaigns/7/messages': () => new Response(new ReadableStream({
        start(c) { c.enqueue(bytes.slice(0, cut)); c.enqueue(bytes.slice(cut)); c.close() },
      })),
    })
    const events = []
    await generateMessages(7, '', (e) => events.push(e))
    expect(events).toEqual([{ type: 'delta', delta: 'Şişli' }])
  })

  it('handles several events in one read and an event split at the separator', async () => {
    const a = sse('delta', { delta: 'a' })
    const b = sse('retry', { reason: 'invalid', errors: [{ path: 'sms', code: 'too_long', message: 'x' }] })
    const c = sse('invalid', { content, errors: [] })
    const { events } = await collect([a + b.slice(0, -1), b.slice(-1) + c])
    expect(events.map((e) => e.type)).toEqual(['delta', 'retry', 'invalid'])
    expect(events[1].reason).toBe('invalid')
  })

  it('ignores comment lines (keep-alive pings)', async () => {
    const { events } = await collect([': ping\n\n', sse('error', { message: 'Claude is busy' })])
    expect(events).toEqual([{ type: 'error', message: 'Claude is busy' }])
  })

  it('sends an empty feedback when none is given', async () => {
    const calls = mockFetch({ 'POST /api/campaigns/7/messages': () => sseResponse([]) })
    await generateMessages(7, undefined, () => {})
    expect(calls[0].body).toEqual({ feedback: '' })
  })

  it('throws ApiError when a precondition fails before the stream starts', async () => {
    mockFetch({ 'POST /api/campaigns/7/messages': () => jsonResponse(409, { error: { code: 'busy', message: 'Claude is already working on this campaign.' } }) })
    const onEvent = vi.fn()
    const err = await generateMessages(7, '', onEvent).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.code).toBe('busy')
    expect(onEvent).not.toHaveBeenCalled()
  })
})

describe('health', () => {
  it('reports ok', async () => {
    mockFetch({ 'GET /healthz': () => jsonResponse(200, { status: 'ok', db: 'ok' }) })
    await expect(health()).resolves.toEqual({ status: 'ok', db: 'ok' })
  })

  it('reports an unhealthy API with the reason', async () => {
    mockFetch({ 'GET /healthz': () => jsonResponse(503, { status: 'unavailable', db: 'down' }) })
    await expect(health()).resolves.toEqual({ status: 'unavailable', db: 'down' })
  })

  it('reports an unreachable API instead of throwing', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    await expect(health()).resolves.toEqual({ status: 'unreachable' })
  })
})

describe('watchDelivery (EventSource)', () => {
  let handlers
  beforeEach(() => {
    FakeEventSource.install()
    handlers = { onProgress: vi.fn(), onDone: vi.fn(), onError: vi.fn() }
  })

  it('subscribes to the campaign stream and passes progress through', () => {
    watchDelivery(7, handlers)
    const es = FakeEventSource.last()
    expect(es.url).toBe('/api/campaigns/7/events')
    es.emit('progress', { campaign_id: 7 })
    expect(handlers.onProgress).toHaveBeenCalledWith({ campaign_id: 7 })
  })

  it('closes on done so the browser does not reconnect to a finished stream', () => {
    watchDelivery(7, handlers)
    const es = FakeEventSource.last()
    es.emit('done', { status: 'completed' })
    expect(es.readyState).toBe(FakeEventSource.CLOSED)
    expect(handlers.onDone).toHaveBeenCalledWith({ status: 'completed' })
  })

  it("closes on the server's error event and reports its message", () => {
    watchDelivery(7, handlers)
    const es = FakeEventSource.last()
    es.emit('error', { message: 'Could not read delivery status; reconnect to retry.' })
    expect(es.readyState).toBe(FakeEventSource.CLOSED)
    expect(handlers.onError).toHaveBeenCalledWith('Could not read delivery status; reconnect to retry.', false)
  })

  it('tells a reconnecting connection apart from a closed one', () => {
    watchDelivery(7, handlers)
    const es = FakeEventSource.last()
    es.fail(FakeEventSource.CONNECTING)
    expect(handlers.onError).toHaveBeenLastCalledWith(null, true)
    es.fail(FakeEventSource.CLOSED)
    expect(handlers.onError).toHaveBeenLastCalledWith(null, false)
  })

  it('returns a function that stops watching', () => {
    const stop = watchDelivery(7, handlers)
    stop()
    expect(FakeEventSource.last().readyState).toBe(FakeEventSource.CLOSED)
  })
})
