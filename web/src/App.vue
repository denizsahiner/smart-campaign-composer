<template>
  <header class="top">
    <div class="inner">
      <RouterLink to="/" class="brand">Smart Campaign Composer</RouterLink>
      <RouterLink to="/new" class="cta"><BaseButton>Yeni kampanya</BaseButton></RouterLink>
    </div>
  </header>
  <main class="page"><RouterView /></main>
</template>

<script setup>
import BaseButton from './components/BaseButton.vue'
</script>

<style scoped>
.top { border-bottom: 1px solid var(--line); }
.inner { max-width: 720px; margin: 0 auto; padding: var(--space-4); display: flex; justify-content: space-between; align-items: center; }
.brand { font: 400 20px/26px var(--font-serif); color: var(--ink); text-decoration: none; }
.cta { text-decoration: none; }
</style>
