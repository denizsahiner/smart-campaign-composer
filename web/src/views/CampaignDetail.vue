<template>
  <p v-if="loadError" class="notice error">{{ loadError }}</p>
  <p v-else-if="!c" class="muted">Yükleniyor…</p>
  <div v-else>
    <div class="row spread">
      <h1 class="title">Kampanya {{ c.id }}</h1>
      <StatusBadge :status="c.status" />
    </div>
    <p class="small muted lead">Oluşturuldu {{ new Date(c.created_at).toLocaleString('tr-TR') }}</p>
    <p v-if="logErrors" class="notice error row spread problems">
      <span>Bu kampanyada {{ logErrors.toLocaleString('tr-TR') }} hata kaydedildi.</span>
      <RouterLink :to="`/logs?campaign_id=${c.id}&level=error`"><BaseButton variant="secondary">Logları gör</BaseButton></RouterLink>
    </p>

    <section class="section">
      <h2 class="heading">Brief</h2>
      <textarea v-if="editable" v-model="brief" class="field" rows="4" maxlength="2000" />
      <p v-else class="quote">{{ c.brief }}</p>
      <div v-if="editable" class="row">
        <BaseButton variant="secondary" :disabled="!brief.trim() || brief.trim() === c.brief" :loading="busy === 'audience'" @click="rerun">Kitleyi yeniden oluştur</BaseButton>
      </div>
      <p v-if="audienceError" class="notice error">{{ audienceError.message }}</p>
    </section>

    <section v-if="c.audience" class="section">
      <h2 class="heading">Hedef kitle</h2>
      <div class="metrics">
        <div><div class="metric">{{ c.audience.reach.total.toLocaleString('tr-TR') }}</div><div class="small muted">kişi</div></div>
        <div><div class="metric">{{ c.audience.reach.sms.toLocaleString('tr-TR') }}</div><div class="small muted">SMS ile</div></div>
        <div><div class="metric">{{ c.audience.reach.email.toLocaleString('tr-TR') }}</div><div class="small muted">e-posta ile</div></div>
      </div>
      <ul class="rules">
        <li v-for="d in c.audience.description" :key="d">{{ d }}</li>
      </ul>
      <p class="small muted">Sayım: {{ new Date(c.audience.as_of).toLocaleString('tr-TR') }}. Bir kanal için izni olmayan kişilere o kanaldan mesaj gönderilmez.</p>

      <div v-if="needsAck" class="notice warn stack">
        <p><strong>Hedef kitleye dahil edilmeyenler:</strong> brief, sistemin filtreleyemediği şeyler istiyor.</p>
        <ul class="rules"><li v-for="u in c.audience.unsupported_criteria" :key="u">{{ u }}</li></ul>
        <BaseButton variant="secondary" :loading="busy === 'ack'" @click="acknowledge">Anladım, devam et</BaseButton>
      </div>
      <div v-else-if="c.audience.unsupported_criteria.length" class="notice">
        Onaylandı. Hedef kitleye dahil edilmeyenler: {{ c.audience.unsupported_criteria.join('; ') }}
      </div>

      <div v-if="stale && editable" class="notice warn row spread">
        <span>Kitle önceki bir günde sayılmış. Onaylamadan önce yenileyin.</span>
        <BaseButton variant="secondary" :loading="busy === 'refresh'" @click="refresh">Kitleyi yenile</BaseButton>
      </div>
    </section>

    <section v-if="c.audience" class="section">
      <div class="row spread">
        <h2 class="heading">Mesajlar</h2>
        <div v-if="editable && !needsAck && langs.length" class="row">
          <template v-if="hasDrafts">
            <fieldset v-if="langs.length > 1" class="pick row" aria-label="Revize edilecek diller">
              <label v-for="l in langs" :key="l" class="small">
                <input v-model="selected" type="checkbox" :value="l" :disabled="!drafts[l]" /> {{ LANG_NAMES[l] }}
              </label>
            </fieldset>
            <input v-model="feedback" class="field feedback" placeholder="Daha resmi, daha kısa…" maxlength="500" />
            <BaseButton variant="secondary" :disabled="generating || !feedback.trim() || !revisable.length" :loading="generating" @click="revise">Revize et</BaseButton>
          </template>
          <BaseButton v-else variant="secondary" :disabled="generating" :loading="generating" @click="generate()">Mesaj öner</BaseButton>
        </div>
      </div>
      <p v-if="langs.length > 1" class="small muted">Her alıcıya kendi dilindeki mesaj gider. Brief'i istediğiniz dilde yazabilirsiniz.</p>

      <p v-if="streamNote" class="small muted">{{ streamNote }}</p>
      <pre v-if="generating && streamText" class="stream code">{{ streamText }}</pre>
      <p v-if="messageError" class="notice error">{{ messageError }}</p>

      <template v-if="hasDrafts && !generating">
        <div v-if="langs.length" class="tabs row" role="tablist">
          <button v-for="l in langs" :key="l" type="button" role="tab" class="tab" :class="{ active: l === tab }"
            :aria-selected="l === tab" @click="tab = l">
            {{ LANG_NAMES[l] }} · {{ fmt(c.audience.reach.languages?.[l]?.total ?? c.audience.reach.total) }} kişi
            <span v-if="problem(l)" class="warn-mark" title="Bu dilde düzeltilmesi gereken bir şey var">⚠</span>
          </button>
        </div>
        <div v-if="!drafts[tab]" class="notice warn row spread">
          <span>Bu dilde henüz mesaj yok.</span>
          <BaseButton v-if="editable" variant="secondary" @click="generate({ languages: missing })">{{ missingLabel }} yaz</BaseButton>
        </div>
        <template v-else>
          <p v-if="tabErrors.length" class="notice warn">
            Bu taslak henüz doğrulamadan geçmiyor. Aşağıda düzeltin:
            <span v-for="e in tabErrors" :key="e.path + e.code" class="block">{{ e.path.slice(tab.length + 1) || e.path }}: {{ e.message }}</span>
          </p>
          <div class="msg">
            <div class="row spread"><span class="label sms-c">SMS</span><ChannelCounter :used="len(drafts[tab].sms, tab)" :limit="160" /></div>
            <textarea v-model="drafts[tab].sms" class="field" rows="3" :readonly="!editable" />
          </div>
          <div class="msg">
            <div class="row spread"><span class="label email-c">E-posta konusu</span><ChannelCounter :used="len(drafts[tab].email_subject, tab)" :limit="80" /></div>
            <input v-model="drafts[tab].email_subject" class="field" :readonly="!editable" />
          </div>
          <div class="msg">
            <span class="label email-c">E-posta gövdesi</span>
            <textarea v-model="drafts[tab].email_body" class="field" rows="8" :readonly="!editable" />
          </div>
          <p class="small muted">Kişiselleştirmek için <code class="code">{{ PLACEHOLDER }}</code> kullanın. Sınırlar, bu dildeki alıcıların en uzun adına ({{ longest(tab) }} karakter) göre kontrol edilir.</p>
        </template>
      </template>
      <p v-else-if="!generating && !streamText" class="muted">Henüz mesaj yok.</p>
    </section>

    <section v-if="c.audience && editable" class="section">
      <h2 class="heading">Onay</h2>
      <fieldset class="when">
        <legend class="sr-only">Gönderim zamanı</legend>
        <label><input v-model="when" type="radio" value="now" /> Hemen gönder</label>
        <label><input v-model="when" type="radio" value="later" /> İleri tarihte gönder</label>
        <div v-if="when === 'later'" class="later">
          <input v-model="sendAt" type="datetime-local" class="field date" aria-label="Gönderim zamanı (İstanbul saati)"
            :min="minLocal" :max="maxLocal" />
          <span class="small muted">İstanbul saati · 09:00–21:00 arası, en geç {{ maxLabel }}</span>
        </div>
      </fieldset>
      <p v-if="when === 'later'" class="muted">
        Alıcılar ve metinler şimdi sabitlenir; mesajlar seçilen zamanda gönderilmeye başlar. O zamana kadar zamanlamayı iptal edebilirsiniz.
      </p>
      <p v-else class="muted">Onaylarsanız mesajlar, sahte sağlayıcıya gönderilmek üzere kuyruğa alınır. Bu işlem geri alınamaz.</p>
      <p v-if="approveError" class="notice error">
        {{ approveError.message }}
        <span v-for="e in approveError.fields" :key="e.path + e.code" class="block">{{ e.path }}: {{ e.message }}</span>
      </p>
      <div class="row">
        <BaseButton :disabled="!canApprove" :loading="busy === 'approve'" @click="approve">
          {{ when === 'later' ? 'Zamanla' : 'Onayla ve gönder' }}
        </BaseButton>
        <span v-if="!canApprove" class="small muted">{{ approveBlocker }}</span>
      </div>
    </section>

    <section v-if="scheduled" class="section notice scheduled">
      <p><strong>⏱ {{ fmtTime(c.scheduled_at) }} tarihinde gönderilecek.</strong></p>
      <p v-if="c.audience" class="small">
        {{ fmt(c.audience.reach.total) }} kişi · SMS {{ fmt(c.audience.reach.sms) }} · E-posta {{ fmt(c.audience.reach.email) }}
      </p>
      <p v-if="cancelError" class="notice error">{{ cancelError }}</p>
      <div class="row">
        <BaseButton variant="secondary" :loading="busy === 'cancel'" @click="cancelSchedule">Zamanlamayı iptal et</BaseButton>
      </div>
    </section>

    <section v-if="queued" class="section notice">
      <strong>Kuyruğa alındı:</strong> {{ queued.sms.toLocaleString('tr-TR') }} SMS ve {{ queued.email.toLocaleString('tr-TR') }} e-posta.
      <span v-if="queued.by_language && Object.keys(queued.by_language).length > 1">
        ({{ Object.entries(queued.by_language).map(([l, n]) => `${LANG_NAMES[l] || l}: ${fmt(n)} kişi`).join(', ') }})
      </span>
      <span v-if="queued.without_name.sms || queued.without_name.email">
        {{ queued.without_name.sms }} SMS ve {{ queued.without_name.email }} e-posta isimsiz gider (kayıtlı ad yok).
      </span>
    </section>
    <p v-else-if="sending" class="section notice">
      Onaylandı {{ c.approved_at ? new Date(c.approved_at).toLocaleString('tr-TR') : '' }}.
      <template v-if="c.scheduled_at">Gönderim {{ fmtTime(c.scheduled_at) }} için zamanlanmıştı.</template>
    </p>

    <DeliveryProgress v-if="sending" :key="c.id" :campaign-id="c.id" @done="reload" />
    <FailedRecipients v-if="sending" :campaign-id="c.id" :version="loads" />
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { api, generateMessages } from '../api'
import BaseButton from '../components/BaseButton.vue'
import StatusBadge from '../components/StatusBadge.vue'
import ChannelCounter from '../components/ChannelCounter.vue'
import DeliveryProgress from '../components/DeliveryProgress.vue'
import FailedRecipients from '../components/FailedRecipients.vue'

