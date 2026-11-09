<template>
  <form class="stack" @submit.prevent="submit">
    <h1 class="title">Yeni kampanya</h1>
    <label class="stack">
      <span class="subheading">Kampanyanızı anlatın</span>
      <textarea v-model="brief" class="field" rows="5" maxlength="2000"
        placeholder="İstanbul'da son 30 gün içinde sepetine ürün ekleyip satın almayan kullanıcılara %30 indirim hatırlatması gönder. Samimi ama kısa olsun." />
      <span class="small muted">{{ brief.length }} / 2000</span>
    </label>
    <p v-if="error" class="notice error">{{ error }}</p>
    <BaseButton type="submit" :disabled="!brief.trim()" :loading="busy">Oluştur</BaseButton>
  </form>
</template>

<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import BaseButton from '../components/BaseButton.vue'

const router = useRouter()
const brief = ref('')
const busy = ref(false)
const error = ref('')

async function submit() {
  busy.value = true
  error.value = ''
  try {
    const res = await api.create(brief.value.trim())
    router.push(`/campaigns/${res.campaign.id}`)
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<style scoped>
label { display: block; }
label > * + * { margin-top: var(--space-2); display: block; }
</style>
