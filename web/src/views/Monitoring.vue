<template>
  <div>
    <div class="row spread">
      <h1 class="title">İzleme</h1>
      <span class="small muted">
        <template v-if="updatedAt">Güncellendi {{ updatedAt.toLocaleTimeString('tr-TR') }} · her {{ POLL_MS / 1000 }} sn</template>
      </span>
    </div>
    <p class="small muted lead">Tüm kampanyalar genelinde kuyruk, teslimat, Claude çağrıları ve bağımlı servislerin durumu.</p>

    <p v-if="error" class="notice error section-gap">{{ error }}<template v-if="m"> Son alınan değerler gösteriliyor.</template></p>
    <LoadingNote v-if="!m && !error" class="section-gap" />

    <template v-if="m">
      <section class="section">
        <div class="row spread">
          <h2 class="heading">Hatalar</h2>
          <RouterLink to="/logs" class="small">Loglara git →</RouterLink>
        </div>
        <p v-if="logSummary" :class="logSummary.errors ? 'notice error' : 'muted'">
          Son 24 saatte {{ fmt(logSummary.errors) }} hata, {{ fmt(logSummary.warnings) }} uyarı.
        </p>
        <p v-else class="muted">Log özeti alınamadı.</p>
        <p class="small muted">Uyarıların çoğu sahte sağlayıcının hatalarından sonra planlanan yeniden denemelerdir; hatalar kalıcı başarısızlıklardır.</p>
      </section>

      <section class="section">
        <h2 class="heading">Servisler</h2>
        <ul class="services">
          <li v-for="s in services" :key="s.name" class="card row spread">
            <span class="subheading">{{ s.name }}</span>
            <span class="row state"><i class="dot" :class="s.tone" />{{ s.text }}</span>
            <span v-if="s.error" class="small muted mono err">{{ s.error }}</span>
          </li>
        </ul>
      </section>

      <section class="section">
        <h2 class="heading">Kuyruk</h2>
        <div class="metrics">
          <div v-for="ch in CHANNELS" :key="ch.key">
            <div class="metric" :class="ch.key + '-c'">{{ fmt(m.queue_depth[ch.key] || 0) }}</div>
            <div class="small muted">{{ ch.name }} bekliyor</div>
          </div>
        </div>
        <p class="small muted">Bekleyen, gönderilmekte olan ve yeniden denenecek mesajlar. Gönderim kanal başına saniyede en fazla 100 mesajla sınırlı.</p>
      </section>

      <section class="section">
        <h2 class="heading">Mesajlar</h2>
        <table class="table">
          <thead>
            <tr><th scope="col">Durum</th><th v-for="ch in CHANNELS" :key="ch.key" scope="col" class="num">{{ ch.name }}</th></tr>
          </thead>
          <tbody>
            <tr v-for="st in STATUSES" :key="st">
              <th scope="row"><StatusBadge :status="st" /></th>
              <td v-for="ch in CHANNELS" :key="ch.key" class="num mono">{{ fmt(m.messages[ch.key][st]) }}</td>
            </tr>
          </tbody>
        </table>
      </section>

      <section class="section">
        <h2 class="heading">Claude</h2>
        <div class="metrics wrap">
          <div><div class="metric">{{ fmt(m.llm.calls) }}</div><div class="small muted">çağrı</div></div>
          <div><div class="metric">{{ fmt(m.llm.validation_failures) }}</div><div class="small muted">doğrulamadan geçmeyen</div></div>
          <div><div class="metric">{{ fmt(m.llm.errors) }}</div><div class="small muted">hata</div></div>
          <div><div class="metric">{{ fmt(Math.round(m.llm.avg_latency_ms)) }}</div><div class="small muted">ort. ms</div></div>
        </div>
        <p class="small muted">
          Token: {{ fmt(m.llm.input_tokens) }} girdi · {{ fmt(m.llm.output_tokens) }} çıktı.
          Doğrulamadan geçmeyen yanıtlar hatalarıyla birlikte Claude'a geri gönderilir ve düzeltilmesi istenir.
        </p>
      </section>

      <section v-if="providerTotals" class="section">
        <h2 class="heading">Sahte sağlayıcı</h2>
        <table class="table">
          <thead>
            <tr><th scope="col" /><th v-for="ch in CHANNELS" :key="ch.key" scope="col" class="num">{{ ch.name }}</th></tr>
          </thead>
          <tbody>
            <tr v-for="row in PROVIDER_ROWS" :key="row.key">
              <th scope="row">{{ row.label }}</th>
              <td v-for="ch in CHANNELS" :key="ch.key" class="num mono">{{ fmt(providerTotals[ch.key]?.[row.key] || 0) }}</td>
            </tr>
            <tr v-for="reason in rejectReasons" :key="reason">
              <th scope="row" class="indent">Reddedilen: {{ REJECT_LABELS[reason] || reason }}</th>
              <td v-for="ch in CHANNELS" :key="ch.key" class="num mono">{{ fmt(providerTotals[ch.key]?.rejected?.[reason] || 0) }}</td>
            </tr>
          </tbody>
        </table>
        <p class="small muted">
          "Tekrar" sayısı, aynı mesajın yeniden denendiğinde sağlayıcının daha önceki teslimatı döndürdüğü istekleri gösterir.
          Yani yeniden denemeler ikinci bir gönderime dönüşmemiştir.
        </p>
      </section>
    </template>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import LoadingNote from '../components/LoadingNote.vue'
