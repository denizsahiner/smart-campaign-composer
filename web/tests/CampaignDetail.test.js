import { describe, it, expect, beforeEach } from 'vitest'
import { mount, flushPromises, RouterLinkStub } from '@vue/test-utils'
import CampaignDetail from '../src/views/CampaignDetail.vue'
import { campaign, jsonResponse, sseResponse, sse, mockFetch, FakeEventSource } from './helpers'

const GOOD = { sms: 'Merhaba {{first_name}}, sepetin seni bekliyor!', email_subject: '%30 indirim', email_body: 'Sepetindekiler hâlâ orada.' }

const SUMMARY = 'GET /api/logs/summary?campaign_id=7&level=error'
const FAILURES = 'GET /api/campaigns/7/failures'
const mountDetail = () => mount(CampaignDetail, { props: { id: '7' }, global: { stubs: { RouterLink: RouterLinkStub } } })

// errors: what the log summary reports for the campaign.
async function open(c, routes = {}, { errors = 0 } = {}) {
  const calls = mockFetch({
    'GET /api/campaigns/7': () => jsonResponse(200, c),
    [SUMMARY]: () => jsonResponse(200, { errors, warnings: 3 }),
    [FAILURES]: () => jsonResponse(200, { failures: [], total: 0, next_before: null }),
    ...routes,
  })
  const w = mountDetail()
  await flushPromises()
  return { w, calls }
}

// The last call other than the log summary checks that follow every action.
const lastAction = (calls) => calls.filter((x) => x.key !== SUMMARY && x.key !== FAILURES).at(-1)

const button = (w, text) => w.findAll('button').find((b) => b.text().replace('…', '') === text)
const approveButton = (w) => button(w, 'Onayla ve gönder')
const blocker = (w) => approveButton(w).element.parentElement.querySelector('.small.muted')?.textContent

beforeEach(() => FakeEventSource.install())

describe('loading', () => {
  it('shows the campaign, its reach and audience description', async () => {
    const { w } = await open(campaign())
    expect(w.text()).toContain('Kampanya 7')
    expect(w.text()).toContain('1.200') // tr-TR thousands separator
    expect(w.text()).toContain('Şehir: İstanbul')
  })

  it('shows a load error', async () => {
    mockFetch({ 'GET /api/campaigns/7': () => jsonResponse(404, { error: { code: 'not_found', message: 'Campaign not found.' } }) })
    const w = mountDetail()
    await flushPromises()
    expect(w.text()).toContain('Campaign not found.')
  })
})

describe('logged problems', () => {
  it('says nothing when no error was logged for the campaign (retry warnings do not count)', async () => {
    const { w } = await open(campaign())
    expect(w.find('.problems').exists()).toBe(false)
  })

  it('says how many errors were logged and links to them on the logs page', async () => {
    const { w } = await open(campaign(), {}, { errors: 2 })
    expect(w.find('.problems').text()).toContain('Bu kampanyada 2 hata kaydedildi.')
    expect(w.findComponent(RouterLinkStub).props('to')).toBe('/logs?campaign_id=7&level=error')
  })

  it('checks again after an action', async () => {
    let errors = 0
    const { w } = await open(campaign(), {
      'POST /api/campaigns/7/audience': () => { errors = 1; return jsonResponse(200, { campaign: campaign(), audience_error: { code: 'llm_unavailable', message: 'Claude is busy.' } }) },
      [SUMMARY]: () => jsonResponse(200, { errors, warnings: 0 }),
    })
    expect(w.find('.problems').exists()).toBe(false)
    await w.find('textarea').setValue('yeni brief')
    await button(w, 'Kitleyi yeniden oluştur').trigger('click')
    await flushPromises()
    expect(w.find('.problems').text()).toContain('1 hata')
  })

  it('still shows the campaign when the summary fails', async () => {
    const { w } = await open(campaign(), { [SUMMARY]: () => jsonResponse(500, { error: { code: 'internal', message: 'x' } }) })
    expect(w.text()).toContain('Kampanya 7')
    expect(w.find('.problems').exists()).toBe(false)
  })
})

