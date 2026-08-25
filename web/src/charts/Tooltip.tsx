import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'

export interface TooltipState {
  content: ReactNode
  /** Position within the plot container, in pixels. */
  x: number
  y: number
  /**
   * True when there was not enough room above the mark, so the tooltip hangs downward from the
   * anchor's top edge instead of upward from it.
   */
  below: boolean
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

  /**
   * The height a tooltip needs above a mark before it starts covering the card's own title.
   *
   * Three lines plus padding and the 8px offset. Approximate on purpose: measuring the tooltip
   * would mean rendering it first, and being 6px out here costs nothing while a layout pass on
   * every pointer move costs something real.
   */
  const CLEARANCE = 64

  const place = useCallback((el: Element, content: ReactNode, pinned: boolean): TooltipState | null => {
    const box = containerRef.current?.getBoundingClientRect()
    if (!box) return null
    const r = el.getBoundingClientRect()
    const top = r.top - box.top
    return {
      content,
      // Centred over the mark, anchored to its top edge.
      x: r.left - box.left + r.width / 2,
      y: top,
      // A full-height bar's top edge IS the top of the plot, so upward would put the tooltip over
      // the card title. Hanging downward from the same edge covers the mark instead, which is the
      // thing the reader is already pointing at and can therefore afford to lose.
      below: top < CLEARANCE,
      pinned,
    }
  }, [])

  const show = useCallback((el: Element, content: ReactNode, pin = false) => {
    setTip((prev) => place(el, content, pin) ?? prev)
  }, [place])

  const hide = useCallback((force = false) => {
    setTip((prev) => (prev && prev.pinned && !force ? prev : null))
  }, [])

  const toggle = useCallback((el: Element, content: ReactNode) => {
    setTip((prev) => {
      // Clicking the same mark again dismisses it, so a pinned tooltip is never a trap.
      if (prev?.pinned) return null
      return place(el, content, true) ?? prev
    })
  }, [place])

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

  /**
   * Handlers to spread onto each mark.
   *
   * `anchor` separates the thing you POINT AT from the thing the tooltip is positioned against.
   * They are usually the same element, but not always: a bar chart wants the whole column to be
   * hoverable — a 2px-tall bar for a quiet year is not a hit target — while the tooltip still
   * belongs just above the bar's own top edge rather than floating at the top of the plot. Return
   * null to fall back to the hovered element.
   */
  const marks = useCallback(
    (content: ReactNode, anchor?: (el: Element) => Element | null | undefined) => {
      const at = (el: Element) => anchor?.(el) ?? el
      return {
        tabIndex: 0,
        onMouseEnter: (e: React.MouseEvent) => show(at(e.currentTarget), content),
        onMouseLeave: () => hide(),
        onFocus: (e: React.FocusEvent) => show(at(e.currentTarget), content),
        onBlur: () => hide(),
        onClick: (e: React.MouseEvent) => {
          // Without this the document-level dismissal below fires on the same click.
          e.stopPropagation()
          toggle(at(e.currentTarget), content)
        },
      }
    },
    [show, hide, toggle],
  )

  return { containerRef, tip, marks }
}

/** Renders the active tooltip inside a position:relative plot container. */
export function Tooltip({ tip }: { tip: TooltipState | null }) {
  if (!tip) return null
  return (
    <div
      className={`tooltip${tip.pinned ? ' tooltip--pinned' : ''}${tip.below ? ' tooltip--below' : ''}`}
      style={{ left: tip.x, top: tip.y }}
      role="status"
    >
      {tip.content}
    </div>
  )
}
