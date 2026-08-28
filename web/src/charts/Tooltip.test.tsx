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

/** Exactly 100 days, so the per-occurrence averages are arithmetic a reader can check. */
const coverage = {
  firstPlayedAt: '2026-01-01T00:00:00.000Z',
  lastPlayedAt: '2026-04-10T00:00:00.000Z',
  approximate: false,
}

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
    const { container } = render(<HourRhythm buckets={hours} timezone="Europe/Madrid" coverage={coverage} />)
    const col = container.querySelectorAll('.column')[9]!
    fireEvent.focus(col)
    expect(screen.getByText('2h (120m)')).toBeTruthy()
  })

  it('works on the weekday chart too', () => {
    const weekdays: BucketValue[] = Array.from({ length: 7 }, (_, i) => ({
      bucket: i, plays: i === 1 ? 5 : 0, msPlayed: i === 1 ? 1_800_000 : 0,
    }))
    const { container } = render(<WeekdayRhythm buckets={weekdays} coverage={coverage} />)
    // Column 0, because the chart leads with Monday. The bucket NUMBER is still 1 (Go's
    // time.Weekday); only the display order is Monday-first.
    fireEvent.mouseEnter(container.querySelectorAll('.column')[0]!)
    // Scoped to the tooltip: "Mon" also appears on the axis and in the table view.
    const tip = container.querySelector('.tooltip')!
    expect(tip.textContent).toContain('Mon')
    // Under an hour the readable form is already minutes, so it is not restated.
    expect(tip.textContent).toContain('30m')
    expect(tip.textContent).not.toContain('(30m)')
  })

  it('leads the weekday chart with Monday, not with Sunday', () => {
    // The stored buckets are Go's time.Weekday (0 = Sunday) and stay that way -- renumbering them
    // would mislabel every bar until the next nightly histogram rebuild. The ORDER is fixed at
    // render, and this is what asserts it.
    const weekdays: BucketValue[] = Array.from({ length: 7 }, (_, i) => ({
      bucket: i, plays: i, msPlayed: i * 600_000,
    }))
    const { container } = render(<WeekdayRhythm buckets={weekdays} coverage={coverage} />)
    const axis = [...container.querySelectorAll('.columns__axis span')].map((s) => s.textContent)
    expect(axis).toEqual(['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'])
  })

  it('names the weekday of a heatmap day, not just its date', () => {
    // "Was that a Saturday?" is the question a grid of days invites, and a bare date cannot
    // answer it. 2026-08-20 was a Thursday.
    const { container } = render(<Calendar days={days} timezone="Europe/Madrid" />)
    fireEvent.mouseEnter(container.querySelector('.heatmap__cell:not(.heatmap__cell--empty)')!)
    expect(container.querySelector('.tooltip__label')!.textContent).toContain('Thu')
  })

  it('restates an hour bucket as an average per day', () => {
    // 2h over a 100-day window is 72 seconds a day, which formatDuration rounds to "1m".
    const { container } = render(
      <HourRhythm buckets={hours} timezone="Europe/Madrid" coverage={coverage} />,
    )
    fireEvent.mouseEnter(container.querySelectorAll('.column')[9]!)
    expect(container.querySelector('.tooltip')!.textContent).toContain('avg 1m per day')
  })

  it('never prints "avg 0m", which reads as a bug rather than as a quiet hour', () => {
    // A sub-minute average is the common case for a dead hour spread over years, and rounding it
    // to zero tells the reader nothing. Same defect the estimated-share figures guard against.
    const sliver = hours.map((h) => (h.bucket === 9 ? { ...h, msPlayed: 60_000 } : h))
    const { container } = render(
      <HourRhythm buckets={sliver} timezone="Europe/Madrid" coverage={coverage} />,
    )
    fireEvent.mouseEnter(container.querySelectorAll('.column')[9]!)
    const text = container.querySelector('.tooltip')!.textContent!
    expect(text).toContain('avg <1m per day')
    expect(text).not.toContain('avg 0m')
  })

  it('divides a weekday bucket by that weekday alone, and names it in full', () => {
    // 100 days from a Thursday holds 14 Mondays, so 30m of Mondays averages 2m each.
    const weekdays: BucketValue[] = Array.from({ length: 7 }, (_, i) => ({
      bucket: i, plays: i === 1 ? 5 : 0, msPlayed: i === 1 ? 1_800_000 : 0,
    }))
    const { container } = render(<WeekdayRhythm buckets={weekdays} coverage={coverage} />)
    fireEvent.mouseEnter(container.querySelectorAll('.column')[0]!)
    expect(container.querySelector('.tooltip')!.textContent).toContain('avg 2m per Monday')
  })

  it('says nothing about the average of an empty bucket', () => {
    // "avg <1m per day" claims listening that did not happen. The tooltip already says "0m, 0
    // plays" and the bar already wears the surface tone.
    const { container } = render(
      <HourRhythm buckets={hours} timezone="Europe/Madrid" coverage={coverage} />,
    )
    fireEvent.mouseEnter(container.querySelectorAll('.column')[4]!) // 04:00, no listening
    const text = container.querySelector('.tooltip')!.textContent!
    expect(text).toContain('0 plays')
    expect(text).not.toContain('avg')
  })

  it('omits the average when there is no window to divide by', () => {
    // An unpopulated coverage row is what this looks like, and no figure beats a wrong one.
    const blank = { firstPlayedAt: null, lastPlayedAt: null, approximate: true }
    const { container } = render(
      <HourRhythm buckets={hours} timezone="Europe/Madrid" coverage={blank} />,
    )
    fireEvent.mouseEnter(container.querySelectorAll('.column')[9]!)
    expect(container.querySelector('.tooltip')!.textContent).not.toContain('avg')
    // ...and the table drops the whole column rather than heading an empty one.
    fireEvent.click(screen.getByRole('button', { name: 'Table' }))
    expect(screen.queryByText('Average')).toBeNull()
  })

  it('carries the average into the table view, so the figure is not hover-only', () => {
    // Card's contract: every chart ships a tabular equivalent. A tooltip-only figure would be
    // available to a mouse and to nothing else.
    render(<HourRhythm buckets={hours} timezone="Europe/Madrid" coverage={coverage} />)
    fireEvent.click(screen.getByRole('button', { name: 'Table' }))
    expect(screen.getByText('Average')).toBeTruthy()
    expect(screen.getByText('avg 1m per day')).toBeTruthy()
  })
})
