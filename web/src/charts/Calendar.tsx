import type { DayValue } from '../lib/types'
import { formatDate, formatDuration, formatNumber } from '../lib/format'
import { maxOf, sequentialColor, toWeeks } from '../lib/scale'
import { Card } from './Card'

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
              <td className="num">{formatDuration(d.msPlayed)}</td>
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
      subtitle={`Trailing 12 months, local days (${timezone})`}
      table={table}
    >
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
                  title={
                    day.plays > 0
                      ? `${formatDate(day.date)} — ${formatDuration(day.msPlayed)}, ${formatNumber(day.plays)} plays`
                      : `${formatDate(day.date)} — nothing`
                  }
                />
              ),
            )}
          </div>
        ))}
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