const props = defineProps({ id: String })
const PLACEHOLDER = '{{first_name}}'
const LANG_NAMES = { tr: 'Türkçe', en: 'English' }
const fmt = (n) => n.toLocaleString('tr-TR')

const c = ref(null)
const loadError = ref('')
const brief = ref('')
const busy = ref('')
const audienceError = ref(null)
const drafts = ref({}) // draft per language: {tr: {sms, email_subject, email_body}, en: …}
const tab = ref('')
const selected = ref([]) // languages to revise
const feedback = ref('')
const generating = ref(false)
const streamText = ref('')
const streamNote = ref('')
const messageError = ref('')
const fieldErrors = ref([])
const approveError = ref(null)
const queued = ref(null)
const when = ref('now') // now | later
const sendAt = ref('') // datetime-local value, Istanbul time
const cancelError = ref('')
const logErrors = ref(0)
const loads = ref(0) // bumped on every (re)load, so the failed recipients list follows
let abort = null
let startCheck = null // timer that reloads a scheduled campaign once its time has come

const editable = computed(() => c.value && (c.value.status === 'draft' || c.value.status === 'ready'))
const sending = computed(() => c.value && (c.value.status === 'approved' || c.value.status === 'completed'))
const scheduled = computed(() => c.value?.status === 'scheduled')
const needsAck = computed(() => {
  const a = c.value?.audience
  return !!a && a.unsupported_criteria.length > 0 && !a.unsupported_acknowledged
})
// Languages the messages are written in: those with a reachable recipient.
const langs = computed(() => c.value?.audience?.content_languages || [])
const hasDrafts = computed(() => langs.value.some((l) => drafts.value[l]))
const missing = computed(() => langs.value.filter((l) => !drafts.value[l]))
const missingLabel = computed(() => missing.value.map((l) => LANG_NAMES[l] || l).join(' ve '))
const revisable = computed(() => selected.value.filter((l) => drafts.value[l] && langs.value.includes(l)))
const tabErrors = computed(() => fieldErrors.value.filter((e) => e.path === tab.value || e.path.startsWith(`${tab.value}.`)))

