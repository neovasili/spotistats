import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ApiError,
  fetchList,
  type Dim,
  type GenreMatch,
  type ListItem,
  type Metrics,
  type Order,
  type Sort,
  fetchMeta,
} from '../lib/api'
import { formatDuration, formatMinutes, formatNumber } from '../lib/format'
import { Duration } from '../components/Duration'
import { fraction, maxOf } from '../lib/scale'
import { useUrlParams } from '../lib/router'
import { useFillViewport } from '../lib/useFillViewport'
import { EntityDetail } from './EntityDetail'
import { PeriodPicker } from './PeriodPicker'
import { GenrePicker, type GenreOption } from './GenrePicker'
import { downloadCsv } from './csv'
import { EntityName } from '../components/EntityName'
import { Artwork, SpotifyLink } from '../components/Artwork'

const PAGE = 50

/** Only dimensions with browsable entities. TOTAL has none; GENRE no longer has data. */
const DIMS: { dim: Dim; label: string }[] = [
  { dim: 'TRACK', label: 'Tracks' },
  { dim: 'ARTIST', label: 'Artists' },
  { dim: 'ALBUM', label: 'Albums' },
]

type State =
  | { status: 'loading' }
  | { status: 'error'; message: string }
  | {
      status: 'ready'
      items: ListItem[]
      nextCursor?: string
      total?: number
      totals?: Metrics
      caveat?: string
      truncated?: boolean
    }

/**
 * The detail page: every entity ever played, grouped and filtered on demand.
 *
 * This is the half of the product the static snapshot cannot serve. The dashboard answers a
 * fixed set of questions and is therefore a file; the questions here are chosen by the reader
 * ("minutes of Within Temptation during 2025"), so they go to the query API.
 */
