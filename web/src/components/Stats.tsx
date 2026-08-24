import type { Dashboard } from '../lib/types'
import { formatDuration, formatHours, formatMinutes, formatNumber, formatPercent } from '../lib/format'

/**
 * The hero figure.
 *
 * A single headline number is a hero figure, not a one-bar bar chart. Text tokens only -- a
 * coloured number would imply an encoding that is not there.
 */
export function Hero({ data }: { data: Dashboard }) {
  const { allTime } = data
  return (
    <div className="hero">
      <p className="hero__label">Total listening</p>
      <p className="hero__value">
        {formatHours(allTime.msPlayed)}
        <span className="hero__unit">hours</span>
      </p>
      <p className="hero__detail">
        {formatMinutes(allTime.msPlayed)} · {formatNumber(allTime.plays)} plays
        {allTime.estimatedRatio > 0 && (
          <>
            {' · '}
            <span title="Plays captured from the recently-played endpoint carry no duration, so they count the track's full length.">
              {estimatedShare(allTime.estimatedRatio)} estimated
            </span>
          </>
        )}
      </p>
    </div>
  )
}

interface TileProps {
  label: string
  value: string
  /** A secondary rendering of the same quantity, e.g. a duration restated in minutes. */
  sub?: string
  hint?: string
}

function Tile({ label, value, sub, hint }: TileProps) {
  return (
    <div className="tile" title={hint}>
      <p className="tile__label">{label}</p>
      <p className="tile__value">{value}</p>
      {sub && <p className="tile__sub">{sub}</p>}
    </div>
  )
}

/**
 * A row of stat tiles.
 *
 * A handful of headline numbers is a KPI row, not a grouped bar chart: they share no scale, and
 * plotting them together would invite a comparison that means nothing.
 */
export function KPIRow({ data }: { data: Dashboard }) {
  const { kpis, currentYear } = data
  return (
    <div className="tiles">
      <Tile label="Tracks" value={formatNumber(kpis.distinctTracks)} hint="Distinct tracks played" />
      <Tile label="Artists" value={formatNumber(kpis.distinctArtists)} />
      <Tile label="Albums" value={formatNumber(kpis.distinctAlbums)} />
      <Tile
        label="Current streak"
        value={`${kpis.currentStreak}d`}
        // Every other tile carries a second line, so an empty one here made the row look like
        // it followed a rule it did not. The longest streak is the natural comparison and was
        // already only a tooltip away.
        sub={`longest ${formatNumber(kpis.longestStreak)}d`}
        hint="Consecutive days with at least one play"
      />
      <Tile
        label={currentYear.period}
        value={formatDuration(currentYear.metrics.msPlayed)}
        sub={formatMinutes(currentYear.metrics.msPlayed)}
        hint={`${formatNumber(currentYear.metrics.plays)} plays this year`}
      />
    </div>
  )
}

/**
 * The share of the total that is estimated rather than measured.
 *
 * Rounding to whole points printed "0% estimated" for a ratio of 0.0004 -- a figure that reads as
 * a bug and tells the reader nothing, which is the one outcome worse than either the truth or
 * silence. Same defect as the chart caveats had, on a different code path.
 */
function estimatedShare(ratio: number): string {
  const pct = Math.round(ratio * 100)
  return pct === 0 ? '<1%' : formatPercent(ratio)
}

/**
 * Data-quality figures, deliberately NOT in the KPI row above.
 *
 * Genre coverage used to sit alongside Tracks, Artists, Albums and Current streak. Those answer
 * "how much did I listen?"; coverage answers "how much of this can you trust?" — a caveat wearing
 * the clothes of an achievement. Worse, it was the only one shown: artist coverage matters MORE,
 * because it governs whether the rankings are split across two rows per artist, and it was not
 * there at all.
 *
 * So both appear, together, in a visibly quieter block. They vanish at full coverage rather than
 * becoming permanent furniture — the same rule the per-card caveats follow.
 */
export function CoverageRow({ data }: { data: Dashboard }) {
  const { artistCoverage, genreCoverage } = data
  const items = [
    artistCoverage > 0 && artistCoverage < 0.99
      ? {
          label: 'Artist attribution',
          value: formatPercent(artistCoverage),
          hint:
            'Share of listening time attributed to a real Spotify artist. Imported history ' +
            'identifies artists by name until their tracks are resolved, and a partly-resolved ' +
            'artist occupies two rows, so rankings below the top few are provisional.',
        }
      : null,
    genreCoverage > 0 && genreCoverage < 0.99
      ? {
          label: 'Genre coverage',
          value: formatPercent(genreCoverage),
          hint:
            'Share of listening time whose artists carry at least one MusicBrainz genre. ' +
            'Which genres appear is reliable; their exact order is not.',
        }
      : null,
  ].filter((i): i is { label: string; value: string; hint: string } => i !== null)

  if (items.length === 0) return null

  return (
    <div className="coverage">
      <span className="coverage__title">Data quality</span>
      {items.map((i) => (
        <span key={i.label} className="coverage__item" title={i.hint}>
          <span className="coverage__label">{i.label}</span>
          <span className="coverage__value">{i.value}</span>
        </span>
      ))}
      <span className="coverage__note">
        Improving as imported history is resolved; these disappear at full coverage.
      </span>
    </div>
  )
}