const stale = computed(() => {
  const a = c.value?.audience
  return !!a && new Date(a.as_of).toDateString() !== new Date().toDateString()
})

// The longest first name among a language's recipients: what the backend measures with.
function longest(lang) {
  const r = c.value?.audience?.reach
  return r?.languages?.[lang]?.longest_first_name ?? r?.longest_first_name ?? 0
}

// Length in characters, with the placeholder replaced by the longest name (as the backend checks).
function len(text, lang) {
  return [...(text || '').replaceAll('{{first_name}}', 'x'.repeat(longest(lang)))].length
}

const over = (l) => len(drafts.value[l].sms, l) > 160 || len(drafts.value[l].email_subject, l) > 80
const empty = (l) => !drafts.value[l].sms.trim() || !drafts.value[l].email_subject.trim() || !drafts.value[l].email_body.trim()
// A language whose tab gets a warning mark: no draft, a field over its limit or empty,
// or validation errors.
const problem = (l) => !drafts.value[l] || over(l) || empty(l) || fieldErrors.value.some((e) => e.path === l || e.path.startsWith(`${l}.`))

// Scheduling rules, as the backend checks them: at least a minute ahead, at most 14 days,
// 09:00–21:00 Istanbul time. Turkey is UTC+3 all year, so Istanbul wall time is UTC + 3h.
const TR_OFFSET = 3 * 60 * 60 * 1000
const MAX_AHEAD = 14 * 24 * 60 * 60 * 1000
const toLocal = (d) => new Date(d.getTime() + TR_OFFSET).toISOString().slice(0, 16)
const fromLocal = (v) => new Date(`${v}:00+03:00`)
const nowTick = ref(Date.now())
const minLocal = computed(() => toLocal(new Date(nowTick.value + 60 * 1000)))
const maxLocal = computed(() => toLocal(new Date(nowTick.value + MAX_AHEAD)))
const maxLabel = computed(() => new Date(nowTick.value + MAX_AHEAD).toLocaleDateString('tr-TR', { timeZone: 'Europe/Istanbul' }))
const fmtTime = (iso) => new Date(iso).toLocaleString('tr-TR', { timeZone: 'Europe/Istanbul', dateStyle: 'long', timeStyle: 'short' })

