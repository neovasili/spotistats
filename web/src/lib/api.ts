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
