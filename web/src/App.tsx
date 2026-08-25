import { useEffect, useState, type ReactNode } from 'react'
import type { Dashboard } from './lib/types'
import { Calendar } from './charts/Calendar'
import { RankedBars } from './charts/RankedBars'
import { HourRhythm, WeekdayRhythm } from './charts/Rhythm'
import { CoverageRow, Hero, KPIRow, RecordsRow } from './components/Stats'
import { ByYear } from './charts/ByYear'
import { YearArtists } from './charts/YearArtists'
import { Footer } from './components/Footer'
import { ThemeToggle } from './components/ThemeToggle'
import { Explorer } from './explorer/Explorer'
import { ROUTE_PATHS, navigateTo, useRoute, type NavRoute, type Route } from './lib/router'
import { ArtistProfilePage } from './artist/ArtistProfile'

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
  const route = useRoute()

  return (
    <div className="page">
      <header className="page__head">
        <div>
          <h1 className="page__title">Spotistats</h1>
          <p className="page__sub">Personal listening statistics</p>
        </div>
        <div className="page__actions">
          <Nav route={route} />
          <ThemeToggle />
        </div>
      </header>

      {/* The footer carries snapshot provenance -- coverage window, generation time, the
          estimated-duration caveats -- so it belongs to the dashboard, not the shell. The
          Explorer reads the live API and states its caveats per entity instead. */}
      {route.name === 'explore' && <Explorer />}
      {route.name === 'artist' && <ArtistProfilePage id={route.id} />}
      {route.name === 'dashboard' && <DashboardPage />}
    </div>
  )
}

