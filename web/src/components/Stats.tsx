import type { ReactNode } from 'react'
import type { Dashboard, Entry, PartialPeriod } from '../lib/types'
import { Artwork } from './Artwork'
import { ArtistProfileLink } from './ProfileLink'
import {
  formatDate,
  formatDuration,
  formatHours,
  formatMinutes,
  formatNumber,
  formatPercent,
} from '../lib/format'

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
        {/* Twenty-six thousand hours is a number nobody can picture. Days of continuous play is
            the same fact at a scale a person holds. */}
        {continuousDays(allTime.msPlayed)} of continuous play · {formatMinutes(allTime.msPlayed)} ·{' '}
        {formatNumber(allTime.plays)} plays
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
  /**
   * A secondary rendering of the same quantity, e.g. a duration restated in minutes.
   *
   * A node rather than a string, because the year-over-year delta carries a coloured arrow and
   * the colour has to sit on its own element to stay out of the surrounding muted ink.
   */
  sub?: ReactNode
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
 * A row of stat tiles: what the archive CONTAINS.
 *
 * A handful of headline numbers is a KPI row, not a grouped bar chart: they share no scale, and
 * plotting them together would invite a comparison that means nothing.
 *
 * Inventory only, and that is the whole rule. The current streak and this year's total used to
 * sit here too, and they answer a different question -- not "how big is the collection?" but
 * "what is happening lately?" -- so they moved into the Activity band, next to the charts that
 * put them in context. What is left reads down one axis: artists, their albums, the tracks on
 * them, the genres over the top.
 */
export function KPIRow({ data }: { data: Dashboard }) {
  const { kpis } = data
  return (
    <div className="tiles">
      <Tile label="Artists" value={formatNumber(kpis.distinctArtists)} hint="Distinct artists played" />
      <Tile label="Albums" value={formatNumber(kpis.distinctAlbums)} hint="Distinct albums played" />
      <Tile label="Tracks" value={formatNumber(kpis.distinctTracks)} hint="Distinct tracks played" />
      {/* Computed by the rollup since genres arrived and never rendered until now. Gated on a
          positive count rather than shown as zero: genres come from MusicBrainz enrichment, so
          the field is 0 before the first pass has run, and "0 genres" would be a confident lie
          about a number nobody has calculated yet. Same rule `genresAvailable` applies to the
          genre chart. */}
      {kpis.distinctGenres > 0 && (
        <Tile
          label="Genres"
          value={formatNumber(kpis.distinctGenres)}
          hint="Distinct MusicBrainz genres across all listening"
        />
      )}
    </div>
  )
}

/**
 * The two time-bound figures, in the Activity band rather than the headline row.
 *
 * Both are statements about the recent past, which is exactly what the charts beneath them show:
 * the streak is the right-hand edge of the heatmap read as a number, and this year's total is the
 * last bar of the by-year series. In the headline row they were inventory's neighbours and read
 * as two more totals; here they are the caption to the pictures.
 */
export function ActivityStats({ data }: { data: Dashboard }) {
  const { kpis, currentYear, currentMonth, currentWeek, artistOfMonth, artistOfWeek } = data
  return (
    <div className="tiles tiles--inline">
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
        // The comparison, not the minutes. A bare year total is a number without a judgement;
        // against the same calendar point last year it becomes one. The minutes are still in
        // the tooltip, and every duration elsewhere on the page carries them.
        sub={yearDelta(currentYear.metrics.msPlayed, currentYear.previousYearToDate.msPlayed)}
        hint={
          `${formatNumber(currentYear.metrics.plays)} plays this year · ` +
          `${formatMinutes(currentYear.metrics.msPlayed)} · ` +
          `to this point last year: ${formatDuration(currentYear.previousYearToDate.msPlayed)}`
        }
      />

      {/* The two shorter scales, each beside the artist who defined it. Ordered longest to
          shortest so the row reads as one zoom: this year, this month, this week.

          All four render only when the snapshot carries them. A published bundle can be newer
          than the JSON it fetches, and half a row is better than four tiles reading zero. */}
      {currentMonth && <PeriodTile label="This month" unit="month" period={currentMonth} />}
      {artistOfMonth && <ArtistTile label="Artist of the month" entry={artistOfMonth} />}
      {currentWeek && <PeriodTile label="This week" unit="week" period={currentWeek} />}
      {artistOfWeek && <ArtistTile label="Artist of the week" entry={artistOfWeek} />}
    </div>
  )
}

/**
 * A period still running, with the same delta treatment the year tile gets.
 *
 * The day count goes in the tooltip rather than on the face. "4 days in" is the context that
 * makes a partial total mean something, but it is not the figure -- and a six-tile row has no
 * width for a third line.
 */
function PeriodTile({
  label,
  unit,
  period,
}: {
  label: string
  unit: 'month' | 'week'
  period: PartialPeriod
}) {
  return (
    <Tile
      label={label}
      value={formatDuration(period.metrics.msPlayed)}
      sub={yearDelta(period.metrics.msPlayed, period.previousToDate.msPlayed, `last ${unit}`)}
      hint={
        `${formatNumber(period.metrics.plays)} plays · ` +
        `${formatMinutes(period.metrics.msPlayed)} · ` +
        `day ${period.elapsed} of this ${unit} · ` +
        `to this point last ${unit}: ${formatDuration(period.previousToDate.msPlayed)}`
      }
    />
  )
}

