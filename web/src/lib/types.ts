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
  imageUrl?: string
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
  currentYear: { period: string; metrics: Metrics }
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
  topThisYear: { artists: Entry[]; tracks: Entry[] }
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
  notes: string[]
}
