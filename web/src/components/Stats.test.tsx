// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { ActivityStats, CoverageRow, Hero, KPIRow, RecordsRow } from './Stats'
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

/** A dashboard carrying only what ActivityStats and RecordsRow read. */
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

/** A dashboard carrying only what KPIRow reads. */
function countDash(distinctGenres: number): Dashboard {
  return dash({
    kpis: { distinctTracks: 13, distinctArtists: 3, distinctAlbums: 6, distinctGenres, currentStreak: 2, longestStreak: 9 },
  } as unknown as Partial<Dashboard>)
}

describe('KPIRow', () => {
  it('counts what the archive holds, and nothing time-bound', () => {
    // The streak and the year total moved into the Activity band: they answer "what is happening
    // lately?", not "how big is the collection?", and mixing the two is what made the row read as
    // five unrelated numbers.
    render(<KPIRow data={countDash(7)} />)
    expect(screen.getByText('Artists')).toBeTruthy()
    expect(screen.getByText('Albums')).toBeTruthy()
    expect(screen.getByText('Tracks')).toBeTruthy()
    expect(screen.queryByText('Current streak')).toBeNull()
    expect(screen.queryByText('2026')).toBeNull()
  })

  it('shows the genre count the rollup has always computed', () => {
    render(<KPIRow data={countDash(7)} />)
    expect(screen.getByText('Genres')).toBeTruthy()
    expect(screen.getByText('7')).toBeTruthy()
  })

  it('omits the genre count at zero, which means no enrichment pass has run', () => {
    // Genres come from MusicBrainz, so the field is 0 before the first pass. "0 genres" would be
    // a confident lie about a number nobody has calculated.
    render(<KPIRow data={countDash(0)} />)
    expect(screen.queryByText('Genres')).toBeNull()
  })
})

describe('ActivityStats year-over-year', () => {
  it('compares this year against the same calendar point last year', () => {
    render(<ActivityStats data={yearDash(112_000, 100_000)} />)
    expect(screen.getByText(/\+12% vs this point last year/)).toBeTruthy()
  })

  it('signs a decline', () => {
    render(<ActivityStats data={yearDash(80_000, 100_000)} />)
    expect(screen.getByText(/-20% vs this point last year/)).toBeTruthy()
  })

  it('says nothing when there is no previous year to compare with', () => {
    // A "+100%" against zero is arithmetic dressed as insight -- the first year of an archive has
    // no comparison, and inventing one is worse than leaving the line out.
    const { container } = render(<ActivityStats data={yearDash(80_000, 0)} />)
    expect(screen.queryByText(/vs this point last year/)).toBeNull()
    // Not merely empty: the second line is ABSENT. A blank one makes the tile look like it
    // follows a rule it does not, which is why the streak tile grew a sub-line in the first place.
    const subs = [...container.querySelectorAll('.tile__sub')].map((e) => e.textContent)
    expect(subs).toEqual(['longest 9d'])
  })

  it('carries the direction on the arrow and the sign, not on the colour', () => {
    // Green against red is confusable under the two common colour deficiencies whatever steps
    // are chosen, so the glyph and the sign are the channels that must survive. Asserted rather
    // than left to a stylesheet nobody tests.
    const { container } = render(<ActivityStats data={yearDash(112_000, 100_000)} />)
    const up = container.querySelector('.delta')!
    expect(up.getAttribute('data-dir')).toBe('up')
    expect(up.textContent).toContain('↑')

    cleanup()
    const down = render(<ActivityStats data={yearDash(80_000, 100_000)} />)
      .container.querySelector('.delta')!
    expect(down.getAttribute('data-dir')).toBe('down')
    expect(down.textContent).toContain('↓')
  })

  it('states a level year in words rather than as a signed zero', () => {
    render(<ActivityStats data={yearDash(100_000, 100_000)} />)
    expect(screen.getByText('level with last year')).toBeTruthy()
    expect(screen.queryByText(/0%/)).toBeNull()
  })

  it('keeps the streak beside it, longest included', () => {
    render(<ActivityStats data={yearDash(112_000, 100_000)} />)
    expect(screen.getByText('Current streak')).toBeTruthy()
    expect(screen.getByText('2d')).toBeTruthy()
    expect(screen.getByText('longest 9d')).toBeTruthy()
  })
})

