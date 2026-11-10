<template>
  <div>
    <div class="row spread">
      <h1 class="title">Loglar</h1>
      <BaseButton variant="secondary" :loading="loading" @click="load()">Yenile</BaseButton>
    </div>
    <p class="small muted lead">
      API ve worker'ın uyarıları ve hataları, en yeni en üstte. Eski kayıtlar belli bir süre sonra silinir (varsayılan 7 gün).
      Bilgi seviyesindeki loglar yalnızca container çıktısında görünür (<code class="code">docker compose logs</code>).
    </p>

    <form class="filters section-gap" @submit.prevent="apply">
      <label class="filter">
        <span class="label">Seviye</span>
        <select v-model="f.level" class="field" @change="apply">
          <option value="">Uyarı ve hatalar</option>
          <option value="error">Sadece hatalar</option>
        </select>
      </label>
      <label class="filter">
        <span class="label">Servis</span>
        <select v-model="f.service" class="field" @change="apply">
          <option value="">Tümü</option>
          <option value="api">api</option>
          <option value="worker">worker</option>
        </select>
      </label>
      <label class="filter narrow">
        <span class="label">Kampanya</span>
        <input v-model.trim="f.campaign_id" class="field" inputmode="numeric" placeholder="12" />
      </label>
      <label class="filter">
        <span class="label">Hata kodu</span>
        <input v-model.trim="f.request_id" class="field mono" placeholder="a1b2c3d4e5f60718" />
      </label>
      <label class="filter grow">
        <span class="label">Ara</span>
        <input v-model.trim="f.q" class="field" placeholder="Mesaj ya da ayrıntı" />
      </label>
      <div class="row actions">
        <BaseButton type="submit">Filtrele</BaseButton>
        <button v-if="filtered" type="button" class="link small" @click="clear">Filtreleri temizle</button>
      </div>
    </form>

    <p v-if="error" class="notice error section-gap">{{ error }}</p>
    <LoadingNote v-else-if="logs === null" class="section-gap" />
    <p v-else-if="!logs.length" class="muted section-gap">
      {{ filtered ? 'Bu filtrelerle eşleşen kayıt yok.' : 'Kayıtlı uyarı ya da hata yok.' }}
    </p>

    <ul v-if="logs && logs.length" class="logs section-gap">
      <li v-for="l in logs" :key="l.id" class="log">
        <details>
          <summary class="head">
            <StatusBadge :status="l.level" />
            <span class="small muted mono">{{ time(l.ts) }}</span>
            <span class="small muted">{{ l.service }}</span>
            <span v-if="l.campaign_id" class="small muted">kampanya {{ l.campaign_id }}</span>
            <span class="msg">{{ l.message }}</span>
            <span v-if="l.attrs.err" class="small muted mono err">{{ l.attrs.err }}</span>
          </summary>
          <div class="detail small stack">
            <p v-if="l.campaign_id" class="row">
              <RouterLink :to="`/campaigns/${l.campaign_id}`">Kampanya {{ l.campaign_id }}</RouterLink>
              <button type="button" class="link small" @click="only('campaign_id', l.campaign_id)">Bu kampanyanın logları</button>
            </p>
            <p v-if="l.request_id" class="row">
              <span>Hata kodu <span class="mono">{{ l.request_id }}</span></span>
              <button type="button" class="link small" @click="only('request_id', l.request_id)">Bu isteğin logları</button>
            </p>
            <pre class="attrs code">{{ JSON.stringify(l.attrs, null, 2) }}</pre>
          </div>
        </details>
      </li>
    </ul>
    <div v-if="next" class="row section-gap">
      <BaseButton variant="secondary" :loading="loadingMore" @click="more">Daha eski kayıtlar</BaseButton>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive, computed, watch } from 'vue'
import LoadingNote from '../components/LoadingNote.vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import BaseButton from '../components/BaseButton.vue'
import StatusBadge from '../components/StatusBadge.vue'

// The filters live in the URL, so a link from a campaign or a shared link opens them.
const FIELDS = ['level', 'service', 'campaign_id', 'request_id', 'q']

const route = useRoute()
const router = useRouter()
const f = reactive(Object.fromEntries(FIELDS.map((k) => [k, ''])))
const logs = ref(null)
const next = ref(null)
const error = ref('')
const loading = ref(false)
const loadingMore = ref(false)

const filtered = computed(() => FIELDS.some((k) => route.query[k]))
const params = () => Object.fromEntries(FIELDS.map((k) => [k, route.query[k] || '']))
const time = (ts) => new Date(ts).toLocaleString('tr-TR')

async function load() {
  loading.value = true
  try {
    const res = await api.logs(params())
    logs.value = res.logs
    next.value = res.next_before
    error.value = ''
  } catch (e) {
    error.value = e.status ? e.message : 'API\'ye ulaşılamıyor.'
  } finally {
    loading.value = false
  }
}

async function more() {
  loadingMore.value = true
  try {
    const res = await api.logs({ ...params(), before: next.value })
    logs.value = [...logs.value, ...res.logs]
    next.value = res.next_before
  } catch (e) {
    error.value = e.status ? e.message : 'API\'ye ulaşılamıyor.'
  } finally {
    loadingMore.value = false
  }
}

function apply() {
  const query = {}
  for (const k of FIELDS) if (f[k]) query[k] = String(f[k])
  router.push({ path: '/logs', query })
}
function clear() {
  for (const k of FIELDS) f[k] = ''
  apply()
}
function only(key, value) {
  for (const k of FIELDS) f[k] = ''
  f[key] = String(value)
  apply()
}

watch(() => route.query, () => {
  for (const k of FIELDS) f[k] = route.query[k] || ''
  load()
}, { immediate: true })
</script>

<style scoped>
.lead { margin-top: var(--space-1); }
.section-gap { margin-top: var(--space-6); }
.filters { display: flex; flex-wrap: wrap; gap: var(--space-3); align-items: flex-end; }
.filter { display: grid; gap: var(--space-1); flex: 1 1 140px; min-width: 0; }
.filter.narrow { flex: 0 1 100px; }
.filter.grow { flex: 2 1 200px; }
.filter .field { padding: var(--space-2) var(--space-3); }
.actions { flex-basis: 100%; }
.link { background: none; border: 0; padding: 0; color: var(--brand); text-decoration: underline; cursor: pointer; font: inherit; }
.logs { list-style: none; margin: 0; padding: 0; border-top: 1px solid var(--line); }
.log { border-bottom: 1px solid var(--line); }
.head { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-1) var(--space-3); padding: var(--space-3) 0; cursor: pointer; list-style: none; }
.head::-webkit-details-marker { display: none; }
.msg { flex: 1 1 200px; min-width: 0; }
.err { flex-basis: 100%; overflow-wrap: anywhere; }
.detail { padding: 0 0 var(--space-3); }
.attrs { margin: 0; padding: var(--space-3); background: var(--surface-300); border-radius: var(--radius-md); white-space: pre-wrap; overflow-wrap: anywhere; }
.mono { font-family: var(--font-mono); }
</style>
