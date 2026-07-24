<script setup lang="ts">
import { withBase } from 'vitepress'

const links = [
  { text: 'Home', href: '/' },
  { text: 'Quickstart', href: '/quickstart' },
  { text: 'Why Iterion?', href: '/why-iterion' },
  { text: 'DSL', href: '/dsl' },
  { text: 'Bots', href: '/examples' },
  { text: 'Cloud', href: '/cloud-overview' },
]
</script>

<template>
  <div class="nf">
    <div class="nf-code">404</div>
    <h1 class="nf-title">This page wandered off.</h1>
    <p class="nf-lead">
      The link may be outdated or the page moved — but the graph is still
      running. Here's the way back:
    </p>
    <div class="nf-links">
      <a v-for="l in links" :key="l.href" class="nf-link" :href="withBase(l.href)">{{ l.text }}</a>
    </div>
  </div>
</template>

<style scoped>
.nf {
  max-width: 640px;
  margin: 0 auto;
  padding: 96px 24px 128px;
  text-align: center;
}
.nf-code {
  font-size: 72px;
  font-weight: 800;
  line-height: 1;
  letter-spacing: -0.02em;
  background: linear-gradient(120deg, var(--vp-c-brand-1), var(--vp-c-brand-3));
  -webkit-background-clip: text;
  background-clip: text;
  -webkit-text-fill-color: transparent;
}
.nf-title {
  margin-top: 16px;
  font-size: 28px;
  font-weight: 700;
  border: none;
}
.nf-lead {
  margin-top: 12px;
  color: var(--vp-c-text-2);
  line-height: 1.6;
}
.nf-links {
  margin-top: 32px;
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  justify-content: center;
}
.nf-link {
  padding: 8px 16px;
  border-radius: 20px;
  border: 1px solid var(--vp-c-divider);
  background: var(--vp-c-bg-soft);
  color: var(--vp-c-text-1);
  font-size: 14px;
  font-weight: 500;
  transition: border-color 0.2s ease, color 0.2s ease;
}
.nf-link:hover {
  border-color: var(--vp-c-brand-1);
  color: var(--vp-c-brand-1);
  text-decoration: none;
}
</style>
