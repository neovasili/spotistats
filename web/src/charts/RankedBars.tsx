import type { Entry } from '../lib/types'
import { formatDuration, formatNumber } from '../lib/format'
import { maxOf, fraction } from '../lib/scale'
import { Card } from './Card'

interface Props {
  title: string
  subtitle?: string
  entries: Entry[]
  /** Shown beneath the chart when the dimension's totals cannot be compared to the overall
   *  total -- which is the case for artists, albums and especially genres. */
  caveat?: string
}

/**
 * A ranked horizontal bar chart.
 *
 * Sequential single hue, not categorical: the job here is "compare magnitude, low to high", and
 * a categorical palette would imply the entities are the subject and bury whichever bar actually
 * matters. Every bar is directly labelled, so identity never depends on colour.
 */
export function RankedBars({ title, subtitle, entries, caveat }: Props) {
  const max = maxOf(entries, (e) => e.msPlayed)

  const table = (
    <div className="datatable__scroll">
      <table className="datatable">
        <thead>
          <tr>
            <th scope="col">#</th>
            <th scope="col">Name</th>
            <th scope="col" className="num">Listening</th>
            <th scope="col" className="num">Plays</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.id}>
              <td className="num">{e.rank}</td>
              <td>{e.name}</td>
              <td className="num">{formatDuration(e.msPlayed)}</td>
              <td className="num">{formatNumber(e.plays)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )

  return (
    <Card title={title} subtitle={subtitle} table={table}>
      {entries.length === 0 ? (
        <p className="empty">No listening recorded yet.</p>
      ) : (
        <>
          <div className="bars">
            {entries.map((e) => (
              <div
                key={e.id}
                className="bar"
                title={`${e.name} — ${formatDuration(e.msPlayed)}, ${formatNumber(e.plays)} plays`}
              >
                <span className="bar__rank">{e.rank}</span>
                <span className="bar__name">{e.name}</span>
                <span className="bar__track">
                  <span
                    className="bar__fill"
                    style={{
                      width: `${Math.max(fraction(e.msPlayed, max) * 100, 1.5)}%`,
                      background: 'var(--blue-450)',
                    }}
                  />
                </span>
                <span className="bar__value">{formatDuration(e.msPlayed)}</span>
              </div>
            ))}
          </div>
          {caveat && <p className="card__sub" style={{ marginTop: '0.75rem' }}>{caveat}</p>}
        </>
      )}
    </Card>
  )
}
