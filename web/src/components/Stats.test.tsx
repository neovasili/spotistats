// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { CoverageRow, Hero } from './Stats'
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
