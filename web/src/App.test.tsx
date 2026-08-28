// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render } from '@testing-library/react'
import { App } from './App'
import type { Dashboard } from './lib/types'

afterEach(cleanup)

const metrics = { plays: 10, playsExact: 10, msPlayed: 3_600_000, msPlayedExact: 3_600_000, estimatedRatio: 0 }
const entry = (id: string, name: string) => ({ rank: 1, id, name, plays: 5, msPlayed: 1_800_000 })

const dashboard: Dashboard = {
  generatedAt: '2026-08-23T17:00:00.000Z',
  timezone: 'Europe/Madrid',
  coverage: { firstPlayedAt: '2009-11-01T00:00:00.000Z', lastPlayedAt: '2026-08-23T00:00:00.000Z', approximate: false },
  allTime: metrics,
  currentYear: { period: '2026', metrics, previousYearToDate: metrics },
  kpis: { distinctTracks: 3, distinctArtists: 2, distinctAlbums: 2, distinctGenres: 0, currentStreak: 1, longestStreak: 4 },
  top: {
    artists: [entry('a1', 'All-time Artist')],
    tracks: [entry('t1', 'All-time Track')],
    albums: [entry('al1', 'All-time Album')],
    genres: [],
  },
  topThisYear: {
    artists: [entry('a2', 'This-year Artist')],
    tracks: [entry('t2', 'This-year Track')],
  },
  calendar: [{ date: '2026-08-20', plays: 3, msPlayed: 900_000 }],
  byYear: [
    { period: '2025', plays: 10, msPlayed: 3_600_000 },
    { period: '2026', plays: 5, msPlayed: 1_800_000 },
  ],
  yearArtists: [{ period: '2026', entry: entry('a2', 'This-year Artist') }],
  records: { busiestDay: { date: '2026-08-20', plays: 3, msPlayed: 900_000 }, longestStreak: 4, longestStreakEnd: '2026-08-20' },
  rhythm: {
    hourOfDay: Array.from({ length: 24 }, (_, i) => ({ bucket: i, plays: 0, msPlayed: 0 })),
    weekday: Array.from({ length: 7 }, (_, i) => ({ bucket: i, plays: 0, msPlayed: 0 })),
  },
  genreCoverage: 0,
  artistCoverage: 1,
  genresAvailable: false,
  notes: [],
}

/** Renders the dashboard with the snapshot fetch stubbed. */
async function renderDashboard(over: Partial<Dashboard> = {}) {
  const payload = { ...dashboard, ...over }
  const original = globalThis.fetch
  globalThis.fetch = (() =>
    Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(payload) })) as unknown as typeof fetch
  const view = render(<App />)
  // Let the effect's promise chain settle.
  await new Promise((r) => setTimeout(r, 0))
  globalThis.fetch = original
  return view
}

