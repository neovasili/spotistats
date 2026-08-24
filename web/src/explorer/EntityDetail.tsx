import { useEffect, useMemo, useRef, useState } from 'react'
import {
  ApiError,
  fetchPlays,
  fetchTimeline,
  type Dim,
  type ListItem,
  type Play,
  type TimelineResponse,
} from '../lib/api'
import { estimatedCaveat, formatDurationFull, formatNumber } from '../lib/format'
import { Duration } from '../components/Duration'
import { fraction, maxOf } from '../lib/scale'
import { Card } from '../charts/Card'
import { entityContext } from '../components/EntityName'
import { Artwork, SpotifyLink } from '../components/Artwork'
import { ArtistProfileLink } from '../components/ProfileLink'

interface Props {
  dim: Dim
  item: ListItem
  period: string
  /** Dismisses the panel and returns the results list to full height. */
  onClose: () => void
}

/**
 * The drill-down for one entity: its totals for the selected period, and how it moved month by
 * month across the year.
 *
 * The list row already carries the totals, so those are rendered from it immediately and the
 * only request made here is the timeline. That keeps selecting a row feeling instant, and means
 * a timeline failure degrades to "no trend" rather than blanking the whole panel.
 */
export function EntityDetail({ dim, item, period, onClose }: Props) {
  const year = period === 'ALL' ? undefined : period.slice(0, 4)
  const panel = useRef<HTMLDivElement | null>(null)

  // The panel sits between the filters and the list, so selecting a row from far down a long
  // list scrolls it into view UPWARDS. `block: 'nearest'` is what keeps that minimal: a panel
  // already on screen does not move, which matters when clicking successive rows to compare
  // them. Smooth unless the reader has asked for reduced motion.
  useEffect(() => {
    const reduced = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
    panel.current?.scrollIntoView({
      behavior: reduced ? 'auto' : 'smooth',
      block: 'nearest',
    })
  }, [item.id])
  const [timeline, setTimeline] = useState<TimelineResponse | null>(null)
  const [error, setError] = useState<string | null>(null)

  // The trend to draw, which depends on the selected period.
  //
  // A year selection gets months. An ALL-TIME selection used to get nothing and a "pick a year"
  // nag -- a large empty band in a fixed-height panel, asking the reader to narrow a query they
  // deliberately widened. All time has an obvious trend of its own: one bar per year. The range
  // comes from the entity's own first and last play, so it is the entity's real span rather than
  // a guessed window.
  const span = useMemo(() => {
    if (year) return { bucket: 'month' as const, from: `${year}-01`, to: `${year}-12` }
    const first = item.firstPlayedAt?.slice(0, 4)
    const last = item.lastPlayedAt?.slice(0, 4)
    if (!first || !last) return null
    return { bucket: 'year' as const, from: first, to: last }
  }, [year, item.firstPlayedAt, item.lastPlayedAt])

  useEffect(() => {
    if (!span) {
      setTimeline(null)
      setError(null)
      return
    }
    const controller = new AbortController()
    setTimeline(null)
    setError(null)
    fetchTimeline({ dim, id: item.id, ...span }, controller.signal)
      .then(setTimeline)
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return
        setError(err instanceof ApiError ? err.message : 'Could not load the trend.')
      })
    return () => controller.abort()
  }, [dim, item.id, span])

  const m = item.metrics
  const table = (
    <div className="datatable__scroll">
      <table className="datatable">
        <tbody>
          {item.artistName && (
            <tr><th scope="row">Artist</th><td>{item.artistName}</td></tr>
          )}
          {item.albumName && (
            <tr><th scope="row">Album</th><td>{item.albumName}</td></tr>
          )}
          <tr><th scope="row">Listening</th><td className="num"><Duration ms={m.msPlayed} /></td></tr>
          <tr><th scope="row">Plays</th><td className="num">{formatNumber(m.plays)}</td></tr>
          <tr><th scope="row">First played</th><td className="num">{item.firstPlayedAt?.slice(0, 10) ?? '—'}</td></tr>
          <tr><th scope="row">Last played</th><td className="num">{item.lastPlayedAt?.slice(0, 10) ?? '—'}</td></tr>
        </tbody>
      </table>
    </div>
  )

  return (
    <Card
      title={item.name}
      // The context belongs in the subtitle here rather than under the title: the panel header
      // is already the entity's name at heading size, and repeating it as a two-line block
      // would fight the figures beneath it.
      subtitle={[entityContext(item.artistName, item.albumName), period === 'ALL' ? 'All time' : period]
        .filter(Boolean)
        .join(' · ')}
      table={table}
      onClose={onClose}
    >
      <div className="detail" ref={panel}>
        <div className="detail__head">
          <SpotifyLink kind={spotifyKind(dim)} id={item.id}>
            <Artwork
              thumbUrl={item.imageUrl ?? item.thumbUrl}
              imageUrl={item.imageUrl}
              name={item.name}
              size="lg"
            />
          </SpotifyLink>
          <dl className="detail__figures">
            <div>
              <dt>Listening</dt>
              <dd className="detail__big"><Duration ms={m.msPlayed} /></dd>
            </div>
            <div>
              <dt>Plays</dt>
              <dd className="detail__big">{formatNumber(m.plays)}</dd>
            </div>
          </dl>
        </div>

        {estimatedCaveat(m.estimatedRatio) && (
          <p className="detail__caveat">{estimatedCaveat(m.estimatedRatio)}</p>
        )}

        {/* Artists are the only dimension with external enrichment behind them, so they are
            the only one with a profile page to reach. */}
        {dim === 'ARTIST' && (
          <p className="detail__more">
            <ArtistProfileLink id={item.id}>View full profile →</ArtistProfileLink>
          </p>
        )}

        {!span && (
          <p className="empty">No play dates recorded, so there is no trend to draw.</p>
        )}
        {error && <p className="empty">{error}</p>}
        {timeline && <Trend timeline={timeline} />}

        {/* Individual plays exist only for tracks: /plays is keyed by trackId, and there is no
            equivalent for an artist or an album without fanning out over all their tracks. */}
        {dim === 'TRACK' && <PlayLog trackId={item.id} />}
      </div>
    </Card>
  )
}

