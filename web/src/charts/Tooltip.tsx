import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'

export interface TooltipState {
  content: ReactNode
  /** Position within the plot container, in pixels. */
  x: number
  y: number
  /** Pinned tooltips survive mouseleave; that is what makes them usable by touch. */
  pinned: boolean
}

/**
 * Hover-and-click tooltips for a chart's marks.
 *
 * A native `title` attribute is not enough: it never appears on a touch device, it cannot be
 * reached by keyboard, and its delay makes scanning a 365-cell heatmap tedious. So marks get
 * pointer, focus and click handlers, and clicking PINS the tooltip — which is the only way a
 * touch user can read one at all.
 *
 * Positions are measured relative to the plot container rather than the viewport, so the tooltip
 * travels with the chart when the page scrolls and needs no scroll listener.
 */
export function useTooltip() {
  const containerRef = useRef<HTMLDivElement | null>(null)
  const [tip, setTip] = useState<TooltipState | null>(null)

  const show = useCallback((el: Element, content: ReactNode, pin = false) => {
    const box = containerRef.current?.getBoundingClientRect()
    if (!box) return
    const r = el.getBoundingClientRect()
    setTip({
      content,
      // Centred over the mark, anchored to its top edge.
      x: r.left - box.left + r.width / 2,
      y: r.top - box.top,
      pinned: pin,
    })
  }, [])

  const hide = useCallback((force = false) => {
    setTip((prev) => (prev && prev.pinned && !force ? prev : null))
  }, [])

  const toggle = useCallback((el: Element, content: ReactNode) => {
    setTip((prev) => {
      // Clicking the same mark again dismisses it, so a pinned tooltip is never a trap.
      if (prev?.pinned) return null
      const box = containerRef.current?.getBoundingClientRect()
      if (!box) return prev
      const r = el.getBoundingClientRect()
      return {
        content,
        x: r.left - box.left + r.width / 2,
        y: r.top - box.top,
        pinned: true,
      }
    })
  }, [])

  // A pinned tooltip must be dismissable without hunting for the same mark again.
  useEffect(() => {
    if (!tip?.pinned) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') hide(true)
    }
    const onOutside = (e: MouseEvent) => {
      if (!containerRef.current?.contains(e.target as Node)) hide(true)
    }
    window.addEventListener('keydown', onKey)
    document.addEventListener('click', onOutside)
    return () => {
      window.removeEventListener('keydown', onKey)
      document.removeEventListener('click', onOutside)
    }
  }, [tip?.pinned, hide])

  /** Handlers to spread onto each mark. */
  const marks = useCallback(
    (content: ReactNode) => ({
      tabIndex: 0,
      onMouseEnter: (e: React.MouseEvent) => show(e.currentTarget, content),
      onMouseLeave: () => hide(),
      onFocus: (e: React.FocusEvent) => show(e.currentTarget, content),
      onBlur: () => hide(),
      onClick: (e: React.MouseEvent) => {
        // Without this the document-level dismissal below fires on the same click.
        e.stopPropagation()
        toggle(e.currentTarget, content)
      },
    }),
    [show, hide, toggle],
  )

  return { containerRef, tip, marks }
}

/** Renders the active tooltip inside a position:relative plot container. */
export function Tooltip({ tip }: { tip: TooltipState | null }) {
  if (!tip) return null
  return (
    <div
      className={`tooltip${tip.pinned ? ' tooltip--pinned' : ''}`}
      style={{ left: tip.x, top: tip.y }}
      role="status"
    >
      {tip.content}
    </div>
  )
}