const scheduleBlocker = computed(() => {
  if (when.value !== 'later') return ''
  if (!sendAt.value) return 'Gönderim zamanını seçin.'
  const t = fromLocal(sendAt.value).getTime()
  if (Number.isNaN(t)) return 'Gönderim zamanını seçin.'
  if (t < nowTick.value + 60 * 1000) return 'Gönderim zamanı en az bir dakika sonrası olmalı.'
  if (t > nowTick.value + MAX_AHEAD) return 'En fazla 14 gün sonrasına zamanlanabilir.'
  const hour = Number(sendAt.value.slice(11, 13))
  if (hour >= 21 || hour < 9) return '21:00–09:00 arasına zamanlanamaz.'
  return ''
})

const approveBlocker = computed(() => {
  if (!hasDrafts.value) return 'Önce mesajları üretin.'
  if (needsAck.value) return 'Önce eksik kriterleri onaylayın.'
  if (stale.value) return 'Önce kitleyi yenileyin.'
  if (missing.value.length) return `Önce ${missingLabel.value} mesajlarını yazın.`
  if (langs.value.some(over)) return 'Bir mesaj karakter sınırını aşıyor.'
  if (langs.value.some(empty)) return 'Tüm mesajları doldurun.'
  return scheduleBlocker.value
})
const canApprove = computed(() => !approveBlocker.value && !busy.value && !generating.value)