const PLAY_PAGE = 20

/** Every individual play of one track, newest first. */
function PlayLog({ trackId }: { trackId: string }) {
  const [plays, setPlays] = useState<Play[] | null>(null)
  const [cursor, setCursor] = useState<string | undefined>()
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    setPlays(null)
    setError(null)
    fetchPlays({ trackId, limit: PLAY_PAGE }, controller.signal)
      .then((res) => {
        setPlays(res.items)
        setCursor(res.nextCursor)
      })
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return
        setError(err instanceof ApiError ? err.message : 'Could not load the play history.')
      })
    return () => controller.abort()
  }, [trackId])

  const more = () => {
    if (!cursor) return
    fetchPlays({ trackId, limit: PLAY_PAGE, cursor })
      .then((res) => {
        setPlays((prev) => [...(prev ?? []), ...res.items])
        setCursor(res.nextCursor)
      })
      .catch(() => setError('Could not load more plays.'))
  }

  if (error) return <p className="empty">{error}</p>
  if (!plays) return <p className="empty">Loading plays…</p>
  if (plays.length === 0) return <p className="empty">No individual plays recorded.</p>

  return (
    <div className="playlog">
      <h3 className="playlog__title">Every play</h3>
      <ol className="playlog__list">
        {plays.map((p) => (
          <li key={`${p.playedAt}-${p.trackId}`} className="playlog__row">
            <time dateTime={p.playedAt}>{formatPlayedAt(p.playedAt)}</time>
            <span className="num"><Duration ms={p.msPlayed} /></span>
            {/* An estimated duration is the track's full length, so a skip looks like a full
                listen. Marking the row is the only honest way to show that. */}
            {p.estimated && <span className="playlog__tag">estimated</span>}
          </li>
        ))}
      </ol>
      {cursor && (
        <button type="button" className="ghost-button" onClick={more}>
          Load more plays
        </button>
      )}
    </div>
  )
}

/** Renders an instant in the reader's own locale; the API returns UTC. */
function formatPlayedAt(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime())
    ? iso
    : d.toLocaleString(undefined, {
        year: 'numeric', month: 'short', day: 'numeric',
        hour: '2-digit', minute: '2-digit',
      })
}

function Trend({ timeline }: { timeline: TimelineResponse }) {
  const max = maxOf(timeline.points, (p) => p.metrics.msPlayed)
  const byYear = timeline.bucket === 'year'
  if (max === 0) {
    return <p className="empty">No listening in {timeline.from.slice(0, 4)}.</p>
  }
  const label = byYear
    ? `Listening by year, ${timeline.from} to ${timeline.to}`
    : `Monthly listening in ${timeline.from.slice(0, 4)}`
  return (
    <div className="trend" role="img" aria-label={label}>
      {timeline.points.map((p) => (
        <div key={p.period} className="trend__col" title={`${p.period}: ${formatDurationFull(p.metrics.msPlayed)}`}>
          <div className="trend__track">
            <div
              className="trend__fill"
              style={{ height: `${fraction(p.metrics.msPlayed, max) * 100}%` }}
            />
          </div>
          {/* A year label is the whole period; a month label drops the redundant year, which
              the heading already carries. */}
          <span className="trend__label">{byYear ? p.period : p.period.slice(5)}</span>
        </div>
      ))}
    </div>
  )
}

/** The Spotify entity kind for a browsable dimension. */
function spotifyKind(dim: Dim): 'artist' | 'album' | 'track' {
  return dim === 'ARTIST' ? 'artist' : dim === 'ALBUM' ? 'album' : 'track'
}
