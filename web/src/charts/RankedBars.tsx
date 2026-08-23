import type { Entry } from '../lib/types'
import { formatDurationFull, formatNumber } from '../lib/format'
import { Duration } from '../components/Duration'
import { maxOf, fraction } from '../lib/scale'
import { Card } from './Card'
import { EntityName, entityContext } from '../components/EntityName'

interface Props {
  title: string
  subtitle?: string
  entries: Entry[]
  /** Shown beneath the chart when the dimension's totals cannot be compared to the overall
   *  total -- which is the case for artists, albums and especially genres. */
  caveat?: string
  /**
   * Replaces the chart entirely when the data cannot exist, as opposed to not existing yet.
   *
   * These are different states and must not share a message: "No listening recorded yet"
   * implies waiting will fix it, which is wrong when the upstream field has been removed.
   */
  unavailable?: string
}

/**
 * A ranked horizontal bar chart.
 *
 * Sequential single hue, not categorical: the job here is "compare magnitude, low to high", and
 * a categorical palette would imply the entities are the subject and bury whichever bar actually
 * matters. Every bar is directly labelled, so identity never depends on colour.
 */
export function RankedBars({ title, subtitle, entries, caveat, unavailable }: Props) {
  const max = maxOf(entries, (e) => e.msPlayed)

  // No table view either: an empty table is not an accessible alternative to a chart that
  // cannot be drawn, it is just an empty table.
  if (unavailable) {
    return (
      <Card title={title} subtitle={subtitle}>
        <p className="empty empty--unavailable">{unavailable}</p>
      </Card>
    )
  }

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
              <td>
                <EntityName name={e.name} artistName={e.artistName} albumName={e.albumName} />
              </td>
              <td className="num"><Duration ms={e.msPlayed} /></td>
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
                title={`${e.name}${entityContext(e.artistName, e.albumName) ? ` (${entityContext(e.artistName, e.albumName)})` : ''} — ${formatDurationFull(e.msPlayed)}, ${formatNumber(e.plays)} plays`}
              >
                <span className="bar__rank">{e.rank}</span>
                <span className="bar__name">
                  <EntityName name={e.name} artistName={e.artistName} albumName={e.albumName} />
                </span>
                <span className="bar__track">
                  <span
                    className="bar__fill"
                    style={{
                      width: `${Math.max(fraction(e.msPlayed, max) * 100, 1.5)}%`,
                      background: 'var(--blue-450)',
                    }}
                  />
                </span>
                <span className="bar__value"><Duration ms={e.msPlayed} /></span>
              </div>
            ))}
          </div>
          {caveat && <p className="card__sub" style={{ marginTop: '0.75rem' }}>{caveat}</p>}
        </>
      )}
    </Card>
  )
}
