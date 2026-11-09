<template>
  <span class="counter small mono" :class="state">{{ used }} / {{ limit }}</span>
</template>

<script setup>
import { computed } from 'vue'
const props = defineProps({ used: Number, limit: Number })
const state = computed(() => (props.used > props.limit ? 'over' : props.used >= props.limit * 0.9 ? 'near' : ''))
</script>

<style scoped>
.counter { color: var(--muted); }
.near { color: var(--warning); }
.over { color: var(--danger); font-weight: 700; }
</style>
