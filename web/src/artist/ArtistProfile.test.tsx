// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { ArtistProfilePage } from './ArtistProfile'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

/** A minimal profile payload; each test overrides only the part it is about. */
function profile(over: Record<string, unknown> = {}) {
  return {
    id: 'ar1',
    name: 'Within Temptation',
    mbid: 'mb-1',
    resolvedVia: 'link',
    artistType: 'Group',
    country: 'NL',
    areaName: 'Netherlands',
    beginAreaName: 'Waddinxveen',
    beganAt: '1996',
    beganPrecision: 'year',
    mbGenres: ['symphonic metal', 'gothic metal'],
    members: [{ name: 'Sharon den Adel', instruments: ['vocals'], begin: '1996' }],
    biography: 'A Dutch band.\nSecond paragraph.',
    biographyLang: 'en',
    images: { thumb: 'https://r2.theaudiodb.com/t.jpg' },
    listening: {
      metrics: { plays: 120, playsExact: 120, msPlayed: 26_000_000, msPlayedExact: 26_000_000, estimatedRatio: 0 },
      firstPlayedAt: '2009-04-01T10:00:00.000Z',
      lastPlayedAt: '2026-08-01T10:00:00.000Z',
      spotifyGenres: ['symphonic metal', 'dutch metal'],
    },
    sources: { facts: 'musicbrainz', prose: 'theaudiodb', images: 'theaudiodb' },
    refreshedAt: '2026-08-20T04:15:00.000Z',
    ...over,
  }
}

function stubFetch(status: number, body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => ({
      ok: status < 400,
      status,
      json: async () => body,
    })),
  )
}

