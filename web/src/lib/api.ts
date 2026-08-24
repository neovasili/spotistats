/**
 * Typed client for the query API.
 *
 * The dashboard deliberately does NOT use this -- it reads a static snapshot, so the landing
 * page stays pure CDN. The Explorer is the opposite case: its queries are per-visitor and
 * unbounded in shape, so they have to hit the API.
 */

export interface Metrics {
  plays: number
  playsExact: number
  msPlayed: number
  msPlayedExact: number
  /** Share of msPlayed that is estimated rather than exact, 0..1. */
  estimatedRatio: number
}

export interface ListItem {
  rank: number
  id: string
  name: string
  /** Context that makes a bare title identifiable; absent for artists. */
  artistName?: string
  albumName?: string
  /** Artwork. Often absent: an entity only has it once the API has resolved it. */
  imageUrl?: string
  thumbUrl?: string
  metrics: Metrics
  firstPlayedAt?: string
  lastPlayedAt?: string
}

export interface ListResponse {
  dim: Dim
  period: string
  sort: Sort
  order: Order
  items: ListItem[]
  /** Absent when there is no further page. */
  nextCursor?: string
  total?: number
}

export interface StatsResponse {
  dim: Dim
  id: string
  name: string
  artistName?: string
  albumName?: string
  imageUrl?: string
  thumbUrl?: string
  period: string
  metrics: Metrics
  firstPlayedAt?: string
  lastPlayedAt?: string
  buckets: number
}

export interface TimelinePoint {
  period: string
  metrics: Metrics
}

export interface TimelineResponse {
  dim: Dim
  id: string
  name: string
  artistName?: string
  albumName?: string
  bucket: Bucket
  from: string
  to: string
  points: TimelinePoint[]
}

export interface Play {
  playedAt: string
  trackId: string
  name: string
  msPlayed: number
  estimated: boolean
  source: 'api' | 'export'
}

export interface PlaysResponse {
  items: Play[]
  nextCursor?: string
}

export interface ProfileMember {
  name: string
  mbid?: string
  instruments?: string[]
  begin?: string
  end?: string
  ended?: boolean
}

export interface ProfileImages {
  /** TheAudioDB's portrait; `spotify` is the fallback when there is none. */
  thumb?: string
  spotify?: string
  logo?: string
  cutout?: string
  clearart?: string
  wideThumb?: string
  banner?: string
  fanart?: string[]
}

export interface ProfileListening {
  metrics: Metrics
  firstPlayedAt?: string
  lastPlayedAt?: string
  /** Spotify's own tags. Kept apart from `mbGenres` -- see ArtistProfile. */
  spotifyGenres?: string[]
}

export interface ArtistProfile {
  id: string
  name?: string

  /** Empty when MusicBrainz has no Spotify link for this artist. */
  mbid?: string
  /** 'link' for an editor-asserted relationship, 'override' for a manual correction. */
  resolvedVia?: 'link' | 'override'

  artistType?: string
  country?: string
  areaName?: string
  beginAreaName?: string
  /** Variable precision: '2008', '2008-04' or '2008-04-17'. Render exactly what is here. */
  beganAt?: string
  beganPrecision?: 'year' | 'month' | 'day'
  endedAt?: string
  ended?: boolean

  /**
   * MusicBrainz's genres. NEVER merge these with `listening.spotifyGenres`: they are
   * different taxonomies, so one row of chips would imply an agreement that does not exist,
   * and MusicBrainz's are CC-BY-NC-SA, which makes the separate attribution a licence
   * obligation rather than a courtesy.
   */
  mbGenres?: string[]

  members?: ProfileMember[]

  biography?: string
  biographyLang?: string

  images: ProfileImages
  listening: ProfileListening
  sources: { facts?: string; prose?: string; images?: string }
  refreshedAt?: string
}

export interface MetaResponse {
  metrics: Metrics
  /** The span of stored listening history. Bounds are ISO instants, or null when empty. */
  coverage: {
    firstPlayedAt: string | null
    lastPlayedAt: string | null
    /** True when the bounds come from write-time attributes rather than a full reconcile. */
    approximate: boolean
  }
  timezone: string
}

export type Dim = 'TRACK' | 'ARTIST' | 'ALBUM' | 'GENRE' | 'TOTAL'
export type Sort = 'ms' | 'plays'
export type Order = 'desc' | 'asc'
export type Bucket = 'day' | 'month' | 'year'

const BASE = '/api/v1'

/**
 * ApiError carries the server's own error code so a caller can distinguish "you asked for
 * something impossible" from "the backend is broken" without string-matching a message.
 */
export class ApiError extends Error {
  constructor(
    readonly code: string,
    message: string,
    readonly status: number,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

async function get<T>(path: string, params: Record<string, string | number | undefined>, signal?: AbortSignal): Promise<T> {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    // Omitting empty values rather than sending them matters: the API rejects unknown and
    // malformed parameters outright instead of ignoring them.
    if (v !== undefined && v !== '') q.set(k, String(v))
  }
  const res = await fetch(`${BASE}/${path}?${q}`, { signal })
  if (!res.ok) {
    let code = `HTTP_${res.status}`
    let message = `Request failed (HTTP ${res.status}).`
    try {
      const body = (await res.json()) as { error?: { code?: string; message?: string } }
      if (body.error?.code) code = body.error.code
      if (body.error?.message) message = body.error.message
    } catch {
      // A non-JSON body means the failure came from the edge, not the API. Keep the default.
    }
    throw new ApiError(code, message, res.status)
  }
  return (await res.json()) as T
}

export function fetchList(
  args: {
    dim: Dim
    period: string
    sort: Sort
    order: Order
    limit: number
    q?: string
    cursor?: string
  },
  signal?: AbortSignal,
): Promise<ListResponse> {
  return get<ListResponse>('list', args, signal)
}

export function fetchStats(
  args: { dim: Dim; id: string; period: string },
  signal?: AbortSignal,
): Promise<StatsResponse> {
  return get<StatsResponse>('stats', args, signal)
}

export function fetchTimeline(
  args: { dim: Dim; id: string; bucket: Bucket; from: string; to: string },
  signal?: AbortSignal,
): Promise<TimelineResponse> {
  return get<TimelineResponse>('timeline', args, signal)
}

export function fetchPlays(
  args: { trackId?: string; from?: string; to?: string; limit: number; cursor?: string },
  signal?: AbortSignal,
): Promise<PlaysResponse> {
  return get<PlaysResponse>('plays', args, signal)
}

/**
 * One artist's external profile.
 *
 * A 404 here is meaningful rather than an error: it means this artist has never been through
 * enrichment, which is different from having been checked and not found. Callers distinguish
 * them by `ApiError.status`, and the page says different things in each case.
 */
export function fetchArtistProfile(id: string, signal?: AbortSignal): Promise<ArtistProfile> {
  // A path parameter, so it is encoded here rather than passed through the query builder.
  return get<ArtistProfile>(`artists/${encodeURIComponent(id)}/profile`, {}, signal)
}

/**
 * Dataset-level facts: what the history actually covers.
 *
 * The Explorer needs this to build its year list. Hardcoding a floor was wrong the moment the
 * GDPR export landed -- it read 2015 while the data went back to 2009 -- so the range is derived
 * from the data instead.
 */
export function fetchMeta(signal?: AbortSignal): Promise<MetaResponse> {
  return get<MetaResponse>('meta', {}, signal)
}
