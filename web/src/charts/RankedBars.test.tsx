// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { RankedBars } from './RankedBars'
import type { Entry } from '../lib/types'

afterEach(cleanup)

const entries: Entry[] = [
  { id: 'a1', rank: 1, name: 'Within Temptation', plays: 40, msPlayed: 9_000_000 },
  { id: 'a2', rank: 2, name: 'Sabaton', plays: 20, msPlayed: 4_000_000 },
]

describe('RankedBars', () => {
  it('renders a bar and a table view for real data', () => {
    render(<RankedBars title="Top artists" entries={entries} />)
    expect(screen.getByText('Within Temptation')).toBeTruthy()
    // The table view is the accessibility fallback and must always be reachable.
    expect(screen.getByRole('button', { name: 'Table' })).toBeTruthy()
  })

  it('distinguishes "not yet" from "cannot exist"', () => {
    // Empty, but the data could still arrive: waiting is the right advice.
    render(<RankedBars title="Top genres" entries={[]} />)
    expect(screen.getByText(/No listening recorded yet/)).toBeTruthy()
    cleanup()

    // Empty because the upstream field is gone. Telling the user to wait would be wrong, so
    // the message must NOT be the generic one.
    render(
      <RankedBars
        title="Top genres"
        entries={[]}
        unavailable="Spotify removed artist genres from its Web API in February 2026."
      />,
    )
    expect(screen.getByText(/removed artist genres/)).toBeTruthy()
    expect(screen.queryByText(/No listening recorded yet/)).toBeNull()
  })

  it('offers no table view when there is no chart to substitute for', () => {
    render(<RankedBars title="Top genres" entries={[]} unavailable="Gone upstream." />)
    // An empty table is not an accessible alternative to nothing; it is just an empty table.
    expect(screen.queryByRole('button', { name: 'Table' })).toBeNull()
  })

  it('keeps the table toggle whenever a chart IS drawn', () => {
    // Guards the optional `table` prop from quietly spreading to real charts.
    render(<RankedBars title="Top artists" entries={entries} caveat="Overlaps." />)
    expect(screen.getByRole('button', { name: 'Table' })).toBeTruthy()
  })
})
