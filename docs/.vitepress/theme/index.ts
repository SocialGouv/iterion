import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import { h } from 'vue'
import { useData } from 'vitepress'
import './style.css'
import { installMermaidZoom } from './mermaidZoom'
import NotFound from './NotFound.vue'
import GitHubStar from './GitHubStar.vue'

const Layout = () => {
  const { page } = useData()
  return page.value.isNotFound ? h(NotFound) : h(DefaultTheme.Layout, null, {
    'home-hero-actions-after': () => h(GitHubStar),
    'aside-bottom': () => h(GitHubStar),
  })
}

export default {
  extends: DefaultTheme,
  Layout,
  NotFound,
  setup() {
    installMermaidZoom()
  },
} satisfies Theme
