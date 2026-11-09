<template>
  <div class="stack">
    <h1 class="title">Kampanyalar</h1>
    <p v-if="error" class="notice error">{{ error }}</p>
    <p v-else-if="campaigns === null" class="muted">Yükleniyor…</p>
    <div v-else-if="!campaigns.length" class="card">
      <p>Henüz kampanya yok. Tek cümlelik bir brief yazın, Claude hedef kitleyi ve mesajları önersin.</p>
      <RouterLink to="/new"><BaseButton>Yeni kampanya</BaseButton></RouterLink>
    </div>
    <ul v-else class="list">
      <li v-for="c in campaigns" :key="c.id">
        <RouterLink :to="`/campaigns/${c.id}`" class="card item">
          <div class="row spread">
            <span class="subheading">Kampanya {{ c.id }}</span>
            <StatusBadge :status="c.status" />
          </div>
          <p class="brief">{{ c.brief }}</p>
          <p class="small muted">
            <template v-if="c.audience">{{ c.audience.reach.total.toLocaleString('tr-TR') }} kişi · </template>
            Güncellendi {{ new Date(c.updated_at).toLocaleString('tr-TR') }}
          </p>
        </RouterLink>
      </li>
    </ul>
  </div>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { api } from '../api'
import BaseButton from '../components/BaseButton.vue'
import StatusBadge from '../components/StatusBadge.vue'

const campaigns = ref(null)
const error = ref('')
onMounted(async () => {
  try { campaigns.value = await api.list() } catch (e) { error.value = e.message }
})
</script>

<style scoped>
.list { list-style: none; margin: 0; padding: 0; display: grid; gap: var(--space-3); }
.item { display: block; color: inherit; text-decoration: none; }
.item:hover { border-color: var(--brand); }
.brief { margin: var(--space-2) 0; display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; }
p { margin: 0; }
</style>
