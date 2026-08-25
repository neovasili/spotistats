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

describe('ByYear hover', () => {
  it('shows the tooltip when the bar is hovered, not only the axis label', () => {
    // The handlers used to sit on the label alone, so pointing at the bar -- the obvious gesture
    // -- did nothing and the reader had to find the year underneath it.
    const { container } = render(<ByYear years={years} />)
    const col = container.querySelectorAll('.trend__col')[2]!
    fireEvent.mouseEnter(col)
    const tip = container.querySelector('.tooltip')
    expect(tip?.querySelector('.tooltip__label')?.textContent).toBe('2011')
    expect(tip?.textContent).toContain('plays')
  })

  it('pins on click and dismisses on a second click, so touch can read one', () => {
    const { container } = render(<ByYear years={years} />)
    const col = container.querySelectorAll('.trend__col')[0]!
    fireEvent.click(col)
    expect(container.querySelector('.tooltip--pinned')).toBeTruthy()
    fireEvent.click(col)
    expect(container.querySelector('.tooltip')).toBeNull()
  })

  it('reports a silent year as nothing rather than as zero minutes', () => {
    const { container } = render(<ByYear years={years} />)
    fireEvent.mouseEnter(container.querySelectorAll('.trend__col')[4]!)
    expect(container.querySelector('.tooltip')?.textContent).toContain('nothing')
  })

  it('keeps the whole column reachable by keyboard, including a 2px bar', () => {
    const { container } = render(<ByYear years={years} />)
    const cols = [...container.querySelectorAll('.trend__col')]
    expect(cols.every((c) => c.getAttribute('tabindex') === '0')).toBe(true)
    expect(container.querySelectorAll('.trend__label[tabindex]').length).toBe(0)
  })
})
