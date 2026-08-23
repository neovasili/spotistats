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
              {formatPercent(allTime.estimatedRatio)} estimated
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
  const { kpis, currentYear, genreCoverage } = data
  return (
    <div className="tiles">
      <Tile label="Tracks" value={formatNumber(kpis.distinctTracks)} hint="Distinct tracks played" />
      <Tile label="Artists" value={formatNumber(kpis.distinctArtists)} />
      <Tile label="Albums" value={formatNumber(kpis.distinctAlbums)} />
      <Tile
        label="Current streak"
        value={`${kpis.currentStreak}d`}
        hint={`Longest streak: ${kpis.longestStreak} days`}
      />
      <Tile
        label={currentYear.period}
        value={formatDuration(currentYear.metrics.msPlayed)}
        sub={formatMinutes(currentYear.metrics.msPlayed)}
        hint={`${formatNumber(currentYear.metrics.plays)} plays this year`}
      />
      {genreCoverage > 0 && (
        <Tile
          label="Genre coverage"
          value={formatPercent(genreCoverage)}
          hint="Share of listening whose artists carry at least one genre. Spotify tags many artists with none."
        />
      )}
    </div>
  )
}
