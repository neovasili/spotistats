// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { CoverageRow, Hero, KPIRow, RecordsRow } from './Stats'
import type { Dashboard } from '../lib/types'

afterEach(cleanup)

/** A dashboard with only the fields these components read. */
function dash(over: Partial<Dashboard> = {}): Dashboard {
  return {
    allTime: { plays: 1000, playsExact: 1000, msPlayed: 3_600_000_000, msPlayedExact: 3_600_000_000, estimatedRatio: 0 },
    artistCoverage: 1,
    genreCoverage: 1,
    ...over,
  } as unknown as Dashboard
}

describe('Hero', () => {
  it('never prints "0% estimated"', () => {
    // A ratio of 0.0004 rounded to "0%", which reads as a bug and tells the reader nothing --
    // the one outcome worse than either the truth or silence.
    render(<Hero data={dash({
      allTime: { plays: 1000, playsExact: 999, msPlayed: 3_600_000_000, msPlayedExact: 3_599_000_000, estimatedRatio: 0.0004 },
    })} />)
    expect(screen.getByText(/estimated/).textContent).toContain('<1%')
    expect(screen.queryByText(/0% estimated/)).toBeNull()
  })

  it('reports a real share as a percentage', () => {
    render(<Hero data={dash({
      allTime: { plays: 10, playsExact: 7, msPlayed: 1000, msPlayedExact: 700, estimatedRatio: 0.27 },
    })} />)
    expect(screen.getByText(/estimated/).textContent).toContain('27%')
  })

  it('says nothing when nothing is estimated', () => {
    render(<Hero data={dash()} />)
    expect(screen.queryByText(/estimated/)).toBeNull()
  })
})

describe('CoverageRow', () => {
  it('shows artist attribution as well as genres', () => {
    // Artist coverage was missing entirely, despite mattering MORE: it governs whether the
    // rankings are split across two rows per artist.
    render(<CoverageRow data={dash({ artistCoverage: 0.67, genreCoverage: 0.56 })} />)
    expect(screen.getByText('Artist attribution')).toBeTruthy()
    expect(screen.getByText('67%')).toBeTruthy()
    expect(screen.getByText('Genre coverage')).toBeTruthy()
    expect(screen.getByText('56%')).toBeTruthy()
  })

  it('disappears entirely at full coverage rather than becoming furniture', () => {
    const { container } = render(<CoverageRow data={dash({ artistCoverage: 1, genreCoverage: 1 })} />)
    expect(container.querySelector('.coverage')).toBeNull()
  })

  it('drops just the figure that is complete', () => {
    render(<CoverageRow data={dash({ artistCoverage: 1, genreCoverage: 0.56 })} />)
    expect(screen.queryByText('Artist attribution')).toBeNull()
    expect(screen.getByText('Genre coverage')).toBeTruthy()
  })

  it('shows nothing when a figure is zero, which means no pass has run', () => {
    // Zero is "not computed yet", not "0% covered", and presenting it as a measurement would be
    // a confident lie about a number nobody has calculated.
    const { container } = render(<CoverageRow data={dash({ artistCoverage: 0, genreCoverage: 0 })} />)
    expect(container.querySelector('.coverage')).toBeNull()
  })
})

describe('Hero context', () => {
  it('restates the all-time total as days of continuous play', () => {
    // 26,406 hours is a number nobody can picture. The same fact as days is the whole point of
    // the line, so it is asserted rather than left to survive by luck.
    render(<Hero data={dash({
      allTime: { plays: 1000, playsExact: 1000, msPlayed: 95_040_000_000, msPlayedExact: 95_040_000_000, estimatedRatio: 0 },
    })} />)
    expect(screen.getByText(/continuous play/).textContent).toContain('≈ 1,100 days')
  })
})

/** A dashboard carrying only what KPIRow and RecordsRow read. */
function yearDash(currentMs: number, previousMs: number): Dashboard {
  return dash({
    kpis: { distinctTracks: 1, distinctArtists: 1, distinctAlbums: 1, distinctGenres: 1, currentStreak: 2, longestStreak: 9 },
    currentYear: {
      period: '2026',
      metrics: { plays: 10, playsExact: 10, msPlayed: currentMs, msPlayedExact: currentMs, estimatedRatio: 0 },
      previousYearToDate: { plays: 8, playsExact: 8, msPlayed: previousMs, msPlayedExact: previousMs, estimatedRatio: 0 },
    },
  } as unknown as Partial<Dashboard>)
}

describe('KPIRow year-over-year', () => {
  it('compares this year against the same calendar point last year', () => {
    render(<KPIRow data={yearDash(112_000, 100_000)} />)
    expect(screen.getByText('+12% vs this point last year')).toBeTruthy()
  })

  it('signs a decline', () => {
    render(<KPIRow data={yearDash(80_000, 100_000)} />)
    expect(screen.getByText('-20% vs this point last year')).toBeTruthy()
  })

  it('says nothing when there is no previous year to compare with', () => {
    // A "+100%" against zero is arithmetic dressed as insight -- the first year of an archive has
    // no comparison, and inventing one is worse than leaving the line out.
    render(<KPIRow data={yearDash(80_000, 0)} />)
    expect(screen.queryByText(/vs this point last year/)).toBeNull()
  })
})

describe('RecordsRow', () => {
  const records = { busiestDay: { date: '2019-03-14', plays: 142, msPlayed: 47_000_000 }, longestStreak: 214, longestStreakEnd: '2021-06-02' }

  it('names the busiest day, the longest streak and the first play', () => {
    render(<RecordsRow data={dash({
      records,
      coverage: { firstPlayedAt: '2009-11-01T00:00:00.000Z', lastPlayedAt: '2026-08-25T00:00:00.000Z', approximate: false },
    } as unknown as Partial<Dashboard>)} />)
    expect(screen.getByText('Busiest day')).toBeTruthy()
    expect(screen.getByText('Longest streak').textContent).toBe('Longest streak')
    expect(screen.getByText('214 days')).toBeTruthy()
    expect(screen.getByText('First play')).toBeTruthy()
  })

  it('disappears before any pass has computed it', () => {
    const { container } = render(<RecordsRow data={dash({
      records: { busiestDay: { date: '', plays: 0, msPlayed: 0 }, longestStreak: 0 },
      coverage: { firstPlayedAt: '', lastPlayedAt: '', approximate: false },
    } as unknown as Partial<Dashboard>)} />)
    expect(container.querySelector('.coverage')).toBeNull()
  })
})