describe('ArtistProfilePage', () => {
  beforeEach(() => {
    window.history.replaceState(null, '', '/artist/ar1')
  })

  it('renders the facts at the precision they were stored at', async () => {
    stubFetch(200, profile())
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText('Within Temptation')
    // "1996", never "1 January 1996": the source claims a year, so the page claims a year.
    expect(screen.getByText('1996', { selector: 'dd' })).toBeTruthy()
    expect(screen.queryByText(/January 1996/)).toBeNull()
    // Formation city and country are different facts and both appear.
    expect(screen.getByText('Waddinxveen')).toBeTruthy()
    expect(screen.getByText('Netherlands')).toBeTruthy()
  })

  it('renders a month-precision date as a month, not a day', async () => {
    stubFetch(200, profile({ beganAt: '1996-04', beganPrecision: 'month' }))
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText('Facts')
    // "April 1996" asserts exactly what is stored. "1 April 1996" would invent a day and
    // "1996-04" is a machine string, not a date a reader parses.
    expect(screen.getByText('April 1996', { selector: 'dd' })).toBeTruthy()
    expect(screen.queryByText('1996-04')).toBeNull()
  })

  it('keeps the two genre vocabularies in separate labelled rows', async () => {
    stubFetch(200, profile())
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText('Genres')
    const mbLabel = screen.getByText('MusicBrainz', { selector: '.chiprow__label' })
    const spotifyLabel = screen.getByText('Spotify', { selector: '.chiprow__label' })

    const mbChips = mbLabel.parentElement!.querySelectorAll('.chip')
    const spotifyChips = spotifyLabel.parentElement!.querySelectorAll('.chip')

    expect([...mbChips].map((c) => c.textContent)).toEqual(['symphonic metal', 'gothic metal'])
    expect([...spotifyChips].map((c) => c.textContent)).toEqual(['symphonic metal', 'dutch metal'])

    // The merged-row failure mode: a single row containing every genre from both sources. It
    // would present an agreement that does not exist, so no row may hold more than its own.
    for (const row of document.querySelectorAll('.chiprow')) {
      expect(row.querySelectorAll('.chip').length).toBeLessThanOrEqual(2)
    }
  })

  it('credits every source it actually used', async () => {
    stubFetch(200, profile())
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText(/last checked/)
    // TheAudioDB's terms require the credit AND a link back; MusicBrainz genres are
    // CC-BY-NC-SA, so the licence link is an obligation, not decoration.
    const links = [...document.querySelectorAll('.attribution a')].map((a) => a.getAttribute('href'))
    expect(links.some((h) => h?.includes('theaudiodb.com'))).toBe(true)
    expect(links.some((h) => h?.includes('musicbrainz.org/artist/mb-1'))).toBe(true)
    expect(links.some((h) => h?.includes('by-nc-sa'))).toBe(true)
    expect(links.some((h) => h?.includes('spotify.com'))).toBe(true)
  })

  it('does not credit a source that returned nothing', async () => {
    // A MusicBrainz hit with a TheAudioDB miss is the normal partial case, not an error. The
    // page must not claim a biography came from a service that never answered.
    stubFetch(200, profile({ biography: '', sources: { facts: 'musicbrainz' } }))
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText(/last checked/)
    const links = [...document.querySelectorAll('.attribution a')].map((a) => a.getAttribute('href'))
    expect(links.some((h) => h?.includes('theaudiodb.com'))).toBe(false)
    expect(links.some((h) => h?.includes('musicbrainz.org'))).toBe(true)
  })

  it('states plainly that an unresolved artist has no external profile', async () => {
    stubFetch(200, profile({
      mbid: '', resolvedVia: '', artistType: '', country: '', areaName: '',
      beginAreaName: '', beganAt: '', mbGenres: [], members: [], biography: '',
      sources: {},
    }))
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText(/No external profile is linked/)
    // Never a skeleton, and never invented prose. The listening block still carries figures.
    // Scoped to card titles: "Listening" and "Members" also appear as figure labels, so a
    // bare text query is ambiguous.
    expect(screen.queryByText('Biography', { selector: '.card__title' })).toBeNull()
    expect(screen.queryByText('Members', { selector: '.card__title' })).toBeNull()
    expect(screen.getByText('Listening', { selector: '.card__title' })).toBeTruthy()
    expect(screen.getByText('120')).toBeTruthy()
    // Spotify genres survive independently of enrichment, so that row remains.
    expect(screen.getByText('Spotify', { selector: '.chiprow__label' })).toBeTruthy()
    expect(screen.queryByText('MusicBrainz', { selector: '.chiprow__label' })).toBeNull()
  })

  it('distinguishes never-enriched from enriched-and-empty', async () => {
    // A 404 means enrichment has never looked at this artist. That is a different fact from
    // an unresolved row, and it gets different words -- an unresolved artist has been checked.
    stubFetch(404, { error: { code: 'NOT_FOUND', message: 'no external profile yet' } })
    render(<ArtistProfilePage id="arX" />)

    await screen.findByText('No profile yet')
    expect(screen.queryByText(/No external profile is linked/)).toBeNull()
    expect(screen.queryByText('Loading…')).toBeNull()
  })

  it('reports a real failure as a failure', async () => {
    stubFetch(500, { error: { code: 'INTERNAL', message: 'boom' } })
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText('Nothing to show')
    // A 500 must not be mistaken for "not enriched yet": one is a bug, the other is normal.
    expect(screen.queryByText('No profile yet')).toBeNull()
  })

  it('marks past members without implying current ones ended', async () => {
    stubFetch(200, profile({
      members: [
        { name: 'Past Person', begin: '1996', end: '2002', ended: true },
        { name: 'Current Person', begin: '1996' },
        // A departure whose date MusicBrainz does not record. "1996–" would read as current.
        { name: 'Gone Undated', begin: '1998', ended: true },
      ],
    }))
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText('Members', { selector: '.card__title' })
    const rows = [...document.querySelectorAll('.members__row')]
    // Current members first.
    expect(rows[0]!.textContent).toContain('Current Person')
    expect(rows[0]!.className).not.toContain('members__row--past')

    const undated = rows.find((r) => r.textContent?.includes('Gone Undated'))!
    expect(undated.className).toContain('members__row--past')
    // The word, not just the dimming: identity must not depend on colour alone.
    expect(undated.textContent).toContain('former')
  })

  it('refetches when the artist changes', async () => {
    stubFetch(200, profile())
    const { rerender } = render(<ArtistProfilePage id="ar1" />)
    await screen.findByText('Within Temptation')

    stubFetch(200, profile({ id: 'ar2', name: 'Nightwish' }))
    rerender(<ArtistProfilePage id="ar2" />)
    await waitFor(() => expect(screen.getByText('Nightwish')).toBeTruthy())
  })

  it('requests the id as a path parameter, encoded', async () => {
    stubFetch(200, profile())
    render(<ArtistProfilePage id="nm:some artist" />)
    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const url = (fetch as unknown as { mock: { calls: [string][] } }).mock.calls[0]![0]
    expect(url).toContain('/artists/nm%3Asome%20artist/profile')
  })
})

