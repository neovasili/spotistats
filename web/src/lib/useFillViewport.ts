import { useCallback, useEffect, useRef, useState } from 'react'

/**
 * Measures how much vertical space an element has between its own top and the bottom of the
 * viewport, for use as a `max-height`.
 *
 * A `max-height` (rather than a fixed `height`) is what gives the requested behaviour exactly:
 * the element extends to the bottom of the view, and a scrollbar appears only when the content
 * genuinely exceeds that. A fixed height would leave a short list padded with dead space and
 * an always-visible scroll track.
 *
 * This needs measurement because CSS cannot express "from wherever I happen to start, down to
 * the viewport bottom" — the element's offset depends on a filter row whose height changes with
 * wrapping. `100dvh` is used as the viewport reference so mobile browser chrome collapsing does
 * not leave the list overflowing.
 */
export function useFillViewport<T extends HTMLElement>(
  /** Space to leave beneath the element, in pixels. */
  bottomGap = 24,
): [React.RefObject<T | null>, number | undefined] {
  const ref = useRef<T | null>(null)
  const [maxHeight, setMaxHeight] = useState<number | undefined>()

  const measure = useCallback(() => {
    const el = ref.current
    if (!el) return
    const top = el.getBoundingClientRect().top
    // visualViewport tracks the *visible* height on mobile, where innerHeight includes chrome
    // that is currently collapsed.
    const viewport = window.visualViewport?.height ?? window.innerHeight
    const available = viewport - top - bottomGap
    // A floor keeps the list usable if it is scrolled far down the page, where `available`
    // goes small or negative; below this the list would be a slit.
    setMaxHeight(Math.max(available, 240))
  }, [bottomGap])

  useEffect(() => {
    measure()
    window.addEventListener('resize', measure)
    window.visualViewport?.addEventListener('resize', measure)
    // The element's top moves when the filter row rewraps, which no window event reports.
    const observer =
      typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(measure)
    if (observer && ref.current) observer.observe(document.body)
    return () => {
      window.removeEventListener('resize', measure)
      window.visualViewport?.removeEventListener('resize', measure)
      observer?.disconnect()
    }
  }, [measure])

  return [ref, maxHeight]
}
