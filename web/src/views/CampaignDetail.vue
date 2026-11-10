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
        <div v-if="editable && !needsAck" class="row">
          <input v-if="draft" v-model="feedback" class="field feedback" placeholder="Daha resmi, daha kısa…" maxlength="500" />
          <BaseButton variant="secondary" :disabled="generating || (draft && !feedback.trim())" :loading="generating" @click="generate">
            {{ draft ? 'Revize et' : 'Mesaj öner' }}
          </BaseButton>
        </div>
      </div>

      <p v-if="streamNote" class="small muted">{{ streamNote }}</p>
      <pre v-if="generating && streamText" class="stream code">{{ streamText }}</pre>
      <p v-if="messageError" class="notice error">{{ messageError }}</p>

      <template v-if="draft && !generating">
        <p v-if="fieldErrors.length" class="notice warn">
          Bu taslak henüz doğrulamadan geçmiyor. Aşağıda düzeltin:
          <span v-for="e in fieldErrors" :key="e.path + e.code" class="block">{{ e.path }}: {{ e.message }}</span>
        </p>
        <div class="msg">
          <div class="row spread"><span class="label sms-c">SMS</span><ChannelCounter :used="len(draft.sms)" :limit="160" /></div>
          <textarea v-model="draft.sms" class="field" rows="3" :readonly="!editable" />
        </div>
        <div class="msg">
          <div class="row spread"><span class="label email-c">E-posta konusu</span><ChannelCounter :used="len(draft.email_subject)" :limit="80" /></div>
          <input v-model="draft.email_subject" class="field" :readonly="!editable" />
        </div>
        <div class="msg">
          <span class="label email-c">E-posta gövdesi</span>
          <textarea v-model="draft.email_body" class="field" rows="8" :readonly="!editable" />
        </div>
        <p class="small muted">Kişiselleştirmek için <code class="code">{{ PLACEHOLDER }}</code> kullanın. Sınırlar, kitledeki en uzun ada ({{ c.audience.reach.longest_first_name }} karakter) göre kontrol edilir.</p>
      </template>
      <p v-else-if="!generating && !streamText" class="muted">Henüz mesaj yok.</p>
    </section>

    <section v-if="c.audience && editable" class="section">
      <h2 class="heading">Onay</h2>
      <p class="muted">Onaylarsanız mesajlar, sahte sağlayıcıya gönderilmek üzere kuyruğa alınır. Bu işlem geri alınamaz.</p>
      <p v-if="approveError" class="notice error">
        {{ approveError.message }}
        <span v-for="e in approveError.fields" :key="e.path + e.code" class="block">{{ e.path }}: {{ e.message }}</span>
      </p>
      <div class="row">
        <BaseButton :disabled="!canApprove" :loading="busy === 'approve'" @click="approve">Onayla ve gönder</BaseButton>
        <span v-if="!canApprove" class="small muted">{{ approveBlocker }}</span>
      </div>
    </section>

    <section v-if="queued" class="section notice">
      <strong>Kuyruğa alındı:</strong> {{ queued.sms.toLocaleString('tr-TR') }} SMS ve {{ queued.email.toLocaleString('tr-TR') }} e-posta.
      <span v-if="queued.without_name.sms || queued.without_name.email">
        {{ queued.without_name.sms }} SMS ve {{ queued.without_name.email }} e-posta isimsiz gider (kayıtlı ad yok).
      </span>
    </section>
    <p v-else-if="sending" class="section notice">
      Onaylandı {{ c.approved_at ? new Date(c.approved_at).toLocaleString('tr-TR') : '' }}.
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

const c = ref(null)
const loadError = ref('')
const brief = ref('')
const busy = ref('')
const audienceError = ref(null)
const draft = ref(null)
const feedback = ref('')
const generating = ref(false)
const streamText = ref('')
const streamNote = ref('')
const messageError = ref('')
const fieldErrors = ref([])
const approveError = ref(null)
const queued = ref(null)
const logErrors = ref(0)
const loads = ref(0) // bumped on every (re)load, so the failed recipients list follows
let abort = null

const editable = computed(() => c.value && (c.value.status === 'draft' || c.value.status === 'ready'))
const sending = computed(() => c.value && (c.value.status === 'approved' || c.value.status === 'completed'))
const needsAck = computed(() => {
  const a = c.value?.audience
  return !!a && a.unsupported_criteria.length > 0 && !a.unsupported_acknowledged
})
const stale = computed(() => {
  const a = c.value?.audience
  return !!a && new Date(a.as_of).toDateString() !== new Date().toDateString()
})

// Length in characters, with the placeholder replaced by the longest name (as the backend checks).
function len(text) {
  const n = c.value?.audience?.reach.longest_first_name ?? 0
  return [...(text || '').replaceAll('{{first_name}}', 'x'.repeat(n))].length
}

const overLimit = computed(() => !!draft.value && (len(draft.value.sms) > 160 || len(draft.value.email_subject) > 80))
const approveBlocker = computed(() => {
  if (!draft.value) return 'Önce mesajları üretin.'
  if (needsAck.value) return 'Önce eksik kriterleri onaylayın.'
  if (stale.value) return 'Önce kitleyi yenileyin.'
  if (overLimit.value) return 'Bir mesaj karakter sınırını aşıyor.'
  if (!draft.value.sms.trim() || !draft.value.email_subject.trim() || !draft.value.email_body.trim()) return 'Tüm mesajları doldurun.'
  return ''
})
const canApprove = computed(() => !approveBlocker.value && !busy.value && !generating.value)

function apply(campaign) {
  c.value = campaign
  brief.value = campaign.brief
  if (campaign.content && !generating.value) draft.value = { ...campaign.content }
  fieldErrors.value = campaign.content_errors || []
  loads.value++
  checkLogs()
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
onBeforeUnmount(() => abort?.abort())

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

async function generate() {
  messageError.value = ''
  streamText.value = ''
  streamNote.value = 'Claude yazıyor…'
  generating.value = true
  abort = new AbortController()
  try {
    await generateMessages(c.value.id, draft.value ? feedback.value.trim() : '', (e) => {
      if (e.type === 'delta') streamText.value += e.delta
      else if (e.type === 'retry') { streamText.value = ''; streamNote.value = 'İlk deneme doğrulamadan geçemedi; Claude yeniden deniyor…' }
      else if (e.type === 'validated') { draft.value = { ...e.content }; fieldErrors.value = [] }
      else if (e.type === 'invalid') { if (e.content) draft.value = { ...e.content }; fieldErrors.value = e.errors || [] }
      else if (e.type === 'error') messageError.value = e.message || 'Mesaj üretimi başarısız oldu.'
    }, abort.signal)
    feedback.value = ''
  } catch (e) {
    if (e.name !== 'AbortError') messageError.value = e.message
  } finally {
    generating.value = false
    streamNote.value = ''
    streamText.value = ''
    checkLogs()
  }
}

async function approve() {
  approveError.value = null
  try {
    const res = await run('approve', () => api.approve(c.value.id, draft.value))
    queued.value = res.queued
    apply(res.campaign)
  } catch (e) { approveError.value = { message: e.message, fields: e.fields || [] } }
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
.stream { margin: 0; white-space: pre-wrap; background: var(--surface-300); padding: var(--space-3); border-radius: var(--radius-md); }
.block { display: block; }
</style>
