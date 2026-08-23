// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { Calendar } from './Calendar'
import { HourRhythm, WeekdayRhythm } from './Rhythm'
import type { BucketValue, DayValue } from '../lib/types'

afterEach(cleanup)

const days: DayValue[] = [
  { date: '2026-08-20', plays: 12, msPlayed: 3_660_000 },
  { date: '2026-08-21', plays: 0, msPlayed: 0 },
]

const hours: BucketValue[] = Array.from({ length: 24 }, (_, i) => ({
  bucket: i,
  plays: i === 9 ? 40 : 0,
  msPlayed: i === 9 ? 7_200_000 : 0,
}))

describe('chart tooltips', () => {
  it('shows listening time on hover over a heatmap day', () => {
    const { container } = render(<Calendar days={days} timezone="Europe/Madrid" />)
    const cell = container.querySelector('.heatmap__cell:not(.heatmap__cell--empty)')!
    fireEvent.mouseEnter(cell)
    // Both renderings, because that is what the figure is meant to convey.
    expect(screen.getByText('1h 1m (61m)')).toBeTruthy()
    expect(screen.getByText('12 plays')).toBeTruthy()
  })

  it('hides again on mouse leave', () => {
    const { container } = render(<Calendar days={days} timezone="Europe/Madrid" />)
    const cell = container.querySelector('.heatmap__cell:not(.heatmap__cell--empty)')!
    fireEvent.mouseEnter(cell)
    expect(container.querySelector('.tooltip')).toBeTruthy()
    fireEvent.mouseLeave(cell)
    expect(container.querySelector('.tooltip')).toBeNull()
  })

  it('pins on click and survives mouse leave', () => {
    // Without pinning there is no way to read a tooltip on a touch device at all.
    const { container } = render(<Calendar days={days} timezone="Europe/Madrid" />)
    const cell = container.querySelector('.heatmap__cell:not(.heatmap__cell--empty)')!
    fireEvent.click(cell)
    expect(container.querySelector('.tooltip--pinned')).toBeTruthy()
    fireEvent.mouseLeave(cell)
    expect(container.querySelector('.tooltip--pinned')).toBeTruthy()
  })

  it('unpins when the same mark is clicked again, so a pin is never a trap', () => {
    const { container } = render(<Calendar days={days} timezone="Europe/Madrid" />)
    const cell = container.querySelector('.heatmap__cell:not(.heatmap__cell--empty)')!
    fireEvent.click(cell)
    fireEvent.click(cell)
    expect(container.querySelector('.tooltip')).toBeNull()
  })

  it('dismisses a pinned tooltip on Escape', () => {
    const { container } = render(<Calendar days={days} timezone="Europe/Madrid" />)
    const cell = container.querySelector('.heatmap__cell:not(.heatmap__cell--empty)')!
    fireEvent.click(cell)
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(container.querySelector('.tooltip')).toBeNull()
  })

  it('says so plainly for a day with no listening', () => {
    const { container } = render(<Calendar days={days} timezone="Europe/Madrid" />)
    const cells = container.querySelectorAll('.heatmap__cell:not(.heatmap__cell--empty)')
    fireEvent.mouseEnter(cells[1]!)
    expect(screen.getByText('No listening')).toBeTruthy()
  })

  it('opens on keyboard focus, so the charts are not mouse-only', () => {
    const { container } = render(<HourRhythm buckets={hours} timezone="Europe/Madrid" />)
    const col = container.querySelectorAll('.column')[9]!
    fireEvent.focus(col)
    expect(screen.getByText('2h (120m)')).toBeTruthy()
  })

  it('works on the weekday chart too', () => {
    const weekdays: BucketValue[] = Array.from({ length: 7 }, (_, i) => ({
      bucket: i, plays: i === 1 ? 5 : 0, msPlayed: i === 1 ? 1_800_000 : 0,
    }))
    const { container } = render(<WeekdayRhythm buckets={weekdays} />)
    fireEvent.mouseEnter(container.querySelectorAll('.column')[1]!)
    // Scoped to the tooltip: "Mon" also appears on the axis and in the table view.
    const tip = container.querySelector('.tooltip')!
    expect(tip.textContent).toContain('Mon')
    // Under an hour the readable form is already minutes, so it is not restated.
    expect(tip.textContent).toContain('30m')
    expect(tip.textContent).not.toContain('(30m)')
  })
})
