<template>
  <section v-if="total" class="section">
    <div class="row spread">
      <h2 class="heading">Gönderilemeyen alıcılar</h2>
      <a :href="api.failuresCSV(campaignId)" download class="button-link">CSV indir</a>
    </div>
    <p class="muted">
      {{ total.toLocaleString('tr-TR') }} alıcıya gönderilemedi: sağlayıcı adresi kalıcı olarak reddetti.
      Bu kişilere, bilgileri düzeltilmeden yeniden gönderim yapılamaz.
    </p>
    <div class="scroll">
      <table class="table">
        <thead>
          <tr><th scope="col">Ad</th><th scope="col">Kanal</th><th scope="col">Adres</th><th scope="col">Neden</th><th scope="col">Zaman</th></tr>
        </thead>
        <tbody>
          <tr v-for="f in failures" :key="f.message_id">
            <td>{{ [f.first_name, f.last_name].filter(Boolean).join(' ') || '—' }}</td>
            <td>{{ f.channel === 'sms' ? 'SMS' : 'E-posta' }}</td>
            <td class="mono addr">{{ f.address }}</td>
            <td :title="f.error">{{ f.reason }}</td>
            <td class="small muted nowrap">{{ new Date(f.failed_at).toLocaleString('tr-TR') }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-if="error" class="notice error">{{ error }}</p>
    <div v-if="next" class="row">
      <BaseButton variant="secondary" :loading="loadingMore" @click="more">Daha fazla</BaseButton>
      <span class="small muted">{{ failures.length.toLocaleString('tr-TR') }} / {{ total.toLocaleString('tr-TR') }} gösteriliyor</span>
    </div>
  </section>
</template>

<script setup>
import { ref, watch } from 'vue'
import { api } from '../api'
import BaseButton from './BaseButton.vue'

// Recipients of an approved campaign that the provider rejected for good (an invalid
// phone number or email address). Temporary failures whose retries ran out are not
// listed: their data is fine. Hidden while there are none.
const props = defineProps({
  campaignId: { type: [Number, String], required: true },
  // Changing it reloads the list (the parent bumps it when delivery finishes).
  version: { type: Number, default: 0 },
})

const failures = ref([])
const total = ref(0)
const next = ref(null)
const error = ref('')
const loadingMore = ref(false)

async function load() {
  try {
    const res = await api.failures(props.campaignId)
    failures.value = res.failures
    total.value = res.total
    next.value = res.next_before
    error.value = ''
  } catch { /* stays hidden: the delivery counts above still show the failures */ }
}

async function more() {
  loadingMore.value = true
  try {
    const res = await api.failures(props.campaignId, { before: next.value })
    failures.value = [...failures.value, ...res.failures]
    next.value = res.next_before
    error.value = ''
  } catch (e) {
    error.value = e.status ? e.message : 'API\'ye ulaşılamıyor.'
  } finally {
    loadingMore.value = false
  }
}

watch(() => [props.campaignId, props.version], load, { immediate: true })
</script>

<style scoped>
.scroll { overflow-x: auto; }
.table { width: 100%; border-collapse: collapse; }
.table th, .table td { padding: var(--space-2) var(--space-3); border-bottom: 1px solid var(--line); text-align: left; font-weight: 400; vertical-align: top; }
.table thead th { font: 600 12px/16px var(--font-sans); letter-spacing: 0.06em; text-transform: uppercase; color: var(--muted); }
.mono { font-family: var(--font-mono); }
.addr { overflow-wrap: anywhere; }
.nowrap { white-space: nowrap; }
/* Looks like a secondary BaseButton; a real link so the browser downloads the file. */
.button-link { padding: var(--space-2) var(--space-4); border-radius: var(--radius-md); font: 600 14px/22px var(--font-sans); background: var(--surface-200); color: var(--brand); border: 1px solid var(--brand); text-decoration: none; }
</style>