describe('character counting', () => {
  it('counts {{first_name}} as the longest name in the audience', async () => {
    // "Merhaba " (8) + 10 for the name + "!" (1) = 19
    const { w } = await open(campaign({ contents: { tr: { ...GOOD, sms: 'Merhaba {{first_name}}!' } } }))
    expect(w.findAll('.counter')[0].text()).toBe('19 / 160')
  })

  it('counts characters, not bytes or UTF-16 units', async () => {
    // "ğüşİ" is 4 characters (8 bytes); "🎉" is 1 character (2 UTF-16 units).
    const { w } = await open(campaign({ contents: { tr: { ...GOOD, email_subject: 'ğüşİ🎉' } } }))
    expect(w.findAll('.counter')[1].text()).toBe('5 / 80')
  })

  it('updates as the marketer types', async () => {
    const { w } = await open(campaign({ contents: { tr: GOOD } }))
    await w.find('textarea[rows="3"]').setValue('x'.repeat(161))
    expect(w.findAll('.counter')[0].text()).toBe('161 / 160')
    expect(w.findAll('.counter')[0].classes()).toContain('over')
  })
})

describe('approval rules', () => {
  const yesterday = new Date(Date.now() - 36 * 3600 * 1000).toISOString()

  it.each([
    ['without a draft', {}, 'Önce mesajları üretin.'],
    ['with unacknowledged criteria', { contents: { tr: GOOD }, audience: { unsupported_criteria: ['VIP müşteriler'] } }, 'Önce eksik kriterleri onaylayın.'],
    ['with a stale audience', { contents: { tr: GOOD }, audience: { as_of: yesterday } }, 'Önce kitleyi yenileyin.'],
    ['with an SMS over 160 once the name is in', { contents: { tr: { ...GOOD, sms: 'x'.repeat(151) + '{{first_name}}' } } }, 'Bir mesaj karakter sınırını aşıyor.'],
    ['with an empty field', { contents: { tr: { ...GOOD, email_body: '   ' } } }, 'Tüm mesajları doldurun.'],
  ])('is blocked %s', async (_, overrides, reason) => {
    const { w } = await open(campaign(overrides))
    expect(approveButton(w).attributes('disabled')).toBeDefined()
    expect(blocker(w)).toBe(reason)
  })

  it('checks the blockers in order: acknowledgement before staleness', async () => {
    const { w } = await open(campaign({ contents: { tr: GOOD }, audience: { unsupported_criteria: ['VIP'], as_of: yesterday } }))
    expect(blocker(w)).toBe('Önce eksik kriterleri onaylayın.')
  })

  it('allows approval when everything passes', async () => {
    const { w } = await open(campaign({ contents: { tr: GOOD } }))
    expect(approveButton(w).attributes('disabled')).toBeUndefined()
    expect(blocker(w)).toBeUndefined()
  })

  it('accepts acknowledged criteria', async () => {
    const { w } = await open(campaign({ contents: { tr: GOOD }, audience: { unsupported_criteria: ['VIP'], unsupported_acknowledged: true } }))
    expect(approveButton(w).attributes('disabled')).toBeUndefined()
    expect(w.text()).toContain('Hedef kitleye dahil edilmeyenler: VIP')
  })

  it('hides approval once the campaign is approved', async () => {
    const { w } = await open(campaign({ contents: { tr: GOOD }, status: 'approved', approved_at: new Date().toISOString() }))
    expect(approveButton(w)).toBeUndefined()
    expect(w.find('textarea[rows="3"]').attributes('readonly')).toBeDefined()
  })
})

