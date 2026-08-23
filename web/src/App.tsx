import { useEffect, useState } from 'react'
import type { Dashboard } from './lib/types'
import { Calendar } from './charts/Calendar'
import { RankedBars } from './charts/RankedBars'
import { HourRhythm, WeekdayRhythm } from './charts/Rhythm'
import { Hero, KPIRow } from './components/Stats'
import { Footer } from './components/Footer'
import { ThemeToggle } from './components/ThemeToggle'
import { Explorer } from './explorer/Explorer'
import { ROUTE_PATHS, useRoute, type Route } from './lib/router'

type State =
  | { status: 'loading' }
  | { status: 'error'; message: string }
  | { status: 'ready'; data: Dashboard }

/**
 * The dashboard reads a static snapshot from its own origin rather than calling the API.
 *
 * It is identical for every visitor and changes once a night, so serving it as a file makes the
 * landing page pure CDN: no cold start, no read cost, and it keeps working if the query Lambda is
 * down.
 */
const SNAPSHOT_URL = '/data/dashboard.json'

export function App() {
  const [route, navigate] = useRoute()

  return (
    <div className="page">
      <header className="page__head">
        <div>
          <h1 className="page__title">Spotistats</h1>
          <p className="page__sub">Personal listening statistics</p>
        </div>
        <div className="page__actions">
          <Nav route={route} navigate={navigate} />
          <ThemeToggle />
        </div>
      </header>

      {/* The footer carries snapshot provenance -- coverage window, generation time, the
          estimated-duration caveats -- so it belongs to the dashboard, not the shell. The
          Explorer reads the live API and states its caveats per entity instead. */}
      {route === 'explore' ? <Explorer /> : <DashboardPage />}
    </div>
  )
}

function Nav({ route, navigate }: { route: Route; navigate: (r: Route) => void }) {
  const items: { route: Route; label: string }[] = [
    { route: 'dashboard', label: 'Dashboard' },
    { route: 'explore', label: 'Explorer' },
  ]
  return (
    <nav className="nav" aria-label="Pages">
      {items.map((i) => (
        <a
          key={i.route}
          className="nav__item"
          href={ROUTE_PATHS[i.route]}
          aria-current={route === i.route ? 'page' : undefined}
          onClick={(e) => {
            // A real href keeps the link shareable, middle-clickable and crawlable; the handler
            // just avoids a full reload for a same-origin move. Modified clicks fall through to
            // the browser so open-in-new-tab still works.
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return
            e.preventDefault()
            navigate(i.route)
          }}
        >
          {i.label}
        </a>
      ))}
    </nav>
  )
}

function DashboardPage() {
  const [state, setState] = useState<State>({ status: 'loading' })

  useEffect(() => {
    const controller = new AbortController()
    fetch(SNAPSHOT_URL, { signal: controller.signal })
      .then(async (res) => {
        if (!res.ok) {
          throw new Error(
            res.status === 404
              ? 'No snapshot published yet. Run the rollup to generate one.'
              : `Failed to load the dashboard (HTTP ${res.status}).`,
          )
        }
        return (await res.json()) as Dashboard
      })
      .then((data) => setState({ status: 'ready', data }))
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return
        setState({
          status: 'error',
          message: err instanceof Error ? err.message : 'Something went wrong.',
        })
      })
    return () => controller.abort()
  }, [])

  return (
    <>
      {state.status === 'loading' && <p className="empty">Loading…</p>}

      {state.status === 'error' && (
        <div className="card">
          <h2 className="card__title">Nothing to show</h2>
          <p className="card__sub">{state.message}</p>
          <p className="card__sub" style={{ marginTop: '0.75rem' }}>
            Locally: <code>make dev &amp;&amp; make dev-seed &amp;&amp; make rollup</code>, then{' '}
            <code>make serve</code>.
          </p>
        </div>
      )}

      {state.status === 'ready' && <Content data={state.data} />}
    </>
  )
}

function Content({ data }: { data: Dashboard }) {
  return (
    <>
      <Hero data={data} />
      <KPIRow data={data} />

      <div className="grid">
        <RankedBars title="Top artists" subtitle="All time, by listening time" entries={data.top.artists} />
        <RankedBars title="Top tracks" subtitle="All time, by listening time" entries={data.top.tracks} />
      </div>

      <div className="grid">
        <RankedBars title="Top albums" subtitle="All time, by listening time" entries={data.top.albums} />
        {/*
          A ranked bar, deliberately NOT a stacked bar or a pie. Genres are a many-to-many
          labelling: a track belongs to several at once, so the segments would not sum to the whole
          and no honest "100%" exists.

          Spotify removed the artist genres field from the Web API in February 2026, so in
          practice this card explains its own absence. Saying so beats an empty chart, which
          reads as a bug in the dashboard, and beats hiding the card, which invites "wasn't
          there a genre chart?".
        */}
        <RankedBars
          title="Top genres"
          subtitle={data.genresAvailable ? 'All time, by listening time' : undefined}
          entries={data.top.genres}
          caveat="A track can count under several genres, so these do not add up to the total."
          unavailable={
            data.genresAvailable
              ? undefined
              : 'Spotify removed artist genres from its Web API in February 2026 and offers no ' +
                'replacement, so genre breakdowns can no longer be produced.'
          }
        />
      </div>

      <Calendar days={data.calendar} timezone={data.timezone} />

      <div className="grid">
        <HourRhythm buckets={data.rhythm.hourOfDay} timezone={data.timezone} />
        <WeekdayRhythm buckets={data.rhythm.weekday} />
      </div>

      {data.topThisYear.artists.length > 0 && (
        <div className="grid">
          <RankedBars
            title={`Top artists in ${data.currentYear.period}`}
            entries={data.topThisYear.artists}
          />
          <RankedBars
            title={`Top tracks in ${data.currentYear.period}`}
            entries={data.topThisYear.tracks}
          />
        </div>
      )}

      <Footer data={data} />
    </>
  )
}
