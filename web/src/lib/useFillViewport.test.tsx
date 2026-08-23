// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { useFillViewport } from './useFillViewport'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

/** Places any measured element at `top` in a viewport of `height`. */
function situate(top: number, height: number) {
  vi.stubGlobal('innerHeight', height)
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockReturnValue({
    top, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: top, toJSON: () => ({}),
  })
}

/** The hook only measures once its ref is attached, so the probe must be a real element. */
function Probe({ gap }: { gap?: number }) {
  const [ref, maxHeight] = useFillViewport<HTMLDivElement>(gap)
  return (
    <div ref={ref} data-testid="probe">
      {maxHeight === undefined ? 'unmeasured' : String(maxHeight)}
    </div>
  )
}

describe('useFillViewport', () => {
  it('fills from the element top down to the viewport bottom, less the gap', () => {
    situate(300, 1000)
    render(<Probe gap={24} />)
    expect(screen.getByTestId('probe').textContent).toBe(String(1000 - 300 - 24))
  })

  it('never collapses to an unusable slit when scrolled far down', () => {
    // Deep in the page the element top approaches (or passes) the viewport bottom, so the
    // arithmetic goes small or negative and the list would become a slit.
    situate(980, 1000)
    render(<Probe gap={24} />)
    expect(screen.getByTestId('probe').textContent).toBe('240')
    cleanup()

    situate(1400, 1000)
    render(<Probe gap={24} />)
    expect(screen.getByTestId('probe').textContent).toBe('240')
  })

  it('prefers the visual viewport, which is what mobile chrome actually leaves visible', () => {
    situate(100, 1000)
    vi.stubGlobal('visualViewport', {
      height: 600,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })
    render(<Probe gap={24} />)
    expect(screen.getByTestId('probe').textContent).toBe(String(600 - 100 - 24))
  })
})
