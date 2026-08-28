// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { ByYear } from './ByYear'
import type { PeriodValue } from '../lib/types'

afterEach(cleanup)

/** Seventeen years, one of them silent, ascending -- the real shape of this archive. */
const years: PeriodValue[] = Array.from({ length: 17 }, (_, i) => ({
  period: String(2009 + i),
  plays: i === 4 ? 0 : (i + 1) * 100,
  msPlayed: i === 4 ? 0 : (i + 1) * 3_600_000,
}))

describe('ByYear', () => {
  it('draws one row per year across the whole archive', () => {
    const { container } = render(<ByYear years={years} />)
    expect(container.querySelectorAll('.yearbars__row').length).toBe(17)
  })

  it('reads newest first, so each row lines up with the same year in the card beside it', () => {
    // The whole reason this is a list rather than a column chart: it sits next to "Your year in
    // one artist", which is also newest-first, and a reader runs their eye across one line to
    // get "2025 -- 76 days -- Within Temptation". Reverse either card and that breaks.
    const { container } = render(<ByYear years={years} />)
    const labels = Array.from(container.querySelectorAll('.yearbars__year')).map((y) => y.textContent)
    expect(labels[0]).toBe('2025')
    expect(labels[labels.length - 1]).toBe('2009')
  })

  it('encodes magnitude by bar length, with a floor so a thin year is not absence', () => {
    const { container } = render(<ByYear years={years} />)
    const fills = Array.from(container.querySelectorAll<HTMLElement>('.yearbars__fill'))
    // Newest first, so the first row is the largest year and takes the full track.
    expect(fills[0]!.style.width).toBe('100%')
    // The silent year gets no bar at all -- zero is not a small amount.
    const silent = Array.from(container.querySelectorAll('.yearbars__row'))
      .find((r) => r.querySelector('.yearbars__year')?.textContent === '2013')!
    expect(silent.querySelector<HTMLElement>('.yearbars__fill')!.style.width).toBe('0%')
  })

  it('names the span it covers', () => {
    render(<ByYear years={years} />)
    expect(screen.getByText(/2009 to 2025/)).toBeTruthy()
  })

  it('keeps a silent year as an explicit gap rather than closing it', () => {
    // A year away from Spotify is a fact about the history. Omitting the row would draw a
    // continuous series straight through a discontinuity.
    const { container } = render(<ByYear years={years} />)
    const labels = Array.from(container.querySelectorAll('.yearbars__year')).map((l) => l.textContent)
    expect(labels).toContain('2013')
  })

  it('states every year\'s value on its own row rather than only on hover', () => {
    // The list form earns its keep here: the column chart it replaced put the figure behind a
    // tooltip, which a touch device had to pin and a printed page never showed at all.
    const { container } = render(<ByYear years={years} />)
    const values = container.querySelectorAll('.yearbars__value .duration')
    expect(values.length).toBe(17)
  })

  it('offers the values as a table, like every other card', () => {
    const { container } = render(<ByYear years={years} />)
    fireEvent.click(screen.getByRole('button', { name: 'Table' }))
    expect(container.querySelectorAll('.datatable tbody tr').length).toBe(17)
  })

  it('says which bars cover less than a whole year', () => {
    // The end bars measure eight months and two months against twelve, and nothing about their
    // height says so.
    render(<ByYear years={years} />)
    expect(screen.getByText(/2025 is still in progress and 2009 begins partway/)).toBeTruthy()
  })

  it('renders nothing at all before a rollup has produced a series', () => {
    const { container } = render(<ByYear years={[]} />)
    expect(container.firstChild).toBeNull()
  })
})
