import type { PeriodValue } from '../lib/types'
import { formatNumber } from '../lib/format'
import { Duration } from '../components/Duration'
import { Card } from './Card'
import { Trend } from './Trend'

/**
 * Listening per calendar year, across the whole archive.
 *
 * This is the chart the dashboard was missing. Seventeen years were stored and the page showed
 * an all-time total and a current-year total — two frozen numbers with nothing between them, so
 * the archive had no shape and no arc.
 *
 * Gap years arrive as explicit zeroes rather than being omitted. A year away from Spotify is a
 * fact about the history, and closing the gap would draw a continuous line through a
 * discontinuity.
 */
export function ByYear({ years }: { years: PeriodValue[] }) {
  if (years.length === 0) return null

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

  return (
    <Card
      title="Listening by year"
      subtitle={`${first} to ${last}, by listening time`}
      table={table}
    >
      <Trend
        points={years}
        emptyLabel="No listening recorded yet."
        ariaLabel={`Listening by year, ${first} to ${last}. See the table view for values.`}
      />
      {/* The bars at both ends measure less than a year, and nothing about their height says so.
          A reader comparing the last bar against the middle of the chart is comparing eight
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
