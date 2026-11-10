<template>
  <span class="badge label" :style="{ '--c': `var(--${color})` }"><i class="dot" />{{ text }}</span>
</template>

<script setup>
import { computed } from 'vue'
const props = defineProps({ status: { type: String, required: true } })

// Status is always dot + label, never color alone.
const MAP = {
  draft: 'muted', ready: 'brand', scheduled: 'warning', approved: 'success', completed: 'success',
  pending: 'muted', sending: 'brand', sent: 'success', retry_scheduled: 'warning', failed: 'danger', skipped: 'muted',
  warn: 'warning', error: 'danger',
}
const LABELS = {
  draft: 'Taslak', ready: 'Hazır', scheduled: 'Zamanlandı', approved: 'Onaylandı', completed: 'Tamamlandı',
  pending: 'Bekliyor', sending: 'Gönderiliyor', sent: 'Gönderildi', retry_scheduled: 'Yeniden denenecek', failed: 'Başarısız', skipped: 'Atlandı',
  warn: 'Uyarı', error: 'Hata',
}
const color = computed(() => MAP[props.status] || 'muted')
const text = computed(() => {
  return LABELS[props.status] || props.status
})
</script>

<style scoped>
.badge { display: inline-flex; align-items: center; gap: var(--space-1); padding: 2px var(--space-2); border: 1px solid var(--line); border-radius: var(--radius-sm); background: var(--surface-200); color: var(--ink); text-transform: uppercase; }
.dot { width: 8px; height: 8px; border-radius: 50%; background: var(--c); }
</style>
