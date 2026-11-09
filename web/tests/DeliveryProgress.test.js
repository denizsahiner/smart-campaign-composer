import { describe, it, expect, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import DeliveryProgress from '../src/components/DeliveryProgress.vue'
import { FakeEventSource, progress } from './helpers'

beforeEach(() => FakeEventSource.install())

const mountIt = () => mount(DeliveryProgress, { props: { campaignId: 7 } })
const channel = (w, i) => w.findAll('.channel')[i]

describe('DeliveryProgress', () => {
  it('waits for the first progress event', () => {
    const w = mountIt()
    expect(w.text()).toContain('Gönderim durumu yükleniyor…')
    expect(FakeEventSource.last().url).toBe('/api/campaigns/7/events')
  })

  it('shows finished / total, the status counts and the send rate per channel', async () => {
    const w = mountIt()
    FakeEventSource.last().emit('progress', progress(
      { sms: { sent: 600, failed: 20, pending: 300, retry_scheduled: 80 }, email: { sent: 1000 } },
      { rate_per_second: { sms: 98.4, email: 0 } },
    ))
    await w.vm.$nextTick()

    const sms = channel(w, 0)
    expect(sms.text()).toContain('620 / 1.000') // sent + failed are finished
    expect(sms.text()).toContain('98,4 mesaj/sn')
    expect(sms.text()).toContain('Yeniden denenecek80')
    expect(w.text()).toContain('Canlı')

    const bar = sms.find('[role="progressbar"]')
    expect(bar.attributes('aria-valuenow')).toBe('620')
    expect(bar.attributes('aria-valuemax')).toBe('1000')
    expect(bar.find('.seg.sent').attributes('style')).toContain('width: 60%')
    expect(bar.find('.seg.failed').attributes('style')).toContain('width: 2%')
  })

  it('hides "skipped" unless something was skipped', async () => {
    const w = mountIt()
    FakeEventSource.last().emit('progress', progress({ sms: { sent: 1 }, email: { sent: 1, skipped: 2 } }))
    await w.vm.$nextTick()
    expect(channel(w, 0).text()).not.toContain('Atlandı')
    expect(channel(w, 1).text()).toContain('Atlandı2')
  })

  it('says so when a channel has no recipients', async () => {
    const w = mountIt()
    FakeEventSource.last().emit('progress', progress({ sms: {}, email: { pending: 5 } }))
    await w.vm.$nextTick()
    expect(channel(w, 0).text()).toContain('Bu kanalda alıcı yok.')
    expect(channel(w, 0).find('[role="progressbar"]').exists()).toBe(false)
  })

  it('marks the delivery finished and emits done', async () => {
    const w = mountIt()
    const final = progress({ sms: { sent: 10 }, email: { sent: 5 } }, { status: 'completed' })
    FakeEventSource.last().emit('done', final)
    await w.vm.$nextTick()
    expect(w.emitted('done')).toEqual([[final]])
    expect(w.text()).toContain('Tamamlandı')
    expect(w.text()).not.toContain('Canlı')
    expect(w.text()).not.toContain('mesaj/sn')
  })

  it('shows that it is reconnecting, and clears the note when progress resumes', async () => {
    const w = mountIt()
    const es = FakeEventSource.last()
    es.fail(FakeEventSource.CONNECTING)
    await w.vm.$nextTick()
    expect(w.text()).toContain('Bağlantı koptu, yeniden bağlanılıyor…')
    expect(w.find('button').exists()).toBe(false) // the browser retries on its own

    es.emit('progress', progress({ sms: { sent: 1 } }))
    await w.vm.$nextTick()
    expect(w.text()).not.toContain('Bağlantı koptu')
  })

  it("offers a reconnect after the server's error event", async () => {
    const w = mountIt()
    FakeEventSource.last().emit('error', { message: 'Could not read delivery status; reconnect to retry.' })
    await w.vm.$nextTick()
    expect(w.text()).toContain('Could not read delivery status; reconnect to retry.')

    await w.find('button').trigger('click')
    expect(FakeEventSource.instances).toHaveLength(2)
    expect(FakeEventSource.instances[0].readyState).toBe(FakeEventSource.CLOSED)
    expect(w.find('button').exists()).toBe(false)
  })

  it('offers a reconnect when the browser gives up', async () => {
    const w = mountIt()
    FakeEventSource.last().fail(FakeEventSource.CLOSED)
    await w.vm.$nextTick()
    expect(w.text()).toContain('Gönderim durumu alınamadı.')
    expect(w.find('button').text()).toBe('Yeniden bağlan')
  })

  it('closes the stream when it leaves the page', () => {
    const w = mountIt()
    w.unmount()
    expect(FakeEventSource.last().readyState).toBe(FakeEventSource.CLOSED)
  })
})
