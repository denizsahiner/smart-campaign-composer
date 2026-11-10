<template>
  <p v-if="shown" class="muted">Yükleniyor…</p>
</template>

<script setup>
// Shown only when loading takes longer than DELAY_MS: a fast load (switching tabs) then
// goes straight from nothing to the content instead of flashing this note.
import { ref, onMounted, onBeforeUnmount } from 'vue'

const DELAY_MS = 300
const shown = ref(false)
let timer = null
onMounted(() => { timer = setTimeout(() => { shown.value = true }, DELAY_MS) })
onBeforeUnmount(() => clearTimeout(timer))
</script>