describe('audience actions', () => {
  it('acknowledges unsupported criteria', async () => {
    const c = campaign({ audience: { unsupported_criteria: ['VIP'] } })
    const acked = campaign({ audience: { unsupported_criteria: ['VIP'], unsupported_acknowledged: true } })
    const { w, calls } = await open(c, { 'POST /api/campaigns/7/acknowledge': () => jsonResponse(200, acked) })
    await button(w, 'Anladım, devam et').trigger('click')
    await flushPromises()
    expect(calls.map((x) => x.key)).toContain('POST /api/campaigns/7/acknowledge')
    expect(button(w, 'Anladım, devam et')).toBeUndefined()
    expect(button(w, 'Mesaj öner')).toBeDefined()
  })

  it('refreshes a stale audience', async () => {
    const stale = campaign({ contents: { tr: GOOD }, audience: { as_of: '2020-01-01T09:00:00Z' } })
    const { w, calls } = await open(stale, { 'POST /api/campaigns/7/audience/refresh': () => jsonResponse(200, campaign({ contents: { tr: GOOD } })) })
    await button(w, 'Kitleyi yenile').trigger('click')
    await flushPromises()
    expect(lastAction(calls).key).toBe('POST /api/campaigns/7/audience/refresh')
    expect(button(w, 'Kitleyi yenile')).toBeUndefined()
    expect(approveButton(w).attributes('disabled')).toBeUndefined()
  })

  it('reruns the audience with the edited brief and shows an audience error', async () => {
    const { w, calls } = await open(campaign(), {
      'POST /api/campaigns/7/audience': () => jsonResponse(200, {
        campaign: campaign({ audience: null, brief: 'Ay\'da yaşayanlar' }),
        audience_error: { code: 'unmappable', message: 'Nothing in the brief can be expressed as an audience: Ay' },
      }),
    })
    const rerun = button(w, 'Kitleyi yeniden oluştur')
    expect(rerun.attributes('disabled')).toBeDefined() // brief unchanged
    await w.find('textarea[rows="4"]').setValue("  Ay'da yaşayanlar  ")
    await rerun.trigger('click')
    await flushPromises()
    expect(lastAction(calls)).toEqual({ key: 'POST /api/campaigns/7/audience', body: { brief: "Ay'da yaşayanlar" } })
    expect(w.text()).toContain('Nothing in the brief can be expressed as an audience: Ay')
    expect(w.text()).not.toContain('Hedef kitle')
  })
})

describe('message generation', () => {
  it('streams a draft, then shows it as editable fields', async () => {
    const { w } = await open(campaign(), {
      'POST /api/campaigns/7/messages': () => sseResponse([sse('delta', { delta: '{"sms":' }), sse('validated', { contents: { tr: GOOD } })]),
    })
    await button(w, 'Mesaj öner').trigger('click')
    await flushPromises()
    expect(w.find('textarea[rows="3"]').element.value).toBe(GOOD.sms)
    expect(w.find('.stream').exists()).toBe(false)
    expect(button(w, 'Revize et')).toBeDefined()
  })

  it('shows an invalid draft with its errors so the marketer can fix it', async () => {
    const bad = { ...GOOD, sms: 'x'.repeat(170) }
    const { w } = await open(campaign(), {
      'POST /api/campaigns/7/messages': () => sseResponse([
        sse('retry', { reason: 'invalid', errors: [] }),
        sse('invalid', { contents: { tr: bad }, errors: [{ path: 'tr.sms', code: 'too_long', message: 'SMS is 170 characters; the limit is 160.' }] }),
      ]),
    })
    await button(w, 'Mesaj öner').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('Bu taslak henüz doğrulamadan geçmiyor')
    expect(w.text()).toContain('sms: SMS is 170 characters; the limit is 160.')
    expect(w.find('textarea[rows="3"]').element.value).toBe(bad.sms)
    expect(approveButton(w).attributes('disabled')).toBeDefined()
  })

  it('sends feedback when revising and clears it afterwards', async () => {
    const { w, calls } = await open(campaign({ contents: { tr: GOOD } }), {
      'POST /api/campaigns/7/messages': () => sseResponse([sse('validated', { contents: { tr: { ...GOOD, sms: 'Daha resmi' } } })]),
    })
    expect(button(w, 'Revize et').attributes('disabled')).toBeDefined() // no feedback yet
    await w.find('input.feedback').setValue('daha resmi')
    await button(w, 'Revize et').trigger('click')
    await flushPromises()
    expect(lastAction(calls)).toEqual({ key: 'POST /api/campaigns/7/messages', body: { feedback: 'daha resmi', languages: ['tr'] } })
    expect(w.find('textarea[rows="3"]').element.value).toBe('Daha resmi')
    expect(w.find('input.feedback').element.value).toBe('')
  })

  it("shows the stream's error event", async () => {
    const { w } = await open(campaign(), {
      'POST /api/campaigns/7/messages': () => sseResponse([sse('error', { message: 'Claude is busy or unreachable right now.' })]),
    })
    await button(w, 'Mesaj öner').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('Claude is busy or unreachable right now.')
    expect(w.text()).toContain('Henüz mesaj yok.')
  })

  it('shows a precondition error from before the stream', async () => {
    const { w } = await open(campaign(), {
      'POST /api/campaigns/7/messages': () => jsonResponse(409, { error: { code: 'busy', message: 'Claude is already working on this campaign.' } }),
    })
    await button(w, 'Mesaj öner').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('Claude is already working on this campaign.')
  })
})

