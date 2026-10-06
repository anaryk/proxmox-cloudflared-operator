import { memo, type RefObject, useEffect, useRef } from 'react'

import type { Layout, Model } from './types'

// The most room the mini map takes in the corner of the map, in pixels.
const most = { width: 180, height: 220 }

export interface MiniMapProps {
  model: Model
  layout: Layout
  // The rectangle of what the map shows, which the map moves itself as it
  // pans and zooms, so that the mini map is not drawn again for that.
  view: RefObject<SVGRectElement | null>
  // pick brings the point of the map, in layout units, to the middle of
  // the view.
  onPick(x: number, y: number): void
}

// MiniMap is the whole map in small, with the part in view marked; a press
// or a drag on it brings that part into view. It is for the pointer only:
// a screen reader and the keyboard have the map's own items, which pan into
// view as the focus moves, and the list view.
export const MiniMap = memo(function MiniMap({ model, layout, view, onPick }: MiniMapProps) {
  const svg = useRef<SVGSVGElement>(null)
  const scale = Math.min(most.width / layout.width, most.height / layout.height)

  useEffect(() => {
    const el = svg.current
    if (!el) return
    let pressed: number | undefined
    const pick = (e: PointerEvent) => {
      const box = el.getBoundingClientRect()
      onPick((e.clientX - box.left) / scale, (e.clientY - box.top) / scale)
    }
    const down = (e: PointerEvent) => {
      if (e.button !== 0) return
      e.preventDefault()
      pressed = e.pointerId
      el.setPointerCapture?.(e.pointerId)
      pick(e)
    }
    const move = (e: PointerEvent) => {
      if (pressed === e.pointerId) pick(e)
    }
    const up = (e: PointerEvent) => {
      if (pressed !== e.pointerId) return
      pressed = undefined
      el.releasePointerCapture?.(e.pointerId)
    }
    el.addEventListener('pointerdown', down)
    el.addEventListener('pointermove', move)
    el.addEventListener('pointerup', up)
    el.addEventListener('pointercancel', up)
    return () => {
      el.removeEventListener('pointerdown', down)
      el.removeEventListener('pointermove', move)
      el.removeEventListener('pointerup', up)
      el.removeEventListener('pointercancel', up)
    }
  }, [scale, onPick])

  return (
    <svg
      ref={svg}
      className="fm-mini"
      width={Math.round(layout.width * scale)}
      height={Math.round(layout.height * scale)}
      viewBox={`0 0 ${layout.width} ${layout.height}`}
      aria-hidden="true"
    >
      {model.nodes.map((n) => {
        const b = layout.nodes.get(n.id)
        return b && <rect key={n.id} className={`fm-mini-node fm-mini-${n.kind}`} x={b.x} y={b.y} width={b.width} height={b.height} />
      })}
      <rect ref={view} className="fm-mini-view" />
    </svg>
  )
})
