import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import { h } from 'vue'
import { useData } from 'vitepress'
import './style.css'
import { installMermaidZoom } from './mermaidZoom'
import NotFound from './NotFound.vue'

const Layout = () => {
  const { page } = useData()
  return page.value.isNotFound ? h(NotFound) : h(DefaultTheme.Layout)
}

export default {
  extends: DefaultTheme,
  Layout,
  NotFound,
  setup() {
    installMermaidZoom()
  },
} satisfies Theme