describe('approval', () => {
  const approved = () => campaign({ contents: { tr: GOOD }, status: 'approved', approved_at: new Date().toISOString() })

  it('sends the edited content, shows what was queued and starts following delivery', async () => {
    const { w, calls } = await open(campaign({ contents: { tr: GOOD } }), {
      'POST /api/campaigns/7/approve': () => jsonResponse(200, {
        campaign: approved(),
        queued: { sms: 900, email: 1000, without_name: { sms: 3, email: 0 } },
      }),
    })
    await w.find('textarea[rows="3"]').setValue('Elle düzenlendi {{first_name}}')
    await approveButton(w).trigger('click')
    await flushPromises()

    expect(lastAction(calls)).toEqual({ key: 'POST /api/campaigns/7/approve', body: { contents: { tr: { ...GOOD, sms: 'Elle düzenlendi {{first_name}}' } } } })
    expect(w.text()).toContain('Kuyruğa alındı: 900 SMS ve 1.000 e-posta.')
    expect(w.text()).toContain('3 SMS ve 0 e-posta isimsiz gider')
    expect(approveButton(w)).toBeUndefined()
    expect(w.text()).toContain('Gönderim')
    expect(FakeEventSource.last().url).toBe('/api/campaigns/7/events')
  })

  it('shows the field errors the backend rejects the content with', async () => {
    const { w } = await open(campaign({ contents: { tr: GOOD } }), {
      'POST /api/campaigns/7/approve': () => jsonResponse(422, {
        error: { code: 'invalid_content', message: "The messages don't pass validation.", errors: [{ path: 'email_body', code: 'unknown_placeholder', message: 'Unknown placeholder {{city}}.' }] },
      }),
    })
    await approveButton(w).trigger('click')
    await flushPromises()
    expect(w.text()).toContain("The messages don't pass validation.")
    expect(w.text()).toContain('email_body: Unknown placeholder {{city}}.')
    expect(approveButton(w)).toBeDefined()
  })

  it('lists the rejected recipients of an approved campaign, and reloads them when delivery is done', async () => {
    let total = 1
    const failure = (id) => ({ message_id: id, user_id: 3, first_name: 'Ayşe', last_name: 'Yılmaz', channel: 'sms', address: '+902125550123', reason: 'Geçersiz telefon numarası', error: '400 invalid_recipient: not a Turkish mobile number', failed_at: new Date().toISOString() })
    const { w, calls } = await open(approved(), {
      [FAILURES]: () => jsonResponse(200, { failures: Array.from({ length: total }, (_, i) => failure(i + 1)), total, next_before: null }),
    })
    expect(w.text()).toContain('Gönderilemeyen alıcılar')
    expect(w.text()).toContain('1 alıcıya gönderilemedi')
    expect(calls.filter((x) => x.key === FAILURES)).toHaveLength(1)

    total = 2
    mockFetch({
      'GET /api/campaigns/7': () => jsonResponse(200, campaign({ contents: { tr: GOOD }, status: 'completed' })),
      [SUMMARY]: () => jsonResponse(200, { errors: 2, warnings: 0 }),
      [FAILURES]: () => jsonResponse(200, { failures: [failure(1), failure(2)], total, next_before: null }),
    })
    FakeEventSource.last().emit('done', { campaign_id: 7, status: 'completed', channels: { sms: {}, email: {} }, total: { sms: 0, email: 0 }, rate_per_second: {} })
    await flushPromises()
    expect(w.text()).toContain('2 alıcıya gönderilemedi')
  })

  it('shows no failed recipients section before approval', async () => {
    const { w, calls } = await open(campaign({ contents: { tr: GOOD } }))
    expect(w.text()).not.toContain('Gönderilemeyen alıcılar')
    expect(calls.map((x) => x.key)).not.toContain(FAILURES)
  })

  it('reloads the campaign when delivery is done', async () => {
    const { w } = await open(approved())
    expect(w.find('.badge').text()).toBe('Onaylandı')
    mockFetch({ 'GET /api/campaigns/7': () => jsonResponse(200, campaign({ contents: { tr: GOOD }, status: 'completed' })) })
    FakeEventSource.last().emit('done', { campaign_id: 7, status: 'completed', channels: { sms: {}, email: {} }, total: { sms: 0, email: 0 }, rate_per_second: {} })
    await flushPromises()
    expect(w.find('.badge').text()).toBe('Tamamlandı')
  })
})

