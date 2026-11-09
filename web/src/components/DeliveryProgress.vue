<template>
  <section class="section">
    <div class="row spread">
      <h2 class="heading">Gönderim</h2>
      <span v-if="finished" class="small muted">Tamamlandı</span>
      <span v-else-if="p" class="small muted live"><i class="pulse" />Canlı</span>
    </div>

    <p v-if="note" class="notice warn row spread">
      <span>{{ note }}</span>
      <BaseButton v-if="canReconnect" variant="secondary" @click="connect">Yeniden bağlan</BaseButton>
    </p>
    <p v-if="!p && !note" class="muted">Gönderim durumu yükleniyor…</p>

    <template v-if="p">
      <div v-for="ch in channels" :key="ch.key" class="channel">
        <div class="row spread">
          <span class="label" :class="ch.key + '-c'">{{ ch.name }}</span>
          <span class="small mono">
            {{ done(ch.key).toLocaleString('tr-TR') }} / {{ p.total[ch.key].toLocaleString('tr-TR') }}
            <template v-if="!finished"> · {{ rate(ch.key) }} mesaj/sn</template>
          </span>
        </div>
        <div v-if="p.total[ch.key]" class="bar" role="progressbar" :aria-label="ch.name + ' gönderim ilerlemesi'"
          aria-valuemin="0" :aria-valuemax="p.total[ch.key]" :aria-valuenow="done(ch.key)">
          <span v-for="seg in SEGMENTS" :key="seg" class="seg" :class="seg"
            :style="{ width: pct(ch.key, seg) + '%' }" />
        </div>
        <p v-else class="small muted">Bu kanalda alıcı yok.</p>
        <ul v-if="p.total[ch.key]" class="counts">
          <li v-for="st in visibleStatuses(ch.key)" :key="st" class="row">
            <StatusBadge :status="st" /><span class="mono">{{ p.channels[ch.key][st].toLocaleString('tr-TR') }}</span>
          </li>
        </ul>
      </div>
      <p class="small muted">
        Sahte sağlayıcı ara sıra hata, gecikme ve 429 döndürür; bu mesajlar bekleme süresi artırılarak (backoff) yeniden denenir.
        Kalıcı hatalar (ör. geçersiz alıcı) ya da deneme hakkı biten mesajlar "Başarısız" sayılır.
        Kanal izni olmayan kişiler kuyruğa hiç alınmaz.
      </p>
    </template>
  </section>
</template>

<script setup>
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { watchDelivery } from '../api'
import BaseButton from './BaseButton.vue'
import StatusBadge from './StatusBadge.vue'

const props = defineProps({ campaignId: { type: Number, required: true } })
const emit = defineEmits(['done'])

const channels = [{ key: 'sms', name: 'SMS' }, { key: 'email', name: 'E-posta' }]
const STATUSES = ['pending', 'sending', 'retry_scheduled', 'sent', 'failed', 'skipped']
// Bar segments, in drawing order; the unfilled rest is what's still waiting.
const SEGMENTS = ['sent', 'failed', 'skipped']

const p = ref(null)
const finished = ref(false)
const note = ref('')
const canReconnect = ref(false)
let stop = null

const done = (ch) => p.value.channels[ch].sent + p.value.channels[ch].failed + p.value.channels[ch].skipped
const pct = (ch, st) => (p.value.total[ch] ? (p.value.channels[ch][st] / p.value.total[ch]) * 100 : 0)
// skipped exists in the schema but the worker doesn't produce it today; show it only if it ever appears.
const visibleStatuses = (ch) => STATUSES.filter((st) => st !== 'skipped' || p.value.channels[ch][st] > 0)
const rate = (ch) => (p.value.rate_per_second[ch] || 0).toLocaleString('tr-TR', { maximumFractionDigits: 1 })

function connect() {
  stop?.()
  note.value = ''
  canReconnect.value = false
  stop = watchDelivery(props.campaignId, {
    onProgress(data) {
      p.value = data
      note.value = ''
    },
    onDone(data) {
      p.value = data
      finished.value = true
      emit('done', data)
    },
    onError(message, retrying) {
      if (finished.value) return
      if (retrying) {
        note.value = 'Bağlantı koptu, yeniden bağlanılıyor…'
        return
      }
      stop?.()
      note.value = message || 'Gönderim durumu alınamadı.'
      canReconnect.value = true
    },
  })
}

onMounted(connect)
onBeforeUnmount(() => stop?.())
</script>

<style scoped>
.channel { display: grid; gap: var(--space-2); }
.sms-c { color: var(--sms); }
.email-c { color: var(--email); }
.bar { display: flex; height: 10px; border-radius: var(--radius-sm); background: var(--surface-300); overflow: hidden; }
.seg { transition: width 0.4s ease; }
.seg.sent { background: var(--success); }
.seg.failed { background: var(--danger); }
.seg.skipped { background: var(--control-line); }
.counts { list-style: none; margin: 0; padding: 0; display: flex; flex-wrap: wrap; gap: var(--space-2) var(--space-4); }
.counts .row { gap: var(--space-2); }
.live { display: inline-flex; align-items: center; gap: var(--space-1); }
.pulse { width: 8px; height: 8px; border-radius: 50%; background: var(--success); animation: pulse 1.6s ease-in-out infinite; }
@keyframes pulse { 50% { opacity: 0.3; } }
@media (prefers-reduced-motion: reduce) { .pulse { animation: none; } .seg { transition: none; } }
</style>
