import type { YearEntry } from '../lib/types'
import { formatNumber } from '../lib/format'
import { Duration } from '../components/Duration'
import { Artwork, SpotifyLink } from '../components/Artwork'
import { ArtistProfileLink } from '../components/ProfileLink'
import { Card } from './Card'

/**
 * The artist who defined each year.
 *
 * Chronological, never ranked. Sorting these by listening time would answer a question the
 * all-time leaderboard already answers, and destroy the only thing this card is for: reading
 * down a life in music, one year at a time.
 *
 * Not materialised in any leaderboard — those cover all time, this year, last year and three
 * recent months — so the nightly rollup reads each year's own artist partition to build it.
 */
export function YearArtists({ years, caveat }: { years: YearEntry[]; caveat?: string }) {
  if (years.length === 0) return null

  const table = (
    <div className="datatable__scroll">
      <table className="datatable">
        <thead>
          <tr>
            <th scope="col">Year</th>
            <th scope="col">Artist</th>
            <th scope="col" className="num">Listening</th>
            <th scope="col" className="num">Plays</th>
          </tr>
        </thead>
        <tbody>
          {years.map((y) => (
            <tr key={y.period}>
              <td>{y.period}</td>
              <td>{y.entry.name}</td>
              <td className="num"><Duration ms={y.entry.msPlayed} /></td>
              <td className="num">{formatNumber(y.entry.plays)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )

  // Newest first: the years a reader recognises are the ones they scrolled to see.
  const rows = [...years].reverse()

  return (
    <Card title="Your year in one artist" subtitle="The most-played artist of each year" table={table}>
      <ol className="yearlist">
        {rows.map((y) => (
          <li key={y.period} className="yearlist__row">
            <span className="yearlist__year">{y.period}</span>
            <span className="namecell">
              {/* Artwork links out to Spotify, as the Developer Policy requires; the name links
                  inward to the profile, as it does on every leaderboard. */}
              <SpotifyLink kind="artist" id={y.entry.id}>
                <Artwork thumbUrl={y.entry.thumbUrl} imageUrl={y.entry.imageUrl} name={y.entry.name} />
              </SpotifyLink>
              <ArtistProfileLink id={y.entry.id} className="namecell__link">
                <span className="yearlist__name">{y.entry.name}</span>
              </ArtistProfileLink>
            </span>
            <span className="yearlist__value"><Duration ms={y.entry.msPlayed} /></span>
          </li>
        ))}
      </ol>
      {caveat && <p className="card__sub" style={{ marginTop: '0.75rem' }}>{caveat}</p>}
    </Card>
  )
}
