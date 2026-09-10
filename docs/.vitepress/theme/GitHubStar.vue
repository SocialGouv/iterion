<template>
  <div class="github-star-invite">
    <span>Like what we’re building?</span>
    <a href="https://github.com/SocialGouv/iterion" target="_blank" rel="noreferrer" class="github-star-link">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="m12 3 2.8 5.7 6.2.9-4.5 4.4 1.1 6.2-5.6-3-5.6 3 1.1-6.2L3 9.6l6.2-.9L12 3Z" />
      </svg>
      Star on GitHub
      <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M7 17 17 7M7 7h10v10" />
      </svg>
    </a>
  </div>
</template>

<style scoped>
.github-star-invite {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px 14px;
  margin-top: 26px;
  color: var(--vp-c-text-2);
  font-size: 12px;
  line-height: 1.55;
}

.github-star-link {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  min-height: 40px;
  padding: 8px 12px;
  border: 1px solid var(--vp-c-divider);
  border-radius: 8px;
  background: var(--vp-c-bg-soft);
  color: var(--vp-c-text-1);
  font-weight: 600;
  white-space: nowrap;
  transition: border-color .18s, background .18s;
}

.github-star-link svg { flex-shrink: 0; }
.github-star-link svg:first-child { color: var(--vp-c-brand-1); }
.github-star-link svg:last-child { color: var(--vp-c-text-2); }
.github-star-link:hover { border-color: var(--vp-c-brand-1); background: var(--vp-c-brand-soft); }
.github-star-link:focus-visible { outline: 2px solid var(--vp-c-brand-1); outline-offset: 4px; }

@media (max-width: 959px) {
  .github-star-invite { justify-content: center; }
}

@media (prefers-reduced-motion: reduce) {
  .github-star-link { transition: none; }
}
</style>
