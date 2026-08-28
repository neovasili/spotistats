import type { DayValue } from '../lib/types'
import { formatDate, formatDayWithWeekday, formatDurationFull, formatNumber } from '../lib/format'
import { maxOf, sequentialColor, toWeeks } from '../lib/scale'
import { Card } from './Card'
import { Duration } from '../components/Duration'
import { Tooltip, useTooltip } from './Tooltip'

interface Props {
  days: DayValue[]
  timezone: string
}

/**
 * A calendar heatmap of the trailing twelve months.
 *
 * Sequential blue, one hue, more-is-darker -- the canonical encoding for magnitude on a grid. Days
 * with no listening take the surface tone rather than the lightest blue step, so "nothing" is
 * visually distinct from "a little" instead of merely fainter.
 */
export function Calendar({ days, timezone }: Props) {
  const max = maxOf(days, (d) => d.msPlayed)
  const weeks = toWeeks(days)
  const active = days.filter((d) => d.plays > 0)
  const { containerRef, tip, marks } = useTooltip()

  const table = (
    <div className="datatable__scroll">
      <table className="datatable">
        <thead>
          <tr>
            <th scope="col">Date</th>
            <th scope="col" className="num">Listening</th>
            <th scope="col" className="num">Plays</th>
          </tr>
        </thead>
        <tbody>
          {active.map((d) => (
            <tr key={d.date}>
              <td>{formatDate(d.date)}</td>
              <td className="num"><Duration ms={d.msPlayed} /></td>
              <td className="num">{formatNumber(d.plays)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )

  return (
    <Card
      title="Listening activity"
      subtitle={`Trailing 24 months, local days (${timezone})`}
      table={table}
    >
      <div className="chart-plot" ref={containerRef}>
        {/*
          Month labels along the top. Optional at one year and necessary at two: an unlabelled
          two-year grid cannot answer "which stripe is last winter?", which is most of what a
          reader wants from a heatmap. The year is appended at each January so the boundary is
          visible without a second axis.
        */}
        <div className="heatmap__months" aria-hidden="true">
          {monthLabels(weeks).map((m) => (
            <span
              key={m.key}
              className="heatmap__month"
              style={{ gridColumn: `${m.column} / span ${m.span}` }}
            >
              {m.label}
            </span>
          ))}
        </div>
        <div className="heatmap" role="img" aria-label="Daily listening activity. See the table view for values.">
          {weeks.map((week, wi) => (
            <div className="heatmap__week" key={wi}>
              {week.map((day, di) =>
                day === null ? (
                  <span className="heatmap__cell heatmap__cell--empty" key={di} />
                ) : (
                  <span
                    className="heatmap__cell"
                    key={day.date}
                    style={{ background: sequentialColor(day.msPlayed, max) }}
                    // The native title remains the fallback; a 365-cell grid is where the
                    // browser's own tooltip delay is most tedious, and where a touch device
                    // gets nothing at all without the click-to-pin behaviour.
                    title={
                      day.plays > 0
                        ? `${formatDayWithWeekday(day.date)} — ${formatDurationFull(day.msPlayed)}, ${formatNumber(day.plays)} plays`
                        : `${formatDayWithWeekday(day.date)} — nothing`
                    }
                    {...marks(
                      <>
                        {/* The weekday, not just the date. "Was that a Saturday?" is the question a grid
                            of days invites, and it was the one thing the cell could not answer. */}
                        <div className="tooltip__label">{formatDayWithWeekday(day.date)}</div>
                        {day.plays > 0 ? (
                          <>
                            <div className="tooltip__row">{formatDurationFull(day.msPlayed)}</div>
                            <div className="tooltip__row">{formatNumber(day.plays)} plays</div>
                          </>
                        ) : (
                          <div className="tooltip__row">No listening</div>
                        )}
                      </>,
                    )}
                  />
                ),
              )}
            </div>
          ))}
        </div>
        <Tooltip tip={tip} />
      </div>
      <div className="heatmap__legend">
        <span>Less</span>
        <span className="heatmap__swatch" style={{ background: 'var(--surface-2)' }} />
        <span className="heatmap__swatch" style={{ background: 'var(--blue-150)' }} />
        <span className="heatmap__swatch" style={{ background: 'var(--blue-300)' }} />
        <span className="heatmap__swatch" style={{ background: 'var(--blue-450)' }} />
        <span className="heatmap__swatch" style={{ background: 'var(--blue-600)' }} />
        <span>More</span>
      </div>
    </Card>
  )
}

/**
 * One label per month, positioned over the week column where that month begins.
 *
 * Placed by grid column rather than by pixel maths so it stays aligned if the cell size ever
 * changes. A month whose first week is too narrow to hold its name is dropped rather than
 * overlapped — a collided axis is worse than a sparse one.
 */
function monthLabels(weeks: (DayValue | null)[][]): {
  key: string
  label: string
  column: number
  span: number
}[] {
  const out: { key: string; label: string; column: number; span: number }[] = []
  let lastMonth = ''
  weeks.forEach((week, wi) => {
    // The first real day in the column decides which month the column belongs to.
    const day = week.find((d): d is DayValue => d !== null)
    if (!day) return
    const month = day.date.slice(0, 7)
    if (month === lastMonth) return
    lastMonth = month
    const [y, m] = month.split('-')
    const name = MONTH_ABBR[Number(m) - 1] ?? ''
    out.push({
      key: month,
      // January carries the year, so the boundary between the two years is readable without a
      // separate axis.
      label: m === '01' ? `${name} ${y}` : name,
      column: wi + 1,
      span: 4,
    })
  })
  // Drop labels that would collide. A three-letter month needs about three columns; one
  // carrying a year ("Jan 2025") needs roughly twice that, which is why February crowded it at
  // a uniform threshold. Measuring the label rather than assuming a constant keeps this correct
  // if the format ever changes.
  const kept: typeof out = []
  for (const m of out) {
    const prev = kept[kept.length - 1]
    const need = prev && prev.label.length > 3 ? 6 : 3
    if (!prev || m.column - prev.column >= need) kept.push(m)
  }
  return kept
}

const MONTH_ABBR = [
  'Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
  'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec',
]