describe('ArtistProfilePage most played', () => {
  const tops = {
    topAlbums: [
      { id: 'al1', name: 'Bleed Out', plays: 400, msPlayed: 80_000_000, thumbUrl: 'https://i.scdn.co/a.jpg' },
      { id: 'al2', name: 'Resist', plays: 100, msPlayed: 20_000_000 },
    ],
    topTracks: [
      { id: 't1', name: 'Wireless', context: 'Bleed Out', plays: 90, msPlayed: 18_000_000 },
    ],
    topItemsAt: '2026-08-25T03:15:00.000Z',
  }

  it('lists the artist’s own albums and tracks', async () => {
    stubFetch(200, profile({ listening: { ...profile().listening, ...tops } }))
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText('Most played')
    expect(screen.getByText('Albums')).toBeTruthy()
    expect(screen.getByText('Tracks')).toBeTruthy()
    // The rows use the app's shared bar markup, so they are identical to the dashboard
    // leaderboards rather than a second style that merely resembles them.
    expect(screen.getByText('Bleed Out', { selector: '.entity__name' })).toBeTruthy()
    // A track shows its album, so two identically titled songs are distinguishable.
    expect(screen.getByText('Bleed Out', { selector: '.entity__context' })).toBeTruthy()
    // The bar spans the free space rather than sitting in a fixed column beside the number: a
    // magnitude bar that does not span its axis encodes nothing.
    expect(document.querySelectorAll('.topitems .bar__fill').length).toBe(3)
    // Dated, because these are a nightly snapshot rather than live figures.
    expect(screen.getByText(/as of/)).toBeTruthy()
  })

  it('says nothing at all before a rollup has produced them', async () => {
    // An empty pair of lists would read as "you have never played them", which is false.
    stubFetch(200, profile())
    render(<ArtistProfilePage id="ar1" />)
    await screen.findByText('Within Temptation')
    expect(screen.queryByText('Most played')).toBeNull()
  })

  it('links artwork out to Spotify, as the policy requires', async () => {
    stubFetch(200, profile({ listening: { ...profile().listening, ...tops } }))
    render(<ArtistProfilePage id="ar1" />)

    await screen.findByText('Most played')
    const link = document.querySelector('.topitems .bar a')
    expect(link?.getAttribute('href') ?? '').toContain('open.spotify.com/album/al1')
  })
})

// The durations must carry minutes, as every other figure in the app does. I had hidden them
// with a display:none, which made this the one place a duration read differently.
it('shows durations in minutes as well, like everywhere else', async () => {
  stubFetch(200, profile({
    listening: {
      ...profile().listening,
      topAlbums: [{ id: 'al1', name: 'Bleed Out', plays: 400, msPlayed: 80_000_000 }],
      topTracks: [],
    },
  }))
  render(<ArtistProfilePage id="ar1" />)

  await screen.findByText('Most played')
  const value = document.querySelector('.topitems .bar__value')!
  expect(value.querySelector('.duration__minutes')).not.toBeNull()
  // 80,000,000 ms is 1,333 minutes.
  expect(value.textContent).toContain('1,333m')
})
