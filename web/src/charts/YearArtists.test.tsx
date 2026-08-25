// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { YearArtists } from './YearArtists'
import type { YearEntry } from '../lib/types'

afterEach(cleanup)

const years: YearEntry[] = [
  { period: '2024', entry: { rank: 1, id: '4tZwfgrHOc3mvqYlEYSvVi', name: 'Moderat', plays: 200, msPlayed: 40_000_000 } },
  { period: '2025', entry: { rank: 1, id: 'nm:burial', name: 'Burial', plays: 300, msPlayed: 60_000_000 } },
]

describe('YearArtists', () => {
  it('reads newest first, because that is the end a reader recognises', () => {
    const { container } = render(<YearArtists years={years} />)
    const shown = Array.from(container.querySelectorAll('.yearlist__year')).map((y) => y.textContent)
    expect(shown).toEqual(['2025', '2024'])
  })

  it('keeps the table view chronological, so the years read as a sequence', () => {
    const { container } = render(<YearArtists years={years} />)
    fireEvent.click(screen.getByRole('button', { name: 'Table' }))
    const cells = Array.from(container.querySelectorAll('.datatable tbody tr td:first-child')).map((c) => c.textContent)
    expect(cells).toEqual(['2024', '2025'])
  })

  it('links the name inward to the profile and the artwork outward to Spotify', () => {
    const { container } = render(<YearArtists years={years} />)
    const name = screen.getByText('Moderat').closest('a')
    expect(name?.getAttribute('href')).toBe('/artist/4tZwfgrHOc3mvqYlEYSvVi')
    // Spotify's Developer Policy requires artwork to link back to the entity on Spotify.
    const out = container.querySelector('a[href^="https://open.spotify.com"]')
    expect(out?.getAttribute('href')).toBe('https://open.spotify.com/artist/4tZwfgrHOc3mvqYlEYSvVi')
  })

  it('does not link a name-keyed artist out to a Spotify URL that would 404', () => {
    const { container } = render(<YearArtists years={years} />)
    expect(container.querySelectorAll('a[href^="https://open.spotify.com"]').length).toBe(1)
  })

  it('keeps the row layout intact for a name-keyed artist with no Spotify id', () => {
    // SpotifyLink renders its children unwrapped for an `nm:` id, so a .namecell placed INSIDE it
    // vanishes and the artwork stacks above the name. Most early-year winners are name-keyed, so
    // this is the common case, not the edge one.
    const { container } = render(<YearArtists years={years} />)
    expect(container.querySelectorAll('.yearlist__row .namecell').length).toBe(2)
  })

  it('renders nothing before a rollup has resolved any year', () => {
    const { container } = render(<YearArtists years={[]} />)
    expect(container.firstChild).toBeNull()
  })
})