/**
 * The artist who owns a period: artwork, name, listening time.
 *
 * Artwork rather than a bare name, and not for decoration -- at a sixth of the row a long name
 * ellipsizes, and the thumbnail is the part of the identity that survives the truncation. The
 * name links inward to the profile, as it does on every leaderboard row.
 *
 * Not a <Tile>: the value here is a name, not a quantity, so it wants text wrapping rules rather
 * than tabular figures, and it carries a link and an image a Tile has no slot for.
 */
function ArtistTile({ label, entry }: { label: string; entry: Entry }) {
  return (
    <div
      className="tile tile--artist"
      title={
        `${entry.name} — ${formatDuration(entry.msPlayed)} · ` +
        `${formatNumber(entry.plays)} plays`
      }
    >
      <p className="tile__label">{label}</p>
      <div className="tile__artist">
        <Artwork thumbUrl={entry.thumbUrl} imageUrl={entry.imageUrl} name={entry.name} />
        <ArtistProfileLink id={entry.id} className="namecell__link">
          <span className="tile__artistname">{entry.name}</span>
        </ArtistProfileLink>
      </div>
      <p className="tile__sub">{formatDuration(entry.msPlayed)}</p>
    </div>
  )
}

/**
 * The year-over-year change, signed, arrowed and coloured.
 *
 * The colour is the LAST of the three channels, never the only one. Green against red is
 * confusable under the two common colour deficiencies whichever steps you pick, so the arrow
 * glyph and the sign in the text are what actually carry the direction; the colour makes it
 * readable at a glance for everyone else. The glyph is aria-hidden because the signed percentage
 * beside it already says the same thing, and a screen reader announcing "up arrow plus twelve
 * percent" reads the fact twice.
 */
function yearDelta(
  current: number,
  previousToDate: number,
  against = 'last year',
): ReactNode | undefined {
  const pct = yearOverYear(current, previousToDate)
  // undefined rather than an empty element: `sub` is checked for truthiness, and a component
  // returning null would still leave Tile rendering a blank second line -- the very thing that
  // made the streak tile look like it followed a rule it did not.
  if (pct === undefined) return undefined
  if (pct === 0) return `level with ${against}`
  const up = pct > 0
  return (
    <span className="delta" data-dir={up ? 'up' : 'down'}>
      <span aria-hidden="true">{up ? '↑' : '↓'}</span>
      {` ${up ? '+' : ''}${pct}% vs this point ${against}`}
    </span>
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

/**
 * The all-time total restated as days of unbroken listening.
 *
 * Rounded to whole days and prefixed with a tilde, because the point is the scale rather than the
 * precision -- "1,099.4 days" would imply an accuracy the comparison does not have.
 */
function continuousDays(ms: number): string {
  const days = Math.round(ms / 86_400_000)
  return `≈ ${formatNumber(days)} days`
}

/**
 * This year against the same calendar point last year.
 *
 * Returns undefined when there is nothing to compare with -- the first year of the archive, or
 * before a full pass has computed the previous-year figure. A "+100%" against zero would be
 * arithmetic dressed as insight. Whole points: more precision than that is false confidence.
 */
function yearOverYear(current: number, previousToDate: number): number | undefined {
  if (previousToDate <= 0) return undefined
  return Math.round(((current - previousToDate) / previousToDate) * 100)
}

/**
 * All-time extremes, styled like the coverage strip rather than like the KPI tiles.
 *
 * These are facts about single moments, not measures to compare -- giving them a tile each would
 * put "busiest day" on the same visual footing as "total listening" and invite a reading that
 * means nothing.
 */
export function RecordsRow({ data }: { data: Dashboard }) {
  const { records, coverage } = data
  if (!records || records.busiestDay.msPlayed === 0) return null

  const items = [
    {
      label: 'Busiest day',
      value: `${formatDate(records.busiestDay.date)} · ${formatDuration(records.busiestDay.msPlayed)}`,
      hint: `${formatNumber(records.busiestDay.plays)} plays that day`,
    },
    {
      label: 'Longest streak',
      value: `${formatNumber(records.longestStreak)} days`,
      hint: records.longestStreakEnd
        ? `Ended ${formatDate(records.longestStreakEnd)}`
        : 'Consecutive days with at least one play',
    },
    coverage.firstPlayedAt
      ? {
          label: 'First play',
          value: formatDate(coverage.firstPlayedAt),
          hint: 'The earliest play in the imported history',
        }
      : null,
  ].filter((i): i is { label: string; value: string; hint: string } => i !== null)

  return (
    <div className="coverage">
      <span className="coverage__title">Records</span>
      {items.map((i) => (
        <span key={i.label} className="coverage__item" title={i.hint}>
          <span className="coverage__label">{i.label}</span>
          <span className="coverage__value">{i.value}</span>
        </span>
      ))}
    </div>
  )
}
