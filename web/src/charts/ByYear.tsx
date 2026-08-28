import type { PeriodValue } from '../lib/types'
import { formatDurationFull, formatNumber } from '../lib/format'
import { fraction, maxOf, sequentialColor } from '../lib/scale'
import { Duration } from '../components/Duration'
import { Card } from './Card'

/**
 * Listening per calendar year, across the whole archive.
 *
 * This is the chart the dashboard was missing. Seventeen years were stored and the page showed
 * an all-time total and a current-year total — two frozen numbers with nothing between them, so
 * the archive had no shape and no arc.
 *
 * Read DOWNWARD, one row per year, rather than as a column chart along a time axis. It used to be
 * the latter, sharing the Explorer's Trend component, and the change is not cosmetic: this card
 * sits beside "Your year in one artist", and with both laid out as a row per year the same year
 * lands on the same line in both. A reader can run their eye across "2014 — 25 days — Five Finger
 * Death Punch" as one fact. A column chart beside a list could not be read that way at all, and
 * had to be padded with empty space to match its neighbour's height.
 *
 * Newest first, for the same reason the year list is: aligning the two cards means agreeing on
 * direction, and the years a reader recognises are the ones they came to see.
 *
 * Gap years arrive as explicit zeroes rather than being omitted. A year away from Spotify is a
 * fact about the history, and closing the gap would draw a continuous line through a
 * discontinuity.
 */
export function ByYear({ years }: { years: PeriodValue[] }) {
  if (years.length === 0) return null

  const max = maxOf(years, (y) => y.msPlayed)

  const table = (
    <div className="datatable__scroll">
      <table className="datatable">
        <thead>
          <tr>
            <th scope="col">Year</th>
            <th scope="col" className="num">Listening</th>
            <th scope="col" className="num">Plays</th>
          </tr>
        </thead>
        <tbody>
          {years.map((y) => (
            <tr key={y.period}>
              <td>{y.period}</td>
              <td className="num"><Duration ms={y.msPlayed} /></td>
              <td className="num">{formatNumber(y.plays)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )

  const first = years[0]!.period
  const last = years[years.length - 1]!.period
  const rows = [...years].reverse()

  return (
    <Card
      title="Listening by year"
      subtitle={`${first} to ${last}, by listening time`}
      table={table}
    >
      <ol className="yearbars" role="img" aria-label={`Listening by year, ${first} to ${last}. See the table view for values.`}>
        {rows.map((y) => (
          <li
            key={y.period}
            className="yearbars__row"
            title={
              y.msPlayed > 0
                ? `${y.period} — ${formatDurationFull(y.msPlayed)}, ${formatNumber(y.plays)} plays`
                : `${y.period} — nothing`
            }
          >
            <span className="yearbars__year">{y.period}</span>
            <span className="yearbars__track">
              {/* Sequential fill by magnitude, the same encoding the heatmap uses, so darker
                  means more on both charts. A year with a tiny but non-zero total keeps a 2%
                  floor, so it reads as a sliver rather than as absence. */}
              <span
                className="yearbars__fill"
                style={{
                  width: `${Math.max(fraction(y.msPlayed, max) * 100, y.msPlayed > 0 ? 2 : 0)}%`,
                  background: sequentialColor(y.msPlayed, max),
                }}
              />
            </span>
            <span className="yearbars__value"><Duration ms={y.msPlayed} /></span>
          </li>
        ))}
      </ol>
      {/* The bars at both ends measure less than a year, and nothing about their length says so.
          A reader comparing the newest bar against the middle of the archive is comparing eight
          months to twelve -- worth one line, because the alternative is a chart that quietly
          understates its own edges. */}
      <p className="card__sub" style={{ marginTop: '0.75rem' }}>
        {partialNote(first, last)}
      </p>
    </Card>
  )
}

/**
 * Names the years that are not whole.
 *
 * The last is always in progress. The first usually starts mid-year, because an archive begins on
 * whatever day its owner did -- but not necessarily, so it is only named when the series is long
 * enough for a partial first year to be plausible rather than asserted.
 */
function partialNote(first: string, last: string): string {
  return `${last} is still in progress and ${first} begins partway through the year, so the bars ` +
    'at either end cover less time than the ones between them.'
}