function apply(campaign) {
  c.value = campaign
  brief.value = campaign.brief
  if (!generating.value) {
    drafts.value = Object.fromEntries(Object.entries(campaign.contents || {}).map(([l, d]) => [l, { ...d }]))
    fieldErrors.value = campaign.content_errors || []
  }
  const ls = campaign.audience?.content_languages || []
  if (!ls.includes(tab.value)) tab.value = ls[0] || ''
  selected.value = selected.value.filter((l) => ls.includes(l))
  if (!selected.value.length) selected.value = [...ls]
  loads.value++
  checkLogs()
  watchStart()
}

// A scheduled campaign starts when the worker takes its first message, at the scheduled
// time: reload then, and every few seconds after until it has started, so the progress
// panel appears by itself.
function watchStart() {
  clearTimeout(startCheck)
  if (c.value?.status !== 'scheduled') return
  const wait = Math.max(new Date(c.value.scheduled_at).getTime() - Date.now(), 0) + 3000
  startCheck = setTimeout(reload, Math.min(wait, 2 ** 31 - 1))
}

// Errors logged for this campaign (a failed Claude call, a message given up on, a 500).
// Warnings such as retries are normal and don't count. Re-checked after every action.
async function checkLogs() {
  try {
    logErrors.value = (await api.logSummary({ campaign_id: props.id, level: 'error' })).errors
  } catch { /* the notice is a convenience; the logs page has the full picture */ }
}

onMounted(async () => {
  try { apply(await api.get(props.id)) } catch (e) { loadError.value = e.message }
})
const ticker = setInterval(() => { nowTick.value = Date.now() }, 30 * 1000)
onBeforeUnmount(() => {
  abort?.abort()
  clearTimeout(startCheck)
  clearInterval(ticker)
})

// After delivery finishes the campaign is completed; reload so the status badge follows.
async function reload() {
  try { apply(await api.get(props.id)) } catch { /* the delivery panel already shows the final counts */ }
}

async function run(name, fn) {
  busy.value = name
  try { return await fn() } finally { busy.value = '' }
}

async function rerun() {
  audienceError.value = null
  try {
    const res = await run('audience', () => api.rerunAudience(c.value.id, brief.value.trim()))
    audienceError.value = res.audience_error || null
    apply(res.campaign)
  } catch (e) { audienceError.value = { message: e.message } }
}
async function acknowledge() {
  try { apply(await run('ack', () => api.acknowledge(c.value.id))) } catch (e) { messageError.value = e.message }
}
async function refresh() {
  try { apply(await run('refresh', () => api.refreshAudience(c.value.id))) } catch (e) { messageError.value = e.message }
}

