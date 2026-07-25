import { inBrowser } from 'vitepress'

// Click any rendered mermaid diagram to view it full-screen with zoom + pan.
//
// It does NOT clone the SVG. Cloning a mermaid SVG fights back on every front
// (Vue reactivity, mermaid re-applying its own max-width style, and a
// Chromium/Firefox <foreignObject> clone-ghosting bug that paints a second
// copy). Instead we promote the ORIGINAL .mermaid element to a fixed,
// full-screen box with a CSS class and scale it with a CSS variable — one
// element, one render, no copy can exist.

let installed = false

export function installMermaidZoom() {
  if (!inBrowser || installed) return
  installed = true
  document.addEventListener('click', (e) => {
    const t = e.target as HTMLElement | null
    if (!t || t.closest('a') || t.closest('.mermaid-zoom-bar')) return
    if (document.querySelector('.mermaid-zoomed')) return
    const cont = t.closest('.mermaid') as HTMLElement | null
    if (cont && cont.querySelector('svg')) openZoom(cont)
  })
}

function openZoom(cont: HTMLElement) {
  const backdrop = document.createElement('div')
  backdrop.className = 'mermaid-zoom-backdrop'

  const bar = document.createElement('div')
  bar.className = 'mermaid-zoom-bar'

  let scale = 1
  const apply = () => cont.style.setProperty('--mermaid-zoom', String(scale))
  const zoom = (f: number) => {
    scale = Math.max(0.4, Math.min(scale * f, 8))
    apply()
  }
  const reset = () => {
    scale = 1
    apply()
    cont.scrollTo?.(0, 0)
  }
  const mk = (label: string, title: string, fn: () => void) => {
    const b = document.createElement('button')
    b.type = 'button'
    b.textContent = label
    b.title = title
    b.setAttribute('aria-label', title)
    b.addEventListener('click', (ev) => {
      ev.stopPropagation()
      fn()
    })
    bar.appendChild(b)
  }
  mk('+', 'Zoom in', () => zoom(1.25))
  mk('−', 'Zoom out', () => zoom(0.8))
  mk('⤡', 'Reset', reset)
  mk('✕', 'Close (Esc)', close)

  const onKey = (ev: KeyboardEvent) => {
    if (ev.key === 'Escape') close()
    else if (ev.key === '+' || ev.key === '=') zoom(1.25)
    else if (ev.key === '-') zoom(0.8)
    else if (ev.key === '0') reset()
  }
  const onWheel = (ev: WheelEvent) => {
    ev.preventDefault()
    zoom(ev.deltaY < 0 ? 1.12 : 0.89)
  }
  backdrop.addEventListener('click', close)
  document.addEventListener('keydown', onKey)
  cont.addEventListener('wheel', onWheel, { passive: false })

  function close() {
    document.removeEventListener('keydown', onKey)
    cont.removeEventListener('wheel', onWheel)
    cont.classList.remove('mermaid-zoomed')
    cont.style.removeProperty('--mermaid-zoom')
    backdrop.remove()
    bar.remove()
    document.documentElement.style.overflow = ''
  }

  document.documentElement.style.overflow = 'hidden'
  document.body.appendChild(backdrop)
  cont.classList.add('mermaid-zoomed')
  document.body.appendChild(bar)
  apply()
}
