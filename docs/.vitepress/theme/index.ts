import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import './style.css'
import { installMermaidZoom } from './mermaidZoom'

export default {
  extends: DefaultTheme,
  setup() {
    installMermaidZoom()
  },
} satisfies Theme
