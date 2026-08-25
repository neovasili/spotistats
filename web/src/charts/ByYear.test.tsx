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
  it('draws one bar per year across the whole archive', () => {
    // The 12-column grid inherited from the Explorer's month trend silently dropped everything
    // past the twelfth point, so the bar count is the assertion that matters most here.
    const { container } = render(<ByYear years={years} />)
    expect(container.querySelectorAll('.trend__col').length).toBe(17)
  })

  it('names the span it covers', () => {
    render(<ByYear years={years} />)
    expect(screen.getByText(/2009 to 2025/)).toBeTruthy()
  })

  it('keeps a silent year as an explicit gap rather than closing it', () => {
    // A year away from Spotify is a fact about the history. Omitting the point would draw a
    // continuous series straight through a discontinuity.
    const { container } = render(<ByYear years={years} />)
    const labels = Array.from(container.querySelectorAll('.trend__label')).map((l) => l.textContent)
    expect(labels).toContain('2013')
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
