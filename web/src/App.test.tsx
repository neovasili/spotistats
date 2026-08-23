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
  currentYear: { period: '2026', metrics },
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
async function renderDashboard() {
  const original = globalThis.fetch
  globalThis.fetch = (() =>
    Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(dashboard) })) as unknown as typeof fetch
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

  it('puts the activity heatmap immediately after the headline figures', async () => {
    const { container } = await renderDashboard()
    // Nothing chart-like may come between the KPI tiles and the heatmap.
    const tiles = container.querySelector('.tiles')
    const firstCard = container.querySelector('.card')
    expect(tiles).toBeTruthy()
    expect(firstCard?.querySelector('.card__title')?.textContent).toBe('Listening activity')
  })
})