// Merges what the stream wrote into the drafts: only its languages change, so edits in
// the other tabs stay. errors replace those languages' errors.
function merge(contents, errors = []) {
  const written = Object.keys(contents || {})
  for (const l of written) drafts.value[l] = { ...contents[l] }
  const own = (e) => written.some((l) => e.path === l || e.path.startsWith(`${l}.`))
  fieldErrors.value = [...fieldErrors.value.filter((e) => !own(e)), ...errors]
  if (written.length && !written.includes(tab.value) && !drafts.value[tab.value]) tab.value = written[0]
}

// opts: {languages, feedback}; without languages, every language the audience needs.
async function generate(opts = {}) {
  messageError.value = ''
  streamText.value = ''
  streamNote.value = 'Claude yazıyor…'
  generating.value = true
  abort = new AbortController()
  try {
    await generateMessages(c.value.id, opts, (e) => {
      if (e.type === 'delta') streamText.value += e.delta
      else if (e.type === 'retry') { streamText.value = ''; streamNote.value = 'İlk deneme doğrulamadan geçemedi; Claude yeniden deniyor…' }
      else if (e.type === 'validated') merge(e.contents)
      else if (e.type === 'invalid') merge(e.contents, e.errors || [])
      else if (e.type === 'error') messageError.value = e.message || 'Mesaj üretimi başarısız oldu.'
    }, abort.signal)
    if (opts.feedback) feedback.value = ''
  } catch (e) {
    if (e.name !== 'AbortError') messageError.value = e.message
  } finally {
    generating.value = false
    streamNote.value = ''
    streamText.value = ''
    checkLogs()
  }
}

function revise() {
  return generate({ languages: revisable.value, feedback: feedback.value.trim() })
}

async function approve() {
  approveError.value = null
  try {
    const contents = Object.fromEntries(langs.value.map((l) => [l, drafts.value[l]]))
    const at = when.value === 'later' ? fromLocal(sendAt.value).toISOString() : undefined
    const res = await run('approve', () => api.approve(c.value.id, contents, at))
    queued.value = res.queued
    apply(res.campaign)
  } catch (e) { approveError.value = { message: e.message, fields: e.fields || [] } }
}

async function cancelSchedule() {
  cancelError.value = ''
  try {
    apply(await run('cancel', () => api.cancelSchedule(c.value.id)))
    queued.value = null
  } catch (e) {
    cancelError.value = e.message
    reload() // e.g. the send started meanwhile: show it
  }
}
</script>

<style scoped>
.lead { margin-top: var(--space-1); }
.problems { margin-top: var(--space-4); }
.quote { font: italic 400 18px/28px var(--font-serif); }
.metrics { display: flex; gap: var(--space-8); padding: var(--space-4) 0; border-top: 1px solid var(--line); border-bottom: 1px solid var(--line); }
.metric { color: var(--brand); }
.sms-c { color: var(--sms); }
.email-c { color: var(--email); }
.rules { margin: 0; padding-left: var(--space-6); }
.msg { display: grid; gap: var(--space-2); }
.feedback { width: 240px; padding: var(--space-2) var(--space-3); }
.pick { border: 0; margin: 0; padding: 0; gap: var(--space-3); }
.pick label { display: inline-flex; align-items: center; gap: var(--space-1); }
.tabs { gap: 0; border-bottom: 1px solid var(--line); }
.tab { background: none; border: 0; border-bottom: 2px solid transparent; margin-bottom: -1px; padding: var(--space-2) var(--space-4); font: 600 14px/22px var(--font-sans); color: var(--muted); cursor: pointer; }
.tab.active { color: var(--brand); border-bottom-color: var(--brand); }
.warn-mark { color: var(--warning); margin-left: var(--space-1); }
.stream { margin: 0; white-space: pre-wrap; background: var(--surface-300); padding: var(--space-3); border-radius: var(--radius-md); }
.block { display: block; }
.when { border: 0; margin: 0; padding: 0; display: grid; gap: var(--space-2); }
.when label { display: inline-flex; align-items: center; gap: var(--space-2); }
.later { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-3); padding-left: var(--space-6); }
.date { width: auto; }
.scheduled p { margin: 0 0 var(--space-2); }
.sr-only { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