/** A dashboard with the shorter-scale figures the newer tiles read. */
function recentDash(over: Record<string, unknown> = {}): Dashboard {
  return dash({
    kpis: { distinctTracks: 1, distinctArtists: 1, distinctAlbums: 1, distinctGenres: 1, currentStreak: 2, longestStreak: 9 },
    currentYear: {
      period: '2026',
      metrics: { plays: 10, playsExact: 10, msPlayed: 112_000, msPlayedExact: 112_000, estimatedRatio: 0 },
      previousYearToDate: { plays: 8, playsExact: 8, msPlayed: 100_000, msPlayedExact: 100_000, estimatedRatio: 0 },
    },
    currentMonth: {
      period: '2026-08', elapsed: 15,
      metrics: { plays: 40, playsExact: 40, msPlayed: 7_200_000, msPlayedExact: 7_200_000, estimatedRatio: 0 },
      previousToDate: { plays: 20, playsExact: 20, msPlayed: 3_600_000, msPlayedExact: 3_600_000, estimatedRatio: 0 },
    },
    currentWeek: {
      period: '2026-08-24', elapsed: 4,
      metrics: { plays: 8, playsExact: 8, msPlayed: 1_800_000, msPlayedExact: 1_800_000, estimatedRatio: 0 },
      previousToDate: { plays: 16, playsExact: 16, msPlayed: 3_600_000, msPlayedExact: 3_600_000, estimatedRatio: 0 },
    },
    artistOfMonth: { rank: 1, id: 'a1', name: 'Within Temptation', plays: 20, msPlayed: 4_000_000 },
    artistOfWeek: { rank: 1, id: 'a2', name: 'Five Finger Death Punch', plays: 5, msPlayed: 900_000 },
    ...over,
  } as unknown as Partial<Dashboard>)
}

describe('ActivityStats shorter scales', () => {
  it('reads longest to shortest: year, month, week', () => {
    // One zoom rather than a bag of periods. Ordering them any other way makes the row read as
    // six unrelated figures, which is the defect that got the streak and the year moved out of
    // the headline block in the first place.
    const { container } = render(<ActivityStats data={recentDash()} />)
    const labels = [...container.querySelectorAll('.tile__label')].map((l) => l.textContent)
    expect(labels).toEqual([
      'Current streak', '2026', 'This month', 'Artist of the month', 'This week', 'Artist of the week',
    ])
  })

  it('compares each period against the same stretch of the one before it', () => {
    render(<ActivityStats data={recentDash()} />)
    expect(screen.getByText(/\+100% vs this point last month/)).toBeTruthy()
    expect(screen.getByText(/-50% vs this point last week/)).toBeTruthy()
  })

  it('colours a falling week red and a rising month green, arrows included', () => {
    const { container } = render(<ActivityStats data={recentDash()} />)
    const dirs = [...container.querySelectorAll('.delta')].map((d) => d.getAttribute('data-dir'))
    // year up, month up, week down
    expect(dirs).toEqual(['up', 'up', 'down'])
    const week = [...container.querySelectorAll('.delta')].at(-1)!
    expect(week.textContent).toContain('↓')
  })

  it('names the artist who owns each period, with their listening time', () => {
    render(<ActivityStats data={recentDash()} />)
    expect(screen.getByText('Within Temptation')).toBeTruthy()
    expect(screen.getByText('Five Finger Death Punch')).toBeTruthy()
    // 4,000,000ms is 1h 6m; 900,000ms is 15m.
    expect(screen.getByText('1h 6m')).toBeTruthy()
    expect(screen.getByText('15m')).toBeTruthy()
  })

  it('links an artist tile to the profile, like every other artist on the page', () => {
    const { container } = render(<ActivityStats data={recentDash()} />)
    const href = container.querySelector('.tile--artist a')?.getAttribute('href')
    expect(href).toContain('a1')
  })

  it('puts the day count in the tooltip, not on the face', () => {
    // "4 days in" is what makes a partial total interpretable, but a six-tile row has no width
    // for a third line.
    const { container } = render(<ActivityStats data={recentDash()} />)
    const week = [...container.querySelectorAll('.tile')].find(
      (t) => t.querySelector('.tile__label')?.textContent === 'This week',
    )!
    expect(week.getAttribute('title')).toContain('day 4 of this week')
  })

  it('drops the newer tiles when the snapshot predates them', () => {
    // A published bundle can be newer than the JSON it fetches: the rollup writes a snapshot
    // every two hours and the CDN serves the last one. Half a row beats four tiles reading zero.
    const { container } = render(
      <ActivityStats data={recentDash({
        currentMonth: undefined, currentWeek: undefined,
        artistOfMonth: undefined, artistOfWeek: undefined,
      })} />,
    )
    const labels = [...container.querySelectorAll('.tile__label')].map((l) => l.textContent)
    expect(labels).toEqual(['Current streak', '2026'])
  })

  it('keeps the period tile when its artist is missing, and the other way round', () => {
    // They come from different sources and either can fail on its own, so neither may take the
    // other down with it.
    const { container } = render(
      <ActivityStats data={recentDash({ artistOfMonth: undefined })} />,
    )
    const labels = [...container.querySelectorAll('.tile__label')].map((l) => l.textContent)
    expect(labels).toEqual(['Current streak', '2026', 'This month', 'This week', 'Artist of the week'])
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
