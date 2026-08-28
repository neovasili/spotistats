import type { BucketValue, Dashboard } from '../lib/types'
import {
  formatDuration,
  formatDurationFull,
  formatNumber,
  hourLabel,
  weekdayName,
  weekdayNameLong,
} from '../lib/format'
import { fraction, maxOf } from '../lib/scale'
import { daysInWindow, weekdayOccurrences } from '../lib/window'
import { Card } from './Card'
import { Duration } from '../components/Duration'
import { Tooltip, useTooltip } from './Tooltip'

interface Props {
  title: string
  subtitle?: string
  buckets: BucketValue[]
  label: (bucket: number) => string
  /** Which bucket labels to print on the axis. Labelling all 24 hours is unreadable. */
  labelEvery?: number
  /**
   * The bucket's total restated as a typical-occurrence average, e.g. "avg 12m per day".
   *
   * A bucket total is unreadable as a habit: "4d 7h at 20:00" says nothing until you know it
   * accumulated over seventeen years. Returns undefined when there is no denominator to divide
   * by, so a missing figure is absent rather than wrong.
   */
  average?: (bucket: BucketValue) => string | undefined
}

/**
 * A column chart of listening by time bucket.
 *
 * Sequential single hue: this is magnitude, and the buckets are an ordered scale rather than
 * distinct series. Buckets arrive dense from the snapshot, including empty ones, so a quiet hour
 * reads as "no listening" rather than "no data".
 */
export function Rhythm({ title, subtitle, buckets, label, labelEvery = 1, average }: Props) {
  const max = maxOf(buckets, (b) => b.msPlayed)
  const { containerRef, tip, marks } = useTooltip()
  // Whether the average column exists at all is a property of the chart, not of a row: a table
  // with a header nine rows can fill and fifteen cannot is worse than no column.
  const averaged = average !== undefined && buckets.some((b) => average(b) !== undefined)

  const table = (
    <div className="datatable__scroll">
      <table className="datatable">
        <thead>
          <tr>
            <th scope="col">Bucket</th>
            <th scope="col" className="num">Listening</th>
            {averaged && <th scope="col" className="num">Average</th>}
            <th scope="col" className="num">Plays</th>
          </tr>
        </thead>
        <tbody>
          {buckets.map((b) => (
            <tr key={b.bucket}>
              <td>{label(b.bucket)}</td>
              <td className="num"><Duration ms={b.msPlayed} /></td>
              {averaged && <td className="num">{average?.(b) ?? '—'}</td>}
              <td className="num">{formatNumber(b.plays)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )

  return (
    <Card title={title} subtitle={subtitle} table={table}>
      <div className="chart-plot" ref={containerRef}>
        <div className="columns" role="img" aria-label={`${title}. See the table view for values.`}>
          {buckets.map((b) => {
            const avg = average?.(b)
            return (
              <div
                key={b.bucket}
                className="column"
                // The native title stays as the no-JavaScript and screen-reader fallback; the rich
                // tooltip is what makes the chart readable on a touch device.
                title={
                  `${label(b.bucket)} — ${formatDurationFull(b.msPlayed)}, ${formatNumber(b.plays)} plays` +
                  (avg ? `, ${avg}` : '')
                }
                {...marks(
                  <>
                    <div className="tooltip__label">{label(b.bucket)}</div>
                    <div className="tooltip__row">{formatDurationFull(b.msPlayed)}</div>
                    <div className="tooltip__row">{formatNumber(b.plays)} plays</div>
                    {avg && <div className="tooltip__row">{avg}</div>}
                  </>,
                )}
              >
                <span
                  className="column__fill"
                  style={{
                    height: `${Math.max(fraction(b.msPlayed, max) * 100, b.msPlayed > 0 ? 2 : 0)}%`,
                    background: b.msPlayed > 0 ? 'var(--blue-450)' : 'var(--surface-2)',
                  }}
                />
              </div>
            )
          })}
        </div>
        <Tooltip tip={tip} />
      </div>
      <div className="columns__axis" aria-hidden="true">
        {buckets.map((b, i) => (
          <span key={b.bucket}>{i % labelEvery === 0 ? label(b.bucket) : ''}</span>
        ))}
      </div>
    </Card>
  )
}

/**
 * A bucket total divided by how many times that bucket came round.
 *
 * Two cases print nothing at all. Without a window there is no denominator, and an EMPTY bucket
 * has no average worth stating: the tooltip already says "0m, 0 plays" and the bar already wears
 * the surface tone, so a third line would only repeat it.
 *
 * Everything in between rounds to "<1m" rather than to "0m". A dead hour spread over six thousand
 * days lands there constantly, and "avg 0m per day" reads as a bug rather than as a quiet hour --
 * the same defect the estimated-share figures already guard against, on a third code path. Note
 * the asymmetry with "0m": that one is a measurement, this one is a rounding, and only the second
 * is worth words instead of a figure.
 */
function perOccurrence(ms: number, occurrences: number, unit: string): string | undefined {
  if (occurrences <= 0 || ms <= 0) return undefined
  const each = ms / occurrences
  return `avg ${each < 60_000 ? '<1m' : formatDuration(each)} per ${unit}`
}

/** Hour-of-day, labelled every three hours so the axis stays readable. */
export function HourRhythm({
  buckets,
  timezone,
  coverage,
}: {
  buckets: BucketValue[]
  timezone: string
  coverage: Dashboard['coverage']
}) {
  // Every day contains every hour exactly once, so the denominator is the same for all 24.
  const days = daysInWindow(coverage?.firstPlayedAt, coverage?.lastPlayedAt)
  return (
    <Rhythm
      title="By hour of day"
      subtitle={`Local time (${timezone})`}
      buckets={buckets}
      label={(b) => hourLabel(b).slice(0, 2)}
      labelEvery={3}
      average={(b) => perOccurrence(b.msPlayed, days, 'day')}
    />
  )
}

/**
 * The stored bucket numbers are Go's `time.Weekday`, where 0 is Sunday. The week this app counts
 * starts on MONDAY, so the order is fixed HERE, at the point of display, rather than in the
 * numbering.
 *
 * Renumbering the buckets instead would mean rewriting every stored histogram, and those are only
 * recomputed by the nightly full run -- so between deploying the change and that run, every bar
 * on this chart would be labelled with the wrong day. Reordering at render is instant and needs
 * no coordination. Each bucket keeps its own number, so the label and the per-weekday average
 * still look themselves up correctly.
 */
const MONDAY_FIRST = [1, 2, 3, 4, 5, 6, 0]

export function WeekdayRhythm({
  buckets,
  coverage,
}: {
  buckets: BucketValue[]
  coverage: Dashboard['coverage']
}) {
  // Per weekday, not per day: a window rarely holds a whole number of weeks, so Mondays and
  // Sundays can differ by one and dividing all seven by the same figure would tilt the chart.
  const occurrences = weekdayOccurrences(coverage?.firstPlayedAt, coverage?.lastPlayedAt)
  const byNumber = new Map(buckets.map((b) => [b.bucket, b]))
  // Anything the snapshot did not carry is dropped rather than faked. The rollup emits all seven
  // densely, so this is a guard, not a path.
  const ordered = MONDAY_FIRST.map((n) => byNumber.get(n)).filter((b): b is BucketValue => !!b)

  return (
    <Rhythm
      title="By day of week"
      buckets={ordered}
      label={weekdayName}
      average={(b) => perOccurrence(b.msPlayed, occurrences[b.bucket] ?? 0, weekdayNameLong(b.bucket))}
    />
  )
}
