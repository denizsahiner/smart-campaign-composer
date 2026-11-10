import { describe, it, expect } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import Logs from '../src/views/Logs.vue'
import { jsonResponse, mockFetch } from './helpers'

const Empty = { template: '<div />' }

function log(id, extra = {}) {
  return {
    id, ts: '2026-11-10T10:00:00Z', level: 'error', service: 'worker', message: 'message failed',
    request_id: null, campaign_id: 12, attrs: { err: 'invalid_recipient', channel: 'sms' }, ...extra,
  }
}

// Opens the logs page at path; routes answer the fetches ("GET /api/logs?..." keys).
async function open(path, routes) {
  const calls = mockFetch(routes)
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/logs', component: Logs }, { path: '/campaigns/:id', component: Empty }],
  })
  router.push(path)
  await router.isReady()
  const w = mount({ template: '<RouterView />' }, { global: { plugins: [router] } })
  await flushPromises()
  return { w, calls, router }
}

const rows = (w) => w.findAll('.log')

describe('Logs', () => {
  it('lists logs newest first with level, service, campaign and the error text', async () => {
    const { w } = await open('/logs', {
      'GET /api/logs': () => jsonResponse(200, { logs: [log(2), log(1, { level: 'warn', service: 'api', message: 'llm refused', campaign_id: null, attrs: {} })], next_before: null }),
    })
    expect(rows(w)).toHaveLength(2)
    const first = rows(w)[0].text()
    expect(first).toContain('Hata')
    expect(first).toContain('worker')
    expect(first).toContain('kampanya 12')
    expect(first).toContain('message failed')
    expect(first).toContain('invalid_recipient')
    expect(rows(w)[1].text()).toContain('Uyarı')
    expect(w.text()).not.toContain('Daha eski kayıtlar')
  })

  it('opens with the filters of the URL (the campaign page links here)', async () => {
    const { w, calls } = await open('/logs?campaign_id=12&level=error', {
      'GET /api/logs?level=error&campaign_id=12': () => jsonResponse(200, { logs: [log(1)], next_before: null }),
    })
    expect(calls.map((c) => c.key)).toEqual(['GET /api/logs?level=error&campaign_id=12'])
    expect(w.find('input[inputmode="numeric"]').element.value).toBe('12')
    expect(w.find('select').element.value).toBe('error')
    expect(w.text()).toContain('Filtreleri temizle')
  })

  it('puts the filters in the URL and loads them', async () => {
    const { w, calls, router } = await open('/logs', {
      'GET /api/logs': () => jsonResponse(200, { logs: [], next_before: null }),
      'GET /api/logs?service=api&q=timeout': () => jsonResponse(200, { logs: [], next_before: null }),
    })
    await w.findAll('select')[1].setValue('api')
    await flushPromises()
    await w.find('input[placeholder="Mesaj ya da ayrıntı"]').setValue('timeout')
    await w.find('form').trigger('submit')
    await flushPromises()
    expect(router.currentRoute.value.query).toEqual({ service: 'api', q: 'timeout' })
    expect(calls.at(-1).key).toBe('GET /api/logs?service=api&q=timeout')
    expect(w.text()).toContain('Bu filtrelerle eşleşen kayıt yok.')
  })

  it('filters to one request from a log row', async () => {
    const { w, router } = await open('/logs', {
      'GET /api/logs': () => jsonResponse(200, { logs: [log(1, { request_id: 'abc123' })], next_before: null }),
      'GET /api/logs?request_id=abc123': () => jsonResponse(200, { logs: [log(1, { request_id: 'abc123' })], next_before: null }),
    })
    await w.findAll('button').find((b) => b.text() === 'Bu isteğin logları').trigger('click')
    await flushPromises()
    expect(router.currentRoute.value.query).toEqual({ request_id: 'abc123' })
    expect(w.find('input.mono').element.value).toBe('abc123')
  })

  it('loads older pages', async () => {
    const { w } = await open('/logs', {
      'GET /api/logs': () => jsonResponse(200, { logs: [log(3), log(2)], next_before: 2 }),
      'GET /api/logs?before=2': () => jsonResponse(200, { logs: [log(1)], next_before: null }),
    })
    await w.findAll('button').find((b) => b.text() === 'Daha eski kayıtlar').trigger('click')
    await flushPromises()
    expect(rows(w)).toHaveLength(3)
    expect(w.text()).not.toContain('Daha eski kayıtlar')
  })

  it('shows the details of a log as JSON', async () => {
    const { w } = await open('/logs', { 'GET /api/logs': () => jsonResponse(200, { logs: [log(1)], next_before: null }) })
    expect(w.find('pre.attrs').text()).toContain('"channel": "sms"')
  })

  it('says when there is nothing logged, and when the API is down', async () => {
    let { w } = await open('/logs', { 'GET /api/logs': () => jsonResponse(200, { logs: [], next_before: null }) })
    expect(w.text()).toContain('Kayıtlı uyarı ya da hata yok.')
    ;({ w } = await open('/logs', { 'GET /api/logs': () => Promise.reject(new TypeError('Failed to fetch')) }))
    expect(w.text()).toContain("API'ye ulaşılamıyor.")
  })
})