describe('dashboard reading order', () => {
  it('leads with activity, then this year, then rhythm, then all time', async () => {
    // Recent-first is deliberate: seventeen years of all-time totals barely move month to
    // month, so leading with them buries the part that changes. Asserted because a reordering
    // is exactly the kind of change that gets undone by accident.
    const { container } = await renderDashboard()
    const headings = Array.from(container.querySelectorAll('.card__title')).map((h) => h.textContent)

    const at = (needle: string) => headings.findIndex((h) => h?.includes(needle))
    const activity = at('Listening activity')
    const thisYear = at('Top artists in 2026')
    const hourOfDay = at('By hour of day')
    const allTime = at('Top artists')

    expect(activity).toBeGreaterThanOrEqual(0)
    expect(activity).toBeLessThan(thisYear)
    expect(thisYear).toBeLessThan(hourOfDay)
    expect(hourOfDay).toBeLessThan(headings.lastIndexOf('Top artists'))
    // "Top artists" (all time) must come after the rhythm charts, not before.
    expect(headings.lastIndexOf('Top artists')).toBeGreaterThan(hourOfDay)
    expect(allTime).toBeGreaterThanOrEqual(0)
  })

  it('groups the cards into labelled bands, in reading order', async () => {
    // Nine cards in a flat column is a list, not a document. The headings are what make the
    // heatmap and the by-year bars read as two scales of one question.
    const { container } = await renderDashboard()
    const bands = Array.from(container.querySelectorAll('.section__title')).map((h) => h.textContent)
    expect(bands).toEqual(['Activity', 'This year', 'Rhythm', 'The whole archive'])
  })

  it('leaves the headline block unlabelled, so it stays the page title rather than a peer', async () => {
    const { container } = await renderDashboard()
    const first = container.querySelector('.page > .hero, .page .hero')
    expect(first).toBeTruthy()
    // The hero must not be inside a band.
    expect(first?.closest('.section')).toBeNull()
    // It WEARS the card's chrome but must not take the class: the test below asserts the page's
    // first .card is the heatmap, and a hero carrying the class would break that invisibly.
    expect(first?.classList.contains('card')).toBe(false)
  })

  it('puts the whole-archive year series in the Activity band, next to the heatmap', async () => {
    // Two scales of the same question, plus who each year belonged to. "Your year in one artist"
    // used to trail the all-time leaderboards, where it was read after the reader had stopped.
    const { container } = await renderDashboard()
    const activity = Array.from(container.querySelectorAll('.section')).find(
      (s) => s.querySelector('.section__title')?.textContent === 'Activity',
    )
    const titles = Array.from(activity?.querySelectorAll('.card__title') ?? []).map((t) => t.textContent)
    expect(titles).toEqual(['Listening activity', 'Listening by year', 'Your year in one artist'])
  })

  it('pairs the cards two to a row, except the heatmap', async () => {
    // The heatmap is a hundred-odd week columns wide and cannot be halved; everything else on the
    // page reads as a comparable pair, which is what the .grid wrappers mark.
    const { container } = await renderDashboard()
    const pairs = Array.from(container.querySelectorAll('.grid'))
    expect(pairs.length).toBeGreaterThan(0)
    for (const pair of pairs) {
      expect(pair.querySelectorAll(':scope > .card').length).toBe(2)
    }
    // The heatmap sits directly in its band, outside any pair.
    const heatmap = container.querySelector('.heatmap')!.closest('.card')!
    expect(heatmap.parentElement?.classList.contains('grid')).toBe(false)
  })

  it('leads the headline row with inventory, the streak and the year having moved down', async () => {
    const { container } = await renderDashboard()
    const headline = container.querySelector('.headline')!
    const labels = Array.from(headline.querySelectorAll('.tile__label')).map((l) => l.textContent)
    expect(labels).toEqual(['Artists', 'Albums', 'Tracks'])

    // ...and they land in Activity, ahead of the heatmap.
    const activity = Array.from(container.querySelectorAll('.section')).find(
      (s) => s.querySelector('.section__title')?.textContent === 'Activity',
    )!
    const moved = Array.from(activity.querySelectorAll('.tile__label')).map((l) => l.textContent)
    expect(moved).toEqual(['Current streak', '2026'])
  })

  it('gives This year the same four charts as the archive, in the same order', async () => {
    const { container } = await renderDashboard({
      topThisYear: {
        artists: [entry('a2', 'This-year Artist')],
        tracks: [entry('t2', 'This-year Track')],
        albums: [entry('al2', 'This-year Album')],
        genres: [entry('g2', 'this-year genre')],
      },
      genresAvailable: true,
    })
    const band = Array.from(container.querySelectorAll('.section')).find(
      (s) => s.querySelector('.section__title')?.textContent === 'This year',
    )!
    const titles = Array.from(band.querySelectorAll('.card__title')).map((t) => t.textContent)
    expect(titles).toEqual([
      'Top artists in 2026', 'Top albums in 2026', 'Top genres in 2026', 'Top tracks in 2026',
    ])
  })

  it('lays the two leaderboard bands out identically, which is the point of having both', async () => {
    // The comparison only works if the charts sit in the same places. They drifted apart once --
    // the archive was reordered and This year was not -- which is why one component now draws
    // both, and this fails the moment someone un-shares it.
    //
    // It compares TITLE order, so it does not catch a chart fed the wrong dimension's data; the
    // absolute-order assertions above and below are what pin the layout itself.
    const { container } = await renderDashboard({
      topThisYear: {
        artists: [entry('a2', 'This-year Artist')],
        tracks: [entry('t2', 'This-year Track')],
        albums: [entry('al2', 'This-year Album')],
        genres: [entry('g2', 'this-year genre')],
      },
      genresAvailable: true,
    })
    const dims = (band: string) => {
      const el = Array.from(container.querySelectorAll('.section')).find(
        (s) => s.querySelector('.section__title')?.textContent === band,
      )!
      return Array.from(el.querySelectorAll('.card__title'))
        .map((t) => t.textContent!.replace(/^Top /, '').replace(/ in \d{4}$/, ''))
    }
    expect(dims('This year')).toEqual(dims('The whole archive'))
    // ...and that shared order is the intended one, not merely a consistent accident.
    expect(dims('The whole archive')).toEqual(['artists', 'albums', 'genres', 'tracks'])
  })

  it('falls back to the artists-and-tracks pair when the snapshot predates the other two', async () => {
    // The base fixture has no this-year albums or genres. Empty cards would say "no albums this
    // year", which is a claim rather than a gap.
    const { container } = await renderDashboard()
    const band = Array.from(container.querySelectorAll('.section')).find(
      (s) => s.querySelector('.section__title')?.textContent === 'This year',
    )!
    const titles = Array.from(band.querySelectorAll('.card__title')).map((t) => t.textContent)
    expect(titles).toEqual(['Top artists in 2026', 'Top tracks in 2026'])
  })

  it('shows the genre count once the enrichment pass has run', async () => {
    // The fixture above has distinctGenres: 0, which is what a snapshot looks like before any
    // artist has been matched to MusicBrainz.
    const { container } = await renderDashboard({
      kpis: { ...dashboard.kpis, distinctGenres: 42 },
    })
    const labels = Array.from(container.querySelectorAll('.headline .tile__label')).map((l) => l.textContent)
    expect(labels).toEqual(['Artists', 'Albums', 'Tracks', 'Genres'])
  })

  it('puts the activity heatmap immediately after the headline figures', async () => {
    const { container } = await renderDashboard()
    // Nothing chart-like may come between the KPI tiles and the heatmap.
    const tiles = container.querySelector('.tiles')
    const firstCard = container.querySelector('.card')
    expect(tiles).toBeTruthy()
    expect(firstCard?.querySelector('.card__title')?.textContent).toBe('Listening activity')
  })
})
