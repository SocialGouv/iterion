import { inBrowser } from 'vitepress'

// Make every rendered mermaid diagram click-to-zoom: dense diagrams are hard to
// read at their inline size, so a click opens a full-screen, pannable,
// wheel-zoomable overlay of the same SVG. Dependency-free, theme-aware.

let installed = false

export function installMermaidZoom() {
  if (!inBrowser || installed) return
  installed = true

  document.addEventListener('click', (e) => {
    const target = e.target as HTMLElement | null
    if (!target || target.closest('a')) return // let links inside diagrams work
    if (document.querySelector('.mermaid-zoom-overlay')) return // one at a time
    const container = target.closest('.mermaid') as HTMLElement | null
    const svg = container?.querySelector('svg') as SVGSVGElement | null
    if (svg) openZoomModal(svg)
  })
}

function openZoomModal(source: SVGSVGElement) {
  const overlay = document.createElement('div')
  overlay.className = 'mermaid-zoom-overlay'

  const stage = document.createElement('div')
  stage.className = 'mermaid-zoom-stage'

  const svg = source.cloneNode(true) as SVGSVGElement
  // Render at natural size; the stage transform drives all scaling.
  const vb = svg.viewBox && svg.viewBox.baseVal
  const natW = vb && vb.width ? vb.width : source.getBoundingClientRect().width || 800
  const natH = vb && vb.height ? vb.height : source.getBoundingClientRect().height || 400
  svg.removeAttribute('style')
  svg.style.width = natW + 'px'
  svg.style.height = natH + 'px'
  svg.style.maxWidth = 'none'
  svg.style.maxHeight = 'none'
  stage.appendChild(svg)
  overlay.appendChild(stage)

  const bar = document.createElement('div')
  bar.className = 'mermaid-zoom-bar'
  const mkBtn = (label: string, title: string, fn: () => void) => {
    const b = document.createElement('button')
    b.type = 'button'
    b.textContent = label
    b.setAttribute('aria-label', title)
    b.title = title
    b.addEventListener('click', (ev) => {
      ev.stopPropagation()
      fn()
    })
    return b
  }

  // Pan/zoom state.
  let scale = 1
  let tx = 0
  let ty = 0
  const apply = () => {
    stage.style.transform = `translate(${tx}px, ${ty}px) scale(${scale})`
  }
  const fit = () => {
    const vw = window.innerWidth
    const vh = window.innerHeight
    scale = Math.min((vw * 0.92) / natW, (vh * 0.82) / natH)
    scale = Math.max(0.1, Math.min(scale, 8))
    tx = (vw - natW * scale) / 2
    ty = (vh - natH * scale) / 2
    apply()
  }
  const zoomAt = (factor: number, cx: number, cy: number) => {
    const next = Math.max(0.1, Math.min(scale * factor, 12))
    // keep the point under the cursor stationary
    tx = cx - ((cx - tx) * next) / scale
    ty = cy - ((cy - ty) * next) / scale
    scale = next
    apply()
  }

  bar.appendChild(mkBtn('+', 'Zoom in', () => zoomAt(1.25, window.innerWidth / 2, window.innerHeight / 2)))
  bar.appendChild(mkBtn('−', 'Zoom out', () => zoomAt(0.8, window.innerWidth / 2, window.innerHeight / 2)))
  bar.appendChild(mkBtn('⤡', 'Reset', fit))
  bar.appendChild(mkBtn('✕', 'Close (Esc)', close))
  overlay.appendChild(bar)

  // Wheel zoom toward the cursor.
  overlay.addEventListener(
    'wheel',
    (ev) => {
      ev.preventDefault()
      zoomAt(ev.deltaY < 0 ? 1.12 : 0.89, ev.clientX, ev.clientY)
    },
    { passive: false },
  )

  // Drag to pan.
  let dragging = false
  let sx = 0
  let sy = 0
  stage.addEventListener('pointerdown', (ev) => {
    dragging = true
    sx = ev.clientX - tx
    sy = ev.clientY - ty
    stage.setPointerCapture(ev.pointerId)
  })
  stage.addEventListener('pointermove', (ev) => {
    if (!dragging) return
    tx = ev.clientX - sx
    ty = ev.clientY - sy
    apply()
  })
  const endDrag = () => {
    dragging = false
  }
  stage.addEventListener('pointerup', endDrag)
  stage.addEventListener('pointercancel', endDrag)

  // Close on backdrop click (not when dragging the diagram) or Esc.
  overlay.addEventListener('click', (ev) => {
    if (ev.target === overlay) close()
  })
  const onKey = (ev: KeyboardEvent) => {
    if (ev.key === 'Escape') close()
    else if (ev.key === '+' || ev.key === '=') zoomAt(1.25, window.innerWidth / 2, window.innerHeight / 2)
    else if (ev.key === '-') zoomAt(0.8, window.innerWidth / 2, window.innerHeight / 2)
    else if (ev.key === '0') fit()
  }
  document.addEventListener('keydown', onKey)

  function close() {
    document.removeEventListener('keydown', onKey)
    overlay.remove()
    document.documentElement.style.overflow = ''
  }

  document.documentElement.style.overflow = 'hidden'
  document.body.appendChild(overlay)
  fit()
}