import { api } from '../api'
import StatusBadge from '../components/StatusBadge.vue'

const POLL_MS = 3000
const CHANNELS = [{ key: 'sms', name: 'SMS' }, { key: 'email', name: 'E-posta' }]
const STATUSES = ['pending', 'sending', 'retry_scheduled', 'sent', 'failed', 'skipped']
const PROVIDER_ROWS = [
  { key: 'requests', label: 'İstek' },
  { key: 'deliveries', label: 'Teslimat' },
  { key: 'duplicates', label: 'Tekrar (aynı mesaj)' },
  { key: 'hung_after_delivery', label: 'Teslim edildi, yanıt dönmedi' },
]
const REJECT_LABELS = {
  invalid_recipient: 'geçersiz alıcı', invalid_request: 'geçersiz istek', in_progress: 'işlemde',
  rate_limited: 'hız sınırı (429)', throttled: 'kısıtlama penceresi (429)', server_error: 'sunucu hatası', timeout: 'zaman aşımı',
}
const SERVICE_TEXT = {
  ok: ['success', 'Çalışıyor'], unavailable: ['danger', 'Sağlıksız'], unreachable: ['danger', 'Erişilemiyor'],
}

const m = ref(null)
const logSummary = ref(null)
const error = ref('')
const updatedAt = ref(null)
let timer = null
let inFlight = false

const fmt = (n) => (n ?? 0).toLocaleString('tr-TR')

function service(name, dep) {
  if (!dep) return { name, tone: 'muted', text: 'Raporlanmıyor' }
  const [tone, text] = SERVICE_TEXT[dep.status] || ['warning', dep.status]
  return { name, tone, text, error: dep.error }
}
const services = computed(() => [
  { name: 'API ve veritabanı', tone: error.value ? 'danger' : 'success', text: error.value ? 'Erişilemiyor' : 'Çalışıyor' },
  service('Worker', m.value.worker),
  service('Sahte sağlayıcı', m.value.provider),
])

const providerTotals = computed(() => (m.value?.provider?.status === 'ok' ? m.value.provider.totals : null))
const rejectReasons = computed(() => {
  const seen = new Set()
  for (const ch of CHANNELS) Object.keys(providerTotals.value?.[ch.key]?.rejected || {}).forEach((r) => seen.add(r))
  return [...seen].sort()
})

async function load() {
  if (inFlight || document.hidden) return
  inFlight = true
  // The log summary is fetched alongside; if only it fails, the metrics still show.
  const summary = api.logSummary({ since_hours: 24 }).catch(() => null)
  try {
    m.value = await api.metrics()
    error.value = ''
    updatedAt.value = new Date()
  } catch (e) {
    error.value = e.status ? e.message : 'API\'ye ulaşılamıyor.'
  } finally {
    logSummary.value = await summary
    inFlight = false
  }
}

onMounted(() => {
  load()
  timer = setInterval(load, POLL_MS)
})
onBeforeUnmount(() => clearInterval(timer))
</script>

<style scoped>
.lead { margin-top: var(--space-1); }
.section-gap { margin-top: var(--space-6); }
.services { list-style: none; margin: 0; padding: 0; display: grid; gap: var(--space-2); }
.state { gap: var(--space-2); }
.err { flex-basis: 100%; overflow-wrap: anywhere; }
.dot { width: 8px; height: 8px; border-radius: 50%; display: inline-block; }
.dot.success { background: var(--success); }
.dot.danger { background: var(--danger); }
.dot.warning { background: var(--warning); }
.dot.muted { background: var(--control-line); }
.metrics { display: flex; gap: var(--space-8); padding: var(--space-4) 0; border-top: 1px solid var(--line); border-bottom: 1px solid var(--line); }
.metrics.wrap { flex-wrap: wrap; gap: var(--space-4) var(--space-8); }
.sms-c { color: var(--sms); }
.email-c { color: var(--email); }
.table { width: 100%; border-collapse: collapse; }
.table th, .table td { padding: var(--space-2) var(--space-3); border-bottom: 1px solid var(--line); text-align: left; font-weight: 400; }
.table thead th { font: 600 12px/16px var(--font-sans); letter-spacing: 0.06em; text-transform: uppercase; color: var(--muted); }
.table .num { text-align: right; }
.indent { padding-left: var(--space-6) !important; color: var(--muted); }
</style>
