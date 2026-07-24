import { inBrowser } from 'vitepress'

// Click any rendered mermaid diagram to open a full-screen, centered, zoom/pan
// overlay of the same SVG. Dependency-free, theme-aware. The SVG is sized with
// max-width/height and flex-centered (no manual fit math), on an opaque backdrop.

let installed = false

export function installMermaidZoom() {
  if (!inBrowser || installed) return
  installed = true
  document.addEventListener('click', (e) => {
    const target = e.target as HTMLElement | null
    if (!target || target.closest('a')) return
    if (document.querySelector('.mermaid-zoom-overlay')) return
    const container = target.closest('.mermaid') as HTMLElement | null
    const svg = container?.querySelector('svg') as SVGSVGElement | null
    if (svg) openZoomModal(svg)
  })
}

function openZoomModal(source: SVGSVGElement) {
  const overlay = document.createElement('div')
  overlay.className = 'mermaid-zoom-overlay'

  const inner = document.createElement('div')
  inner.className = 'mermaid-zoom-inner'

  // Logical size from the viewBox (a mermaid SVG's reliable dimensions; its
  // width/height attrs and bbox can be tiny for wide/short flowcharts).
  const vb = source.viewBox && source.viewBox.baseVal
  const vbW = vb && vb.width ? vb.width : source.getBoundingClientRect().width || 800
  const vbH = vb && vb.height ? vb.height : source.getBoundingClientRect().height || 400

  const svg = source.cloneNode(true) as SVGSVGElement
  svg.removeAttribute('style')
  svg.classList.add('mermaid-zoom-svg')

  // Fit the SVG to fill the viewport (max-width alone can't upscale a
  // width:auto SVG — it only constrains). Recompute on resize.
  const fitSize = () => {
    const k = Math.min((window.innerWidth * 0.92) / vbW, (window.innerHeight * 0.84) / vbH)
    svg.setAttribute('width', String(Math.round(vbW * k)))
    svg.setAttribute('height', String(Math.round(vbH * k)))
  }
  fitSize()

  inner.appendChild(svg)
  overlay.appendChild(inner)

  let scale = 1
  let tx = 0
  let ty = 0
  const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(v, hi))
  const apply = () => {
    inner.style.transform = `translate(${tx}px, ${ty}px) scale(${scale})`
  }
  const reset = () => {
    scale = 1
    tx = 0
    ty = 0
    apply()
  }
  const zoom = (factor: number) => {
    scale = clamp(scale * factor, 0.25, 14)
    apply()
  }

  const bar = document.createElement('div')
  bar.className = 'mermaid-zoom-bar'
  const mkBtn = (label: string, title: string, fn: () => void) => {
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
  mkBtn('+', 'Zoom in', () => zoom(1.25))
  mkBtn('−', 'Zoom out', () => zoom(0.8))
  mkBtn('⤡', 'Reset', reset)
  mkBtn('✕', 'Close (Esc)', close)
  overlay.appendChild(bar)

  overlay.addEventListener(
    'wheel',
    (ev) => {
      ev.preventDefault()
      zoom(ev.deltaY < 0 ? 1.12 : 0.89)
    },
    { passive: false },
  )

  let dragging = false
  let sx = 0
  let sy = 0
  inner.addEventListener('pointerdown', (ev) => {
    dragging = true
    sx = ev.clientX - tx
    sy = ev.clientY - ty
    inner.setPointerCapture(ev.pointerId)
  })
  inner.addEventListener('pointermove', (ev) => {
    if (!dragging) return
    tx = ev.clientX - sx
    ty = ev.clientY - sy
    apply()
  })
  const endDrag = () => {
    dragging = false
  }
  inner.addEventListener('pointerup', endDrag)
  inner.addEventListener('pointercancel', endDrag)

  overlay.addEventListener('click', (ev) => {
    if (ev.target === overlay) close()
  })
  const onKey = (ev: KeyboardEvent) => {
    if (ev.key === 'Escape') close()
    else if (ev.key === '+' || ev.key === '=') zoom(1.25)
    else if (ev.key === '-') zoom(0.8)
    else if (ev.key === '0') reset()
  }
  document.addEventListener('keydown', onKey)
  window.addEventListener('resize', fitSize)

  function close() {
    document.removeEventListener('keydown', onKey)
    window.removeEventListener('resize', fitSize)
    overlay.remove()
    document.documentElement.style.overflow = ''
    document.documentElement.classList.remove('mermaid-zoom-active')
  }

  document.documentElement.style.overflow = 'hidden'
  document.documentElement.classList.add('mermaid-zoom-active')
  document.body.appendChild(overlay)
  apply()
}
