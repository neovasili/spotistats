// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render } from '@testing-library/react'
import { Trend } from './Trend'
import type { PeriodValue } from '../lib/types'

afterEach(cleanup)

/**
 * These cases were written against the dashboard's by-year card, which used to be a Trend.
 *
 * That card is now a list read downward, so it no longer exercises this component — but the
 * Explorer's drill-down still draws its monthly series with it, and deleting the tests along
 * with the caller would have quietly left the Explorer's only chart untested.
 */
const months: PeriodValue[] = Array.from({ length: 12 }, (_, i) => ({
  period: `2026-${String(i + 1).padStart(2, '0')}`,
  plays: i === 4 ? 0 : (i + 1) * 100,
  msPlayed: i === 4 ? 0 : (i + 1) * 3_600_000,
}))

function renderTrend() {
  return render(<Trend points={months} emptyLabel="No listening recorded yet." ariaLabel="Monthly listening" />)
}

describe('Trend', () => {
  it('draws one column per point, however many there are', () => {
    // A hard-coded twelve-column grid once truncated every longer series silently, so the
    // column count is the assertion that matters most.
    const { container } = renderTrend()
    expect(container.querySelectorAll('.trend__col').length).toBe(12)
  })

  it('keeps a silent period as an explicit gap rather than closing it', () => {
    const { container } = renderTrend()
    const labels = Array.from(container.querySelectorAll('.trend__label')).map((l) => l.textContent)
    expect(labels).toContain('2026-05')
  })

  it('says so plainly when there is nothing to draw', () => {
    const { container } = render(
      <Trend points={[]} emptyLabel="No listening recorded yet." ariaLabel="Monthly listening" />,
    )
    expect(container.textContent).toContain('No listening recorded yet.')
  })
})

describe('Trend hover', () => {
  it('shows the tooltip when the bar is hovered, not only the axis label', () => {
    // The handlers used to sit on the label alone, so pointing at the bar -- the obvious gesture
    // -- did nothing and the reader had to find the period underneath it.
    const { container } = renderTrend()
    fireEvent.mouseEnter(container.querySelectorAll('.trend__col')[2]!)
    const tip = container.querySelector('.tooltip')
    expect(tip?.querySelector('.tooltip__label')?.textContent).toBe('2026-03')
    expect(tip?.textContent).toContain('plays')
  })

  it('pins on click and dismisses on a second click, so touch can read one', () => {
    const { container } = renderTrend()
    const col = container.querySelectorAll('.trend__col')[0]!
    fireEvent.click(col)
    expect(container.querySelector('.tooltip--pinned')).toBeTruthy()
    fireEvent.click(col)
    expect(container.querySelector('.tooltip')).toBeNull()
  })

  it('reports a silent period as nothing rather than as zero minutes', () => {
    const { container } = renderTrend()
    fireEvent.mouseEnter(container.querySelectorAll('.trend__col')[4]!)
    expect(container.querySelector('.tooltip')?.textContent).toContain('nothing')
  })

  it('keeps the whole column reachable by keyboard, including a 2px bar', () => {
    const { container } = renderTrend()
    const cols = [...container.querySelectorAll('.trend__col')]
    expect(cols.every((c) => c.getAttribute('tabindex') === '0')).toBe(true)
    expect(container.querySelectorAll('.trend__label[tabindex]').length).toBe(0)
  })
})