export function Explorer() {
  // Every filter lives in the URL, so any view is a shareable link (docs/SPECS.md 7.2).
  const [params, setParams] = useUrlParams()
  const dim = readDim(params.get('dim'))
  const period = params.get('period') || 'ALL'
  const sort: Sort = params.get('sort') === 'plays' ? 'plays' : 'ms'
  const order: Order = params.get('order') === 'asc' ? 'asc' : 'desc'
  const urlQuery = params.get('q') ?? ''
  // Comma-joined in the URL, which keeps a shared link readable
  // (?genres=power+metal,symphonic+metal). Verified against the whole vocabulary: no genre
  // contains a comma, so the simpler encoding is unambiguous here.
  const genresParam = params.get('genres') ?? ''
  const genres = useMemo(
    () => genresParam.split(',').map((g) => g.trim()).filter(Boolean),
    [genresParam],
  )
  const genreMatch: GenreMatch = params.get('genreMatch') === 'all' ? 'all' : 'any'

  const setDim = (d: Dim) => setParams({ dim: d, id: undefined })
  const setPeriod = (pd: string) => setParams({ period: pd === 'ALL' ? undefined : pd })
  const setGenres = (next: string[]) =>
    setParams({
      genres: next.length > 0 ? next.join(',') : undefined,
      // The rule is meaningless with fewer than two genres, so it is dropped from the URL
      // rather than left behind to reappear on the next selection.
      genreMatch: next.length > 1 && genreMatch === 'all' ? 'all' : undefined,
    })
  const setGenreMatch = (m: GenreMatch) => setParams({ genreMatch: m === 'all' ? 'all' : undefined })

  // The search box is local so typing stays responsive, and is mirrored into the URL once the
  // debounce settles -- writing on every keystroke would rewrite the address bar per character.
  const [query, setQuery] = useState(urlQuery)
  const [selected, setSelected] = useState<ListItem | null>(null)
  // The earliest year with data, so the year picker cannot hide history the way a hardcoded
  // floor of 2015 hid everything before it. Fetched once: /meta describes the dataset, not the
  // query, so it does not change as filters do.
  const [firstYear, setFirstYear] = useState<number | undefined>()

  useEffect(() => {
    const controller = new AbortController()
    fetchMeta(controller.signal)
      .then((m) => {
        const first = m.coverage.firstPlayedAt
        if (first) setFirstYear(new Date(first).getUTCFullYear())
      })
      // A failure here is not worth surfacing: the picker falls back to its own floor, and the
      // results list will report any real API problem far more clearly.
      .catch(() => {})
    return () => controller.abort()
  }, [])
  // The genre vocabulary: the tags this library actually uses, ordered by listening time. Like
  // /meta it describes the dataset rather than the query, so it is fetched once.
  //
  // Read from /list rather than /top, which would have been the obvious choice and is wrong:
  // /top prefers the leaderboard the nightly rollup materialises, and that is capped at 100
  // entries. There are 350 genres, so 250 of them -- everything past rank 100 -- would have
  // been unfilterable, silently. /list reads the whole GENRE partition instead: 350 rows, one
  // query, complete.
  const [genreOptions, setGenreOptions] = useState<GenreOption[] | undefined>()
  useEffect(() => {
    const controller = new AbortController()
    fetchList(
      { dim: 'GENRE', period: 'ALL', sort: 'ms', order: 'desc', limit: 500 },
      controller.signal,
    )
      .then((res) =>
        setGenreOptions(res.items.map((i) => ({ name: i.name, msPlayed: i.metrics.msPlayed }))),
      )
      // A failure leaves the picker saying it has nothing to offer, which is the truth. The
      // results list reports any real API problem far more clearly.
      .catch(() => setGenreOptions([]))
    return () => controller.abort()
  }, [])

  const [state, setState] = useState<State>({ status: 'loading' })

  // Debounced, so typing a name is one request per pause rather than one per keystroke.
  const [debouncedQuery, setDebouncedQuery] = useState(urlQuery)
  useEffect(() => {
    const t = setTimeout(() => setDebouncedQuery(query.trim()), 250)
    return () => clearTimeout(t)
  }, [query])
  useEffect(() => {
    setParams({ q: debouncedQuery || undefined })
  }, [debouncedQuery, setParams])

  // Identifies the current filter set. Every fetch carries the generation it belongs to, so a
  // slow first page cannot land after a newer filter has already replaced the list.
  const generation = useMemo(
    () => `${dim}|${period}|${sort}|${order}|${debouncedQuery}|${genresParam}|${genreMatch}`,
    [dim, period, sort, order, debouncedQuery, genresParam, genreMatch],
  )
  const latest = useRef(generation)
  latest.current = generation

  useEffect(() => {
    const controller = new AbortController()
    const mine = generation
    setState({ status: 'loading' })
    fetchList(
      {
        dim, period, sort, order, limit: PAGE, q: debouncedQuery,
        genres: genres.join(','),
        // Only sent when it can mean something, so the URL and the request stay minimal.
        genreMatch: genres.length > 1 ? genreMatch : undefined,
      },
      controller.signal,
    )
      .then((res) => {
        if (latest.current !== mine) return
        setState({
          status: 'ready',
          items: res.items,
          nextCursor: res.nextCursor,
          total: res.total,
          totals: res.totals,
          caveat: res.caveat,
          truncated: res.truncated,
        })
      })
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return
        if (latest.current !== mine) return
        setState({ status: 'error', message: describe(err) })
      })
    return () => controller.abort()
    // eslint-disable-next-line react-hooks/exhaustive-deps -- generation covers the filter set
  }, [dim, period, sort, order, debouncedQuery, generation])

  // Selecting an entity from a previous filter set would show a detail panel unrelated to the
  // visible list, so the selection is cleared whenever the query changes.
  useEffect(() => setSelected(null), [generation])

  // Restore a shared link's selection once the matching row arrives.
  const selectedId = params.get('id') ?? undefined
  useEffect(() => {
    if (!selectedId || state.status !== 'ready') return
    const hit = state.items.find((i) => i.id === selectedId)
    if (hit) setSelected(hit)
  }, [selectedId, state])

  const select = (i: ListItem) => {
    setSelected(i)
    setParams({ id: i.id })
  }

  // Clearing the URL param as well as the state: the selection is part of the shareable query,
  // so a closed panel must not come back when the link is reopened.
  const clearSelection = useCallback(() => {
    setSelected(null)
    setParams({ id: undefined })
  }, [setParams])

  // Escape closes it, matching the tooltips. A panel that can only be dismissed by hitting a
  // small target is a panel people leave open.
  useEffect(() => {
    if (!selected) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') clearSelection()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [selected, clearSelection])

  const loadMore = useCallback(() => {
    if (state.status !== 'ready' || !state.nextCursor) return
    const mine = latest.current
    const cursor = state.nextCursor
    fetchList({
      dim, period, sort, order, limit: PAGE, q: debouncedQuery, cursor,
      genres: genres.join(','),
      genreMatch: genres.length > 1 ? genreMatch : undefined,
    })
      .then((res) => {
        if (latest.current !== mine) return
        setState((prev) =>
          prev.status === 'ready'
            ? { ...prev, items: [...prev.items, ...res.items], nextCursor: res.nextCursor }
            : prev,
        )
      })
      .catch((err: unknown) => {
        if (latest.current !== mine) return
        setState({ status: 'error', message: describe(err) })
      })
  }, [state, dim, period, sort, order, debouncedQuery, genres, genreMatch])

  const toggleSort = (next: Sort) => {
    if (next === sort) {
      setParams({ order: order === 'desc' ? 'asc' : 'desc' })
      return
    }
    // A new column starts at "most first", which is what a ranking is for.
    setParams({ sort: next, order: 'desc' })
  }

  const exportCsv = () => {
    if (state.status !== 'ready') return
    downloadCsv(`spotistats-${dim.toLowerCase()}-${period.toLowerCase()}.csv`, state.items)
  }

  return (
    <>
      <section className="card">
        <div className="card__head">
          <div>
            <h2 className="card__title">Explore</h2>
            <p className="card__sub">
              {describeQuery(dim, period, sort, order, debouncedQuery, genres, genreMatch)}
            </p>
          </div>
          {state.status === 'ready' && state.items.length > 0 && (
            <button type="button" className="ghost-button" onClick={exportCsv}>
              Export CSV
            </button>
          )}
        </div>

        {/* Filters in one row above the data, so the whole query is visible at a glance. */}
        <div className="filters">
          <div className="segmented" role="group" aria-label="Group by">
            {DIMS.map((d) => (
              <button
                key={d.dim}
                type="button"
                className="segmented__item"
                aria-pressed={dim === d.dim}
                onClick={() => setDim(d.dim)}
              >
                {d.label}
              </button>
            ))}
          </div>

          <PeriodPicker value={period} onChange={setPeriod} firstYear={firstYear} />

          <GenrePicker
            options={genreOptions ?? []}
            selected={genres}
            onChange={setGenres}
            match={genreMatch}
            onMatchChange={setGenreMatch}
            loading={genreOptions === undefined}
          />

          <label className="field">
            <span className="field__label">Search</span>
            <input
              type="search"
              className="field__input"
              value={query}
              placeholder="name contains…"
              onChange={(e) => setQuery(e.target.value)}
            />
          </label>
        </div>
      </section>

      {state.status === 'loading' && <p className="empty">Loading…</p>}

      {state.status === 'error' && (
        <div className="card">
          <h2 className="card__title">That query did not work</h2>
          <p className="card__sub">{state.message}</p>
        </div>
      )}

      {/*
        The drill-down sits between the filters and the list, not after it.

        The list runs to the bottom of the viewport by design, so a panel below it opened
        entirely off-screen and selecting a row looked like nothing had happened -- which is why
        it used to need a scrollIntoView on every selection. Placing it here means the panel
        appears where the reader is already looking, and the list stays reachable directly
        beneath it.
      */}
      {selected && (
        <div className="detailslot">
          <EntityDetail
            dim={dim}
            item={selected}
            period={period}
            onClose={clearSelection}
          />
        </div>
      )}

      {state.status === 'ready' && (
        <>
          <ResultsTotal
            dim={dim}
            count={state.total ?? state.items.length}
            totals={state.totals}
            truncated={state.truncated}
            caveat={state.caveat}
          />
          <ResultTable
            dim={dim}
            items={state.items}
            sort={sort}
            order={order}
            onSort={toggleSort}
            selectedId={selected?.id}
            onSelect={select}
            onLoadMore={state.nextCursor ? loadMore : undefined}
          />
        </>
      )}
    </>
  )
}

function readDim(raw: string | null): Dim {
  return DIMS.some((d) => d.dim === raw) ? (raw as Dim) : 'TRACK'
}

/**
 * The query echoed in prose, as specified. A filter row shows the controls; this shows what
 * they currently MEAN, which is what makes a shared link legible to whoever opens it.
 */
function describeQuery(
  dim: Dim,
  period: string,
  sort: Sort,
  order: Order,
  q: string,
  genres: string[],
  match: GenreMatch,
): string {
  const noun = DIMS.find((d) => d.dim === dim)?.label.toLowerCase() ?? 'entities'
  const when = period === 'ALL' ? 'all time' : period
  const by = sort === 'plays' ? 'plays' : 'listening time'
  const dir = order === 'desc' ? 'most first' : 'least first'
  const matching = q ? ` matching “${q}”` : ''
  // Named rather than counted: this line is what makes a shared link legible to whoever opens
  // it, and "3 genres" tells them nothing.
  const tagged =
    genres.length === 0
      ? ''
      : ` · ${genres.length > 1 ? (match === 'all' ? 'all of ' : 'any of ') : ''}${genres.join(', ')}`
  return `${noun}${matching}${tagged} · ${when} · by ${by}, ${dir}`
}

/**
 * What the whole result set adds up to, above the rows it summarises.
 *
 * The figure a reader wants after applying a filter is not the first row, it is the sum: "how
 * much power metal, in total?". The list can only ever show 50 rows at a time, so without this
 * the question is unanswerable from the page — and a client-side sum would answer it for the
 * rows loaded so far and then change as they pressed "Load more", which is worse than silence.
 *
 * The caveat rides along because it is the same sentence's small print: an artist MusicBrainz
 * could not match is absent from a genre-filtered total entirely, and 16% of listening time is
 * in that state.
 */
function ResultsTotal({
  dim,
  count,
  totals,
  truncated,
  caveat,
}: {
  dim: Dim
  count: number
  totals?: Metrics
  truncated?: boolean
  caveat?: string
}) {
  if (!totals) return null
  const noun = DIMS.find((d) => d.dim === dim)?.label.toLowerCase() ?? 'entities'
  return (
    <div className="resultstotal">
      <p className="resultstotal__line">
        <span className="resultstotal__count">
          {truncated && 'at least '}
          {formatNumber(count)} {count === 1 ? noun.replace(/s$/, '') : noun}
        </span>
        <span className="resultstotal__sep" aria-hidden="true">
          ·
        </span>
        {/* Both renderings, as everywhere: one is how long it feels, the other is what can be
            compared, summed or checked against another tool. */}
        <span className="resultstotal__value">{formatDuration(totals.msPlayed)}</span>
        <span className="resultstotal__minutes">{formatMinutes(totals.msPlayed)}</span>
        <span className="resultstotal__sep" aria-hidden="true">
          ·
        </span>
        <span className="resultstotal__plays">{formatNumber(totals.plays)} plays</span>
      </p>
      {caveat && <p className="resultstotal__caveat">{caveat}</p>}
    </div>
  )
}

function describe(err: unknown): string {
  if (err instanceof ApiError) return err.message
  return err instanceof Error ? err.message : 'Something went wrong.'
}

/** Maps a browsable dimension onto the Spotify entity kind its rows link to. */
const LINK_KIND: Partial<Record<Dim, 'artist' | 'album' | 'track'>> = {
  TRACK: 'track',
  ARTIST: 'artist',
  ALBUM: 'album',
}

interface TableProps {
  dim: Dim
  items: ListItem[]
  sort: Sort
  order: Order
  onSort: (s: Sort) => void
  selectedId?: string
  onSelect: (i: ListItem) => void
  /** Renders a Load more control at the end of the rows when set. */
  onLoadMore?: () => void
}

function ResultTable({ dim, items, sort, order, onSort, selectedId, onSelect, onLoadMore }: TableProps) {
  const kind = LINK_KIND[dim]
  const max = maxOf(items, (i) => (sort === 'plays' ? i.metrics.plays : i.metrics.msPlayed))

  // The list runs to the bottom of the view and scrolls only if the rows genuinely exceed it --
  // including while the drill-down panel is open above it.
  //
  // That works only because the panel has a FIXED height (.detailslot). Filling to the viewport
  // bottom from a variable offset was the earlier problem: the panel's height depended on its
  // contents and on the chart/table toggle, so opening it could collapse the list to almost
  // nothing. With a known panel height the arithmetic closes -- filters + panel + list is
  // exactly the viewport -- and there is one scroll region, inside the list, rather than a page
  // scroll fighting a nested one.
  //
  // selectedId is passed as a re-measure trigger, not used in the measurement: the total page
  // height does not change when the panel opens, so no resize event fires on its own.
  const [scrollRef, maxHeight] = useFillViewport<HTMLDivElement>(24, [selectedId])

  if (items.length === 0) {
    return (
      <div className="card">
        <p className="empty">Nothing matches that query.</p>
      </div>
    )
  }

  // aria-sort tells a screen reader which column is ordered and which way, which the visual
  // arrow alone does not convey.
  const ariaSort = (col: Sort): 'descending' | 'ascending' | 'none' => {
    if (sort !== col) return 'none'
    return order === 'desc' ? 'descending' : 'ascending'
  }

  return (
    <div className="card">
      <div
        className="datatable__scroll results__scroll"
        ref={scrollRef}
        style={maxHeight === undefined ? undefined : { maxHeight }}
      >
        <table className="datatable datatable--interactive">
          <thead>
            <tr>
              <th scope="col" className="num">#</th>
              <th scope="col">Name</th>
              <th scope="col" className="num" aria-sort={ariaSort('ms')}>
                <button type="button" className="sortbutton" onClick={() => onSort('ms')}>
                  Listening{sort === 'ms' && (order === 'desc' ? ' ↓' : ' ↑')}
                </button>
              </th>
              <th scope="col" className="num" aria-sort={ariaSort('plays')}>
                <button type="button" className="sortbutton" onClick={() => onSort('plays')}>
                  Plays{sort === 'plays' && (order === 'desc' ? ' ↓' : ' ↑')}
                </button>
              </th>
              <th scope="col">Last played</th>
            </tr>
          </thead>
          <tbody>
            {items.map((i) => {
              const value = sort === 'plays' ? i.metrics.plays : i.metrics.msPlayed
              return (
                <tr
                  key={i.id}
                  aria-selected={i.id === selectedId}
                  className={i.id === selectedId ? 'is-selected' : undefined}
                >
                  <td className="num">{i.rank}</td>
                  <td>
                    <span className="namecell">
                      {kind && (
                        <SpotifyLink kind={kind} id={i.id}>
                          <Artwork thumbUrl={i.thumbUrl} imageUrl={i.imageUrl} name={i.name} />
                        </SpotifyLink>
                      )}
                      {/* The name opens the drill-down; only the artwork leaves for Spotify,
                          so a click on the row does not navigate away unexpectedly. */}
                      <button type="button" className="linkbutton" onClick={() => onSelect(i)}>
                        <EntityName name={i.name} artistName={i.artistName} albumName={i.albumName} />
                      </button>
                    </span>
                    {/* An in-cell magnitude bar: the ranking is the point of this table, and a
                        column of numbers alone makes the shape of the distribution invisible. */}
                    <span
                      className="inlinebar"
                      style={{ width: `${Math.max(fraction(value, max) * 100, 1)}%` }}
                      aria-hidden="true"
                    />
                  </td>
                  <td className="num"><Duration ms={i.metrics.msPlayed} /></td>
                  <td className="num">{formatNumber(i.metrics.plays)}</td>
                  <td className="dim">{i.lastPlayedAt ? i.lastPlayedAt.slice(0, 10) : '—'}</td>
                </tr>
              )
            })}
          </tbody>
        </table>

        {/*
          Load more lives INSIDE the scroll region, after the rows.
          
          It used to sit below the list, which broke the layout contract two ways: the list sizes
          itself to the viewport bottom, so anything after it overflowed the screen and brought
          back a page scroll, and reserving space for it meant guessing its height -- the guess
          was 52px against an actual 89. At the end of the rows there is nothing to reserve and
          nothing to guess, and "load more rows" is where a reader looks for it anyway.
        */}
        {onLoadMore && (
          <div className="loadmore">
            <button type="button" className="ghost-button" onClick={onLoadMore}>
              Load {PAGE} more
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
