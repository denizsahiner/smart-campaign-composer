<template>
  <header class="top">
    <div class="inner">
      <RouterLink to="/" class="brand">Smart Campaign Composer</RouterLink>
      <nav class="row">
        <RouterLink to="/" class="nav small" :class="{ current: onCampaigns }">Kampanyalar</RouterLink>
        <RouterLink to="/monitoring" class="nav small" :class="{ current: onMonitoring }">
          <i class="dot" :class="healthTone" :title="healthText" />İzleme
          <span class="sr-only">({{ healthText }})</span>
        </RouterLink>
        <RouterLink to="/new" class="cta"><BaseButton>Yeni kampanya</BaseButton></RouterLink>
      </nav>
    </div>
  </header>
  <p v-if="healthy === false" class="banner small" role="status">{{ healthText }}. Bazı işlemler çalışmayabilir.</p>
  <main class="page"><RouterView /></main>
</template>

<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useRoute } from 'vue-router'
import BaseButton from './components/BaseButton.vue'
import { health } from './api'

const route = useRoute()
// A campaign's own page counts as part of the campaign list.
const onCampaigns = computed(() => route.path === '/' || route.path.startsWith('/campaigns/'))
// The logs page is reached from monitoring and counts as part of it.
const onMonitoring = computed(() => route.path === '/monitoring' || route.path === '/logs')

const HEALTH_MS = 15000
const h = ref(null)
let timer = null

const healthy = computed(() => (h.value ? h.value.status === 'ok' : null))
const healthTone = computed(() => (healthy.value === null ? 'muted' : healthy.value ? 'success' : 'danger'))
const healthText = computed(() => {
  if (!h.value) return 'Sistem durumu kontrol ediliyor'
  if (h.value.status === 'ok') return 'Sistem çalışıyor'
  if (h.value.status === 'unreachable') return 'API\'ye ulaşılamıyor'
  return h.value.db === 'down' ? 'Veritabanına ulaşılamıyor' : 'API sağlıksız'
})

async function check() {
  if (!document.hidden) h.value = await health()
}
onMounted(() => {
  check()
  timer = setInterval(check, HEALTH_MS)
})
onBeforeUnmount(() => clearInterval(timer))
</script>

<style scoped>
.top { border-bottom: 1px solid var(--line); }
.inner { max-width: 720px; margin: 0 auto; padding: var(--space-4); display: flex; justify-content: space-between; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
.brand { font: 400 20px/26px var(--font-serif); color: var(--ink); text-decoration: none; }
.cta { text-decoration: none; }
.nav {
  display: inline-flex; align-items: center; gap: var(--space-2);
  padding: var(--space-2) var(--space-3); border-radius: var(--radius-sm);
  color: var(--ink); text-decoration: none; font-weight: 600;
  box-shadow: inset 0 -2px 0 transparent;
  transition: background-color 120ms, box-shadow 120ms;
}
.nav:hover { background: var(--brand-tint); }
.nav.current { color: var(--brand); box-shadow: inset 0 -2px 0 var(--brand); }
.dot { width: 8px; height: 8px; border-radius: 50%; display: inline-block; }
.dot.success { background: var(--success); }
.dot.danger { background: var(--danger); }
.dot.muted { background: var(--control-line); }
.banner { margin: 0; padding: var(--space-2) var(--space-4); text-align: center; background: #fbe9e7; border-bottom: 1px solid var(--danger); }
.sr-only { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
