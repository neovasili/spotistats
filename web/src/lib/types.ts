/**
 * The shape of data/dashboard.json, mirroring internal/rollup's Dashboard.
 *
 * Hand-written rather than generated: the snapshot is a wire format, and a change on either side
 * should be a deliberate edit here, not something a codegen step papers over.
 */

/**
 * Every measure carries the exact subtotal and the estimated ratio.
 *
 * `msPlayedExact` is a SUBSET of `msPlayed`, not a parallel total: plays captured from the
 * recently-played endpoint have no real duration, so they contribute the track's full length to
 * `msPlayed` and nothing to `msPlayedExact`. Rendering `msPlayed` without surfacing a non-zero
 * `estimatedRatio` presents an estimate as a measurement.
 */
export interface Metrics {
  plays: number
  playsExact: number
  msPlayed: number
  msPlayedExact: number
  estimatedRatio: number
}

export interface Entry {
  rank: number
  id: string
  name: string
  plays: number
  msPlayed: number
  /** The large asset; thumbUrl is the small one for list rows. Both often absent — artwork
   *  arrives only once the API has resolved the entity. */
  imageUrl?: string
  thumbUrl?: string
  /**
   * Context that makes a bare title identifiable. Album and track titles repeat heavily across
   * artists, so "Bleed Out" or "Mad World" alone names nothing.
   *
   * Both are optional: an artist entry has no context to add, and a track whose album row is
   * not yet enriched renders without a subtitle rather than with a blank one.
   */
  artistName?: string
  albumName?: string
}

export interface DayValue {
  date: string
  plays: number
  msPlayed: number
}

export interface BucketValue {
  bucket: number
  plays: number
  msPlayed: number
}

export interface Dashboard {
  generatedAt: string
  timezone: string
  coverage: {
    firstPlayedAt: string | null
    lastPlayedAt: string | null
    /** True when the window comes from write-time bounds rather than a full-history pass. */
    approximate: boolean
  }
  allTime: Metrics
  currentYear: {
    period: string
    metrics: Metrics
    /** Last year cut at the same month and day, so the comparison is like for like. */
    previousYearToDate: Metrics
  }
  /**
   * The current month and week so far, each against the same stretch of the period before.
   *
   * Optional, like byYear and yearArtists before them: a deployed bundle can be newer than the
   * snapshot it fetches -- the rollup writes one every two hours, the CDN serves the last one --
   * so every field added after launch has to render as absent rather than as zero.
   */
  currentMonth?: PartialPeriod
  currentWeek?: PartialPeriod
  /** Who the current month belonged to, from that month's artist aggregates. */
  artistOfMonth?: Entry
  /** Who the current week belonged to, counted from the week's plays -- see the rollup. */
  artistOfWeek?: Entry
  kpis: {
    distinctTracks: number
    distinctArtists: number
    distinctAlbums: number
    distinctGenres: number
    currentStreak: number
    longestStreak: number
  }
  top: {
    artists: Entry[]
    tracks: Entry[]
    albums: Entry[]
    genres: Entry[]
  }
  /**
   * The same four dimensions as `top`, for the current year.
   *
   * Albums and genres are optional only because they were added later: a deployed bundle can be
   * newer than the snapshot it fetches, and the band falls back to the artists/tracks pair rather
   * than claiming this year had no albums.
   */
  topThisYear: { artists: Entry[]; tracks: Entry[]; albums?: Entry[]; genres?: Entry[] }
  calendar: DayValue[]
  rhythm: { hourOfDay: BucketValue[]; weekday: BucketValue[] }
  /** Exact share of listening time whose artists carry at least one genre. 0 means unknown. */
  genreCoverage: number
  /**
   * Whether Spotify returned any genre data. Normally false: the artist `genres` field was
   * removed from the Web API in February 2026 and there is no other genre taxonomy, so the
   * genre card explains its absence instead of rendering an empty chart.
   */
  genresAvailable: boolean
  /**
   * Share of listening time carrying artist attribution, 0..1.
   *
   * Below 1 the artist and album rankings are WRONG rather than merely short: an unattributed
   * play counts towards the total and towards no artist, so each artist reads low by a
   * different amount and the ordering changes. The cards say so instead of presenting a
   * fraction of the truth as all of it.
   */
  artistCoverage: number
  /** One entry per calendar year the history spans, oldest first; gap years are explicit zeroes. */
  byYear?: PeriodValue[]
  /** The single most-played artist of each year. */
  yearArtists?: YearEntry[]
  /** All-time extremes. */
  records: Records
  notes: string[]
}

/** A period still in progress, paired with the same span of the one before it. */
export interface PartialPeriod {
  /** "2026-08" for a month; the Monday's date for a week. */
  period: string
  /** Days covered so far, including today -- what makes a partial total interpretable. */
  elapsed: number
  metrics: Metrics
  previousToDate: Metrics
}

export interface PeriodValue {
  period: string
  plays: number
  msPlayed: number
}

export interface YearEntry {
  period: string
  entry: Entry
}

export interface Records {
  busiestDay: DayValue
  longestStreak: number
  longestStreakEnd?: string
}
