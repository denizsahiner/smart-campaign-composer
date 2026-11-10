import { describe, it, expect } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import FailedRecipients from '../src/components/FailedRecipients.vue'
import { jsonResponse, mockFetch } from './helpers'

const failure = (id, over = {}) => ({
  message_id: id, user_id: 10 + id, first_name: 'Ayşe', last_name: 'Yılmaz', channel: 'email',
  address: `ayse${id}@ornek.invalid`, reason: 'Geçersiz e-posta adresi',
  error: '400 invalid_recipient: domain does not exist', failed_at: '2026-11-10T14:24:11Z', ...over,
})

async function open(routes, props = {}) {
  const calls = mockFetch(routes)
  const w = mount(FailedRecipients, { props: { campaignId: 7, ...props } })
  await flushPromises()
  return { w, calls }
}

describe('FailedRecipients', () => {
  it('renders nothing when no recipient was rejected', async () => {
    const { w } = await open({ 'GET /api/campaigns/7/failures': () => jsonResponse(200, { failures: [], total: 0, next_before: null }) })
    expect(w.text()).toBe('')
  })

  it('renders nothing when the list cannot be loaded', async () => {
    const { w } = await open({ 'GET /api/campaigns/7/failures': () => jsonResponse(500, { error: { code: 'internal', message: 'x' } }) })
    expect(w.text()).toBe('')
  })

  it('lists name, channel, full address and reason, with the provider answer on hover and a CSV link', async () => {
    const { w } = await open({
      'GET /api/campaigns/7/failures': () => jsonResponse(200, {
        failures: [failure(2), failure(1, { channel: 'sms', address: '+902125550123', reason: 'Geçersiz telefon numarası', first_name: '', last_name: '' })],
        total: 2, next_before: null,
      }),
    })
    expect(w.text()).toContain('2 alıcıya gönderilemedi')
    const rows = w.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('Ayşe Yılmaz')
    expect(rows[0].text()).toContain('E-posta')
    expect(rows[0].text()).toContain('ayse2@ornek.invalid')
    expect(rows[0].find('[title]').attributes('title')).toBe('400 invalid_recipient: domain does not exist')
    expect(rows[1].text()).toContain('—') // no name on record
    expect(rows[1].text()).toContain('+902125550123')
    expect(rows[1].text()).toContain('Geçersiz telefon numarası')
    const csv = w.find('a[download]')
    expect(csv.attributes('href')).toBe('/api/campaigns/7/failures.csv')
    expect(w.text()).not.toContain('Daha fazla')
  })

  it('pages with Daha fazla', async () => {
    const { w, calls } = await open({
      'GET /api/campaigns/7/failures': () => jsonResponse(200, { failures: [failure(3), failure(2)], total: 3, next_before: 2 }),
      'GET /api/campaigns/7/failures?before=2': () => jsonResponse(200, { failures: [failure(1)], total: 3, next_before: null }),
    })
    expect(w.text()).toContain('2 / 3 gösteriliyor')
    await w.findAll('button').find((b) => b.text() === 'Daha fazla').trigger('click')
    await flushPromises()
    expect(calls.at(-1).key).toBe('GET /api/campaigns/7/failures?before=2')
    expect(w.findAll('tbody tr')).toHaveLength(3)
    expect(w.text()).not.toContain('Daha fazla')
  })

  it('reloads when the version changes', async () => {
    const { w, calls } = await open({ 'GET /api/campaigns/7/failures': () => jsonResponse(200, { failures: [failure(1)], total: 1, next_before: null }) })
    await w.setProps({ version: 1 })
    await flushPromises()
    expect(calls.filter((x) => x.key === 'GET /api/campaigns/7/failures')).toHaveLength(2)
  })
})