describe('languages', () => {
  const EN = { sms: 'Hi {{first_name}}, your cart is waiting!', email_subject: '30% off', email_body: 'Your items are still there.' }
  const bilingual = (overrides = {}) => campaign({
    audience: {
      content_languages: ['tr', 'en'],
      reach: {
        total: 1200, sms: 900, email: 1000, longest_first_name: 10,
        languages: { tr: { total: 1000, sms: 750, email: 830, longest_first_name: 10 }, en: { total: 200, sms: 150, email: 170, longest_first_name: 4 } },
      },
    },
    ...overrides,
  })
  const tabs = (w) => w.findAll('[role="tab"]')
  const tab = (w, name) => tabs(w).find((t) => t.text().startsWith(name))
  const sms = (w) => w.find('textarea[rows="3"]').element.value

  it('shows a tab per language with its recipients, and each draft in its tab', async () => {
    const { w } = await open(bilingual({ contents: { tr: GOOD, en: EN } }))
    expect(tabs(w).map((t) => t.text())).toEqual(['Türkçe · 1.000 kişi', 'English · 200 kişi'])
    expect(sms(w)).toBe(GOOD.sms)
    await tab(w, 'English').trigger('click')
    expect(sms(w)).toBe(EN.sms)
    expect(w.text()).toContain('en uzun adına (4 karakter)')
  })

  it("counts {{first_name}} with the tab's own longest name", async () => {
    const { w } = await open(bilingual({ contents: { tr: { ...GOOD, sms: '{{first_name}}' }, en: { ...EN, sms: '{{first_name}}' } } }))
    expect(w.findAll('.counter')[0].text()).toBe('10 / 160')
    await tab(w, 'English').trigger('click')
    expect(w.findAll('.counter')[0].text()).toBe('4 / 160')
  })

  it('marks the tab with a problem and blocks approval', async () => {
    const { w } = await open(bilingual({ contents: { tr: GOOD, en: { ...EN, sms: 'x'.repeat(161) } } }))
    expect(tab(w, 'Türkçe').find('.warn-mark').exists()).toBe(false)
    expect(tab(w, 'English').find('.warn-mark').exists()).toBe(true)
    expect(blocker(w)).toBe('Bir mesaj karakter sınırını aşıyor.')
  })

  it('shows field errors on their own tab only', async () => {
    const { w } = await open(bilingual({
      status: 'draft',
      contents: { tr: GOOD, en: EN },
      content_errors: [{ path: 'en.email_body', code: 'unknown_placeholder', message: 'unknown placeholder {{city}}' }],
    }))
    expect(w.text()).not.toContain('unknown placeholder')
    await tab(w, 'English').trigger('click')
    expect(w.text()).toContain('email_body: unknown placeholder {{city}}')
    expect(tab(w, 'English').find('.warn-mark').exists()).toBe(true)
  })

  it('revises only the selected languages and keeps edits in the others', async () => {
    const { w, calls } = await open(bilingual({ contents: { tr: GOOD, en: EN } }), {
      'POST /api/campaigns/7/messages': () => sseResponse([sse('validated', { contents: { en: { ...EN, sms: 'Dear {{first_name}}' } } })]),
    })
    await w.find('textarea[rows="3"]').setValue('Elle düzenlendi')
    const boxes = w.findAll('.pick input')
    expect(boxes.map((b) => b.element.checked)).toEqual([true, true])
    await boxes[0].setValue(false) // not Turkish
    await w.find('input.feedback').setValue('more formal')
    await button(w, 'Revize et').trigger('click')
    await flushPromises()
    expect(lastAction(calls)).toEqual({ key: 'POST /api/campaigns/7/messages', body: { feedback: 'more formal', languages: ['en'] } })
    expect(sms(w)).toBe('Elle düzenlendi')
    await tab(w, 'English').trigger('click')
    expect(sms(w)).toBe('Dear {{first_name}}')
  })

  it('cannot revise with no language selected', async () => {
    const { w } = await open(bilingual({ contents: { tr: GOOD, en: EN } }))
    await w.find('input.feedback').setValue('more formal')
    for (const b of w.findAll('.pick input')) await b.setValue(false)
    expect(button(w, 'Revize et').attributes('disabled')).toBeDefined()
  })

  it('offers to write a language that has no draft yet, and blocks approval until then', async () => {
    const { w, calls } = await open(bilingual({ status: 'draft', contents: { tr: GOOD } }), {
      'POST /api/campaigns/7/messages': () => sseResponse([sse('validated', { contents: { en: EN } })]),
    })
    expect(blocker(w)).toBe('Önce English mesajlarını yazın.')
    expect(tab(w, 'English').find('.warn-mark').exists()).toBe(true)
    await tab(w, 'English').trigger('click')
    expect(w.text()).toContain('Bu dilde henüz mesaj yok.')
    await button(w, 'English yaz').trigger('click')
    await flushPromises()
    expect(lastAction(calls)).toEqual({ key: 'POST /api/campaigns/7/messages', body: { languages: ['en'] } })
    expect(sms(w)).toBe(EN.sms)
    expect(approveButton(w).attributes('disabled')).toBeUndefined()
  })

  it('approves with every language', async () => {
    const { w, calls } = await open(bilingual({ contents: { tr: GOOD, en: EN } }), {
      'POST /api/campaigns/7/approve': () => jsonResponse(200, {
        campaign: bilingual({ contents: { tr: GOOD, en: EN }, status: 'approved', approved_at: new Date().toISOString() }),
        queued: { sms: 900, email: 1000, without_name: { sms: 0, email: 0 }, by_language: { tr: 1000, en: 200 } },
      }),
    })
    await approveButton(w).trigger('click')
    await flushPromises()
    expect(lastAction(calls).body).toEqual({ contents: { tr: GOOD, en: EN } })
    expect(w.text()).toContain('(Türkçe: 1.000 kişi, English: 200 kişi)')
  })

  it('has no language picker for a one-language audience', async () => {
    const { w } = await open(campaign({ contents: { tr: GOOD } }))
    expect(w.find('.pick').exists()).toBe(false)
    expect(tabs(w).map((t) => t.text())).toEqual(['Türkçe · 1.200 kişi'])
  })
})
