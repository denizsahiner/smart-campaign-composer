<template>
  <button class="btn" :class="variant" :disabled="disabled || loading" :type="type">
    <slot /><span v-if="loading">…</span>
  </button>
</template>

<script setup>
defineProps({
  variant: { type: String, default: 'primary' }, // primary | secondary
  disabled: Boolean,
  loading: Boolean,
  type: { type: String, default: 'button' },
})
</script>

<style scoped>
.btn { padding: var(--space-2) var(--space-4); border-radius: var(--radius-md); font: 600 14px/22px var(--font-sans); cursor: pointer; border: 1px solid transparent; }
.primary { background: var(--brand); color: var(--brand-ink); }
.secondary { background: var(--surface-200); color: var(--brand); border-color: var(--brand); }
.btn:disabled { opacity: 0.45; cursor: not-allowed; }
</style>
