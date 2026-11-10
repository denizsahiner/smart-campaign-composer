import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount as vtuMount, flushPromises, RouterLinkStub } from '@vue/test-utils'
import Monitoring from '../src/views/Monitoring.vue'
import { jsonResponse, mockFetch } from './helpers'

function metrics(overrides = {}) {
  const zero = { pending: 0, sending: 0, retry_scheduled: 0, sent: 0, failed: 0, skipped: 0 }
  return {
    queue_depth: { sms: 120, email: 0 },
    messages: { sms: { ...zero, pending: 100, retry_scheduled: 20, sent: 984 }, email: { ...zero, sent: 1110, failed: 3 } },
    llm: { calls: 10, validation_failures: 2, errors: 1, avg_latency_ms: 1480.3, input_tokens: 6716, output_tokens: 1148 },
    worker: { status: 'ok' },
    provider: {
      status: 'ok',
      totals: {
        sms: { requests: 698, deliveries: 656, duplicates: 2, hung_after_delivery: 2, rejected: { server_error: 34, timeout: 6 } },
        email: { requests: 790, deliveries: 740, duplicates: 2, hung_after_delivery: 2, rejected: { server_error: 44, throttled: 2 } },
      },
    },
    ...overrides,
  }
}

const SUMMARY = 'GET /api/logs/summary?since_hours=24'
const mount = (c) => vtuMount(c, { global: { stubs: { RouterLink: RouterLinkStub } } })
// mockFetch with the log summary answered, unless routes say otherwise.
const routesWith = (routes, summary = { errors: 0, warnings: 0 }) => mockFetch({ [SUMMARY]: () => jsonResponse(200, summary), ...routes })
const metricsCalls = (calls) => calls.filter((c) => c.key === 'GET /api/metrics')

const row = (w, label) => w.findAll('tr').find((r) => r.find('th').text() === label)
const cells = (r) => r.findAll('td').map((td) => td.text())
const service = (w, name) => w.findAll('.services li').find((li) => li.text().startsWith(name))

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

describe('Monitoring', () => {
  it('shows services, queue, message counts and Claude stats', async () => {
    routesWith({ 'GET /api/metrics': () => jsonResponse(200, metrics()) })
    const w = mount(Monitoring)
    await flushPromises()

    expect(service(w, 'API ve veritabanı').text()).toContain('Çalışıyor')
    expect(service(w, 'Worker').text()).toContain('Çalışıyor')
    expect(service(w, 'Sahte sağlayıcı').text()).toContain('Çalışıyor')
    expect(w.text()).toContain('120SMS bekliyor')
    expect(cells(row(w, 'Gönderildi'))).toEqual(['984', '1.110'])
    expect(cells(row(w, 'Başarısız'))).toEqual(['0', '3'])
    expect(w.text()).toContain('1.480ort. ms')
    expect(w.text()).toContain('6.716 girdi · 1.148 çıktı')
  })

  it("lists every rejection reason either channel saw, with Turkish labels", async () => {
    routesWith({ 'GET /api/metrics': () => jsonResponse(200, metrics()) })
    const w = mount(Monitoring)
    await flushPromises()
    expect(cells(row(w, 'Tekrar (aynı mesaj)'))).toEqual(['2', '2'])
    expect(cells(row(w, 'Reddedilen: sunucu hatası'))).toEqual(['34', '44'])
    expect(cells(row(w, 'Reddedilen: kısıtlama penceresi (429)'))).toEqual(['0', '2'])
    expect(cells(row(w, 'Reddedilen: zaman aşımı'))).toEqual(['6', '0'])
  })

  it('reports a dependency that is down and hides the provider table without totals', async () => {
    routesWith({
      'GET /api/metrics': () => jsonResponse(200, metrics({
        worker: { status: 'unavailable', error: 'no heartbeat for 30s' },
        provider: { status: 'unreachable', error: 'dial tcp: connection refused' },
      })),
    })
    const w = mount(Monitoring)
    await flushPromises()
    expect(service(w, 'Worker').text()).toContain('Sağlıksız')
    expect(service(w, 'Worker').text()).toContain('no heartbeat for 30s')
    expect(service(w, 'Sahte sağlayıcı').text()).toContain('Erişilemiyor')
    expect(w.text()).not.toContain('Tekrar (aynı mesaj)')
  })

  it('says when a dependency is not configured', async () => {
    const m = metrics()
    delete m.worker
    routesWith({ 'GET /api/metrics': () => jsonResponse(200, m) })
    const w = mount(Monitoring)
    await flushPromises()
    expect(service(w, 'Worker').text()).toContain('Raporlanmıyor')
  })

  it('polls every 3 seconds and stops when it leaves the page', async () => {
    const calls = routesWith({ 'GET /api/metrics': () => jsonResponse(200, metrics()) })
    const w = mount(Monitoring)
    await flushPromises()
    expect(metricsCalls(calls)).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(3000)
    expect(metricsCalls(calls)).toHaveLength(2)
    w.unmount()
    await vi.advanceTimersByTimeAsync(9000)
    expect(metricsCalls(calls)).toHaveLength(2)
  })

  it('does not poll while the tab is hidden', async () => {
    const calls = routesWith({ 'GET /api/metrics': () => jsonResponse(200, metrics()) })
    mount(Monitoring)
    await flushPromises()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    await vi.advanceTimersByTimeAsync(9000)
    expect(metricsCalls(calls)).toHaveLength(1)
  })

  it('keeps the last values when the API goes away', async () => {
    let up = true
    routesWith({ 'GET /api/metrics': () => (up ? jsonResponse(200, metrics()) : Promise.reject(new TypeError('Failed to fetch'))) })
    const w = mount(Monitoring)
    await flushPromises()
    up = false
    await vi.advanceTimersByTimeAsync(3000)
    expect(w.text()).toContain("API'ye ulaşılamıyor. Son alınan değerler gösteriliyor.")
    expect(service(w, 'API ve veritabanı').text()).toContain('Erişilemiyor')
    expect(cells(row(w, 'Gönderildi'))).toEqual(['984', '1.110'])
  })

  it('shows the API error message when metrics fail', async () => {
    routesWith({ 'GET /api/metrics': () => jsonResponse(500, { error: { code: 'internal', message: 'Something went wrong on our side.' } }) })
    const w = mount(Monitoring)
    await flushPromises()
    expect(w.text()).toContain('Something went wrong on our side.')
  })

  it('summarises the last 24 hours of logged problems and links to the logs page', async () => {
    routesWith({ 'GET /api/metrics': () => jsonResponse(200, metrics()) }, { errors: 2, warnings: 41 })
    const w = mount(Monitoring)
    await flushPromises()
    expect(w.text()).toContain('Son 24 saatte 2 hata, 41 uyarı.')
    expect(w.findComponent(RouterLinkStub).props('to')).toBe('/logs')
  })

  it('still shows the metrics when the log summary fails', async () => {
    routesWith({
      'GET /api/metrics': () => jsonResponse(200, metrics()),
      [SUMMARY]: () => jsonResponse(500, { error: { code: 'internal', message: 'x' } }),
    })
    const w = mount(Monitoring)
    await flushPromises()
    expect(w.text()).toContain('Log özeti alınamadı.')
    expect(service(w, 'Worker').text()).toContain('Çalışıyor')
  })
})