function Nav({ route }: { route: Route }) {
  const items: { route: NavRoute; label: string }[] = [
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
          aria-current={route.name === i.route ? 'page' : undefined}
          onClick={(e) => {
            // A real href keeps the link shareable, middle-clickable and crawlable; the handler
            // just avoids a full reload for a same-origin move. Modified clicks fall through to
            // the browser so open-in-new-tab still works.
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return
            e.preventDefault()
            navigateTo(ROUTE_PATHS[i.route])
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

/**
 * The warning that belongs on any artist- or album-keyed ranking while attribution is partial.
 *
 * Returns undefined at full coverage so the caveat disappears on its own once the history is
 * fully resolved, rather than becoming a permanent disclaimer nobody reads.
 */
function attributionCaveat(coverage: number): string | undefined {
  if (coverage >= 0.99) return undefined
  return (
    `Incomplete: only ${Math.round(coverage * 100)}% of listening time has artist attribution, ` +
    'so these totals read low and the ranking order is unreliable. Imported history names ' +
    'artists as text rather than by Spotify ID.'
  )
}

/**
 * The caveat for the genre chart.
 *
 * Two separate facts, and both belong here.
 *
 * The first is permanent: genres are a many-to-many labelling, so a play counts under every
 * genre its artists carry and the bars cannot be read as a part-to-whole breakdown. No amount of
 * coverage fixes that.
 *
 * The second disappears as coverage improves, like the artist caveat. It names the ORDER
 * specifically rather than waving at incompleteness, because that is what was measured: splitting
 * the genre-bearing artists into halves and ranking each independently reproduces 7-9 of the top
 * ten, so WHICH genres appear is robust, while the two halves disagree about first place.
 */
function genreCaveat(coverage: number): string {
  const inherent =
    'A track can count under several genres, so these do not add up to the total.'
  if (coverage <= 0 || coverage >= 0.99) return inherent
  return (
    `${inherent} Genres cover ${Math.round(coverage * 100)}% of listening time — they come from ` +
    'MusicBrainz, which finds an artist through their Spotify link, so artists imported by name ' +
    'have none. Which genres appear is reliable; their exact order is not.'
  )
}

/**
 * A labelled band of the dashboard.
 *
 * Nine cards in a flat column is a list, not a document: nothing tells a reader that the heatmap
 * and the by-year bars answer the same question at two scales, or that the all-time leaderboards
 * belong together. The headings cost one line each and turn scrolling into reading.
 */
function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="section" aria-label={title}>
      <h2 className="section__title">{title}</h2>
      {children}
    </section>
  )
}

function Content({ data }: { data: Dashboard }) {
  const year = data.currentYear.period
  const thisYear = data.topThisYear.artists.length > 0
  return (
    <>
      {/*
        Reading order: the global figures, then activity, then this year, then the daily rhythm,
        then all time.

        Recent-first is deliberate. Seventeen years of all-time totals are dominated by whatever
        was played most a decade ago and barely move month to month, so leading with them buries
        the part that actually changes. The listening-activity heatmap comes straight after the
        headline numbers because it is the one chart that shows the shape of the whole period at
        a glance.

        The headline block carries no heading -- it is the page's own title block, and labelling
        it would demote it to a peer of the sections below.
      */}
      <Hero data={data} />
      <KPIRow data={data} />
      <CoverageRow data={data} />

      <Section title="Activity">
        {/* Two scales of the same question, adjacent on purpose: the last two years day by day,
            then the whole archive year by year. */}
        <Calendar days={data.calendar} timezone={data.timezone} />
        <ByYear years={data.byYear ?? []} />
      </Section>

      {thisYear && (
        <Section title="This year">
          <div className="grid">
            <RankedBars
              title={`Top artists in ${year}`}
              subtitle="By listening time"
              kind="artist"
              entries={data.topThisYear.artists}
              caveat={attributionCaveat(data.artistCoverage)}
            />
            <RankedBars
              title={`Top tracks in ${year}`}
              subtitle="By listening time"
              kind="track"
              entries={data.topThisYear.tracks}
            />
          </div>
        </Section>
      )}

      <Section title="Rhythm">
        <div className="grid">
          <HourRhythm buckets={data.rhythm.hourOfDay} timezone={data.timezone} />
          <WeekdayRhythm buckets={data.rhythm.weekday} />
        </div>
      </Section>

      <Section title="The whole archive">
        <RecordsRow data={data} />

        <div className="grid">
          <RankedBars
            title="Top artists"
            subtitle="All time, by listening time"
            kind="artist"
            entries={data.top.artists}
            caveat={attributionCaveat(data.artistCoverage)}
          />
          <RankedBars
            title="Top tracks"
            subtitle="All time, by listening time"
            kind="track"
            entries={data.top.tracks}
          />
        </div>

        <div className="grid">
          <RankedBars
            title="Top albums"
            subtitle="All time, by listening time"
            kind="album"
            entries={data.top.albums}
            caveat={attributionCaveat(data.artistCoverage)}
          />
          {/*
            A ranked bar, deliberately NOT a stacked bar or a pie. Genres are a many-to-many
            labelling: a track belongs to several at once, so the segments would not sum to the whole
            and no honest "100%" exists.

            The genres are MusicBrainz's. Spotify removed its artist genres field in February 2026
            and every artist row carries an empty list, so this card explained its own absence for
            a while; external enrichment gave it a source again.
          */}
          <RankedBars
            title="Top genres"
            subtitle={data.genresAvailable ? 'All time, by listening time' : undefined}
            entries={data.top.genres}
            caveat={genreCaveat(data.genreCoverage)}
            unavailable={
              data.genresAvailable
                ? undefined
                : 'No artist has been matched to MusicBrainz yet, and Spotify removed its own ' +
                  'genre field in February 2026, so there is nothing to chart.'
            }
          />
        </div>

        {/* Last in the band, and last on the page: the one card that is read downward rather
            than compared across, so it wants a reader who has already stopped scanning. */}
        <YearArtists
          years={data.yearArtists ?? []}
          caveat={attributionCaveat(data.artistCoverage)}
        />
      </Section>

      <Footer data={data} />
    </>
  )
}
