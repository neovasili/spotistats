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
  /**
   * Values that move the element without changing the page's height.
   *
   * The ResizeObserver below cannot see those. A fixed-height panel opening ABOVE a
   * fill-to-viewport list is the exact case: the list shrinks by however much the panel takes,
   * so the total is unchanged by construction, so no resize fires, so the measurement that
   * would have shrunk the list never runs. Chicken and egg. Passing the thing that triggered
   * the layout change breaks it.
   */
  deps: readonly unknown[] = [],
): [React.RefObject<T | null>, number | undefined] {
  const ref = useRef<T | null>(null)
  const [maxHeight, setMaxHeight] = useState<number | undefined>()

  const measure = useCallback(() => {
    const el = ref.current
    if (!el) return
    const rect = el.getBoundingClientRect()
    // visualViewport tracks the *visible* height on mobile, where innerHeight includes chrome
    // that is currently collapsed.
    const viewport = window.visualViewport?.height ?? window.innerHeight

    const available = viewport - rect.top - trailingSpace(el) - bottomGap
    // A floor keeps the element usable when `available` goes small or negative -- below this it
    // would be a slit. 160px is about four table rows.
    //
    // It is deliberately low. The floor is the one thing that can break the
    // "everything fits one viewport" contract, because exceeding the available space is exactly
    // what it does; keeping it small means it only bites on genuinely short windows, where
    // something has to give and a scrollable page is the least bad answer.
    setMaxHeight(Math.max(available, 160))
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
    // eslint-disable-next-line react-hooks/exhaustive-deps -- deps is the caller's list of
    // layout triggers, spread deliberately.
  }, [measure, ...deps])

  return [ref, maxHeight]
}

/**
 * The layout space that renders BELOW `el` and outside it: its ancestors' bottom padding, their
 * following siblings, and so on up to the body.
 *
 * This exists because the element is not the last thing on the page — it sits inside a card with
 * padding, inside a page wrapper with more padding — so sizing it to `viewport - top` left that
 * trailing space hanging past the bottom of the screen and the page still scrolled by ~77px.
 *
 * Two wrong answers were tried first, and both are worth recording:
 *
 *  1. **A hardcoded allowance.** 52px, for a Load more block that actually measured 89. Guessing
 *     a layout constant is guessing about something the browser already knows.
 *  2. **`documentElement.scrollHeight - element.bottom`.** Reasonable-looking and unstable:
 *     scrollHeight never reports less than the viewport, so on a page that FITS it measures
 *     leftover empty space as though it were content. Subtracting that shrinks the element,
 *     which leaves more empty space, which subtracts again — it walks down to the floor on every
 *     screen size, which is exactly what it did.
 *
 * Summing (parent.bottom − child.bottom) up the tree is independent of the element's own height,
 * so it is a fixed quantity rather than a feedback loop, and it picks up any future sibling or
 * padding change without anyone remembering this function exists.
 */
function trailingSpace(el: HTMLElement): number {
  let total = 0
  let node: HTMLElement = el
  while (node.parentElement && node !== document.body) {
    const parent = node.parentElement
    // Negative would mean the child overflows its parent, which contributes nothing below.
    total += Math.max(0, parent.getBoundingClientRect().bottom - node.getBoundingClientRect().bottom)
    node = parent
  }
  return total
}
