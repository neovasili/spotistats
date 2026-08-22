import type { BucketValue } from '../lib/types'
import { formatDuration, formatNumber, hourLabel, weekdayName } from '../lib/format'
import { fraction, maxOf } from '../lib/scale'
import { Card } from './Card'

interface Props {
  title: string
  subtitle?: string
  buckets: BucketValue[]
  label: (bucket: number) => string
  /** Which bucket labels to print on the axis. Labelling all 24 hours is unreadable. */
  labelEvery?: number
}

/**
 * A column chart of listening by time bucket.
 *
 * Sequential single hue: this is magnitude, and the buckets are an ordered scale rather than
 * distinct series. Buckets arrive dense from the snapshot, including empty ones, so a quiet hour
 * reads as "no listening" rather than "no data".
 */
export function Rhythm({ title, subtitle, buckets, label, labelEvery = 1 }: Props) {
  const max = maxOf(buckets, (b) => b.msPlayed)

  const table = (
    <div className="datatable__scroll">
      <table className="datatable">
        <thead>
          <tr>
            <th scope="col">Bucket</th>
            <th scope="col" className="num">Listening</th>
            <th scope="col" className="num">Plays</th>
          </tr>
        </thead>
        <tbody>
          {buckets.map((b) => (
            <tr key={b.bucket}>
              <td>{label(b.bucket)}</td>
              <td className="num">{formatDuration(b.msPlayed)}</td>
              <td className="num">{formatNumber(b.plays)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )

  return (
    <Card title={title} subtitle={subtitle} table={table}>
      <div className="columns" role="img" aria-label={`${title}. See the table view for values.`}>
        {buckets.map((b) => (
          <div
            key={b.bucket}
            className="column"
            title={`${label(b.bucket)} — ${formatDuration(b.msPlayed)}, ${formatNumber(b.plays)} plays`}
          >
            <span
              className="column__fill"
              style={{
                height: `${Math.max(fraction(b.msPlayed, max) * 100, b.msPlayed > 0 ? 2 : 0)}%`,
                background: b.msPlayed > 0 ? 'var(--blue-450)' : 'var(--surface-2)',
              }}
            />
          </div>
        ))}
      </div>
      <div className="columns__axis" aria-hidden="true">
        {buckets.map((b, i) => (
          <span key={b.bucket}>{i % labelEvery === 0 ? label(b.bucket) : ''}</span>
        ))}
      </div>
    </Card>
  )
}

/** Hour-of-day, labelled every three hours so the axis stays readable. */
export function HourRhythm({ buckets, timezone }: { buckets: BucketValue[]; timezone: string }) {
  return (
    <Rhythm
      title="By hour of day"
      subtitle={`Local time (${timezone})`}
      buckets={buckets}
      label={(b) => hourLabel(b).slice(0, 2)}
      labelEvery={3}
    />
  )
}

export function WeekdayRhythm({ buckets }: { buckets: BucketValue[] }) {
  return (
    <Rhythm
      title="By day of week"
      buckets={buckets}
      label={weekdayName}
    />
  )
}
