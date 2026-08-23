// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { Explorer } from './Explorer'
import type { ListResponse } from '../lib/api'

// The Explorer keeps its query in the URL, so tests share mutable global state: without this
// reset, one test's search term becomes the next test's initial filter.
beforeEach(() => {
  window.history.replaceState(null, '', '/explore')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

function page(
  items: { id: string; name: string; ms: number; plays: number; artist?: string; album?: string }[],
  nextCursor?: string,
): ListResponse {
  return {
    dim: 'TRACK',
    period: 'ALL',
    sort: 'ms',
    order: 'desc',
    nextCursor,
    items: items.map((i, n) => ({
      rank: n + 1,
      id: i.id,
      name: i.name,
      artistName: i.artist,
      albumName: i.album,
      metrics: { plays: i.plays, playsExact: 0, msPlayed: i.ms, msPlayedExact: 0, estimatedRatio: 1 },
      lastPlayedAt: '2026-08-22T19:00:00.000Z',
    })),
  }
}

/** Records every URL requested and answers from a queue of responses. */
function stubFetch(responses: unknown[], status = 200) {
  const urls: string[] = []
  let i = 0
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      urls.push(url)
      const body = responses[Math.min(i++, responses.length - 1)]
      return Promise.resolve({
        ok: status < 400,
        status,
        json: () => Promise.resolve(body),
      } as Response)
    }),
  )
  return urls
}

describe('Explorer', () => {
  it('lists entities and shows their figures', async () => {
    stubFetch([page([{ id: 't1', name: 'Nails In The Coffin', ms: 499_500, plays: 2 }])])
    render(<Explorer />)
    expect(await screen.findByText('Nails In The Coffin')).toBeTruthy()
    expect(screen.getByText('2')).toBeTruthy()
  })

  it('reports the API error message rather than a generic failure', async () => {
    // The API explains precisely what was wrong with a query; discarding that in favour of
    // "something went wrong" would make an unanswerable question indistinguishable from an
    // outage.
    stubFetch([{ error: { code: 'INVALID_PERIOD', message: 'period=2026-13 is not a month' } }], 400)
    render(<Explorer />)
    expect(await screen.findByText(/period=2026-13 is not a month/)).toBeTruthy()
  })

  it('debounces the search box into one request per pause', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const urls = stubFetch([page([])])
    render(<Explorer />)
    await waitFor(() => expect(urls.length).toBe(1))

    const box = screen.getByPlaceholderText('name contains…')
    // Three keystrokes in quick succession. fireEvent.change goes through React's own event
    // system; assigning .value directly is swallowed by React's change tracking.
    for (const v of ['w', 'wi', 'wit']) {
      fireEvent.change(box, { target: { value: v } })
    }
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300)
    })
    // One initial load plus exactly one search, not one per character.
    await waitFor(() => expect(urls.length).toBe(2))
    expect(urls[1]).toContain('q=wit')
  })

  it('does not send an empty q parameter', async () => {
    // The API rejects malformed parameters outright, so an empty value must be omitted, not sent.
    const urls = stubFetch([page([])])
    render(<Explorer />)
    await waitFor(() => expect(urls.length).toBe(1))
    expect(urls[0]).not.toContain('q=')
  })

  it('offers Load more only when the API returned a cursor', async () => {
    stubFetch([page([{ id: 't1', name: 'One', ms: 1000, plays: 1 }])])
    render(<Explorer />)
    await screen.findByText('One')
    expect(screen.queryByRole('button', { name: /Load/ })).toBeNull()
    cleanup()

    stubFetch([page([{ id: 't1', name: 'One', ms: 1000, plays: 1 }], 'cursor-abc')])
    render(<Explorer />)
    await screen.findByText('One')
    expect(screen.getByRole('button', { name: /Load/ })).toBeTruthy()
  })

  it('shows a distinct message for an empty result', async () => {
    stubFetch([page([])])
    render(<Explorer />)
    expect(await screen.findByText(/Nothing matches that query/)).toBeTruthy()
  })
})

describe('Explorer URL state', () => {
  it('reads its whole query from the URL, so a shared link reproduces the view', async () => {
    window.history.replaceState(null, '', '/explore?dim=ARTIST&period=2026&sort=plays&order=asc&q=within')
    const urls = stubFetch([page([])])
    render(<Explorer />)
    await waitFor(() => expect(urls.length).toBeGreaterThan(0))
    const sent = urls[0]
    expect(sent).toContain('dim=ARTIST')
    expect(sent).toContain('period=2026')
    expect(sent).toContain('sort=plays')
    expect(sent).toContain('order=asc')
    expect(sent).toContain('q=within')
  })

  it('falls back to a valid dimension rather than forwarding a bogus one', async () => {
    // The API rejects an unknown dim outright, so a hand-edited or stale link must not turn
    // into an error page.
    window.history.replaceState(null, '', '/explore?dim=NONSENSE')
    const urls = stubFetch([page([])])
    render(<Explorer />)
    await waitFor(() => expect(urls.length).toBeGreaterThan(0))
    expect(urls[0]).toContain('dim=TRACK')
  })

  it('writes filter changes without pushing history entries', async () => {
    // A filter row that pushes per change makes Back mean "undo one keystroke", stranding the
    // reader many entries from the page they arrived on.
    stubFetch([page([])])
    render(<Explorer />)
    const before = window.history.length
    fireEvent.click(screen.getByRole('button', { name: 'Artists' }))
    await waitFor(() => expect(window.location.search).toContain('dim=ARTIST'))
    expect(window.history.length).toBe(before)
  })

  it('drops a filter from the URL when it returns to its default', async () => {
    // Keeping period=ALL in the address bar makes a default look like a deliberate choice.
    window.history.replaceState(null, '', '/explore?period=2026')
    stubFetch([page([])])
    render(<Explorer />)
    fireEvent.change(screen.getByLabelText('Year'), { target: { value: 'ALL' } })
    await waitFor(() => expect(window.location.search).not.toContain('period'))
  })
})

describe('Explorer name context', () => {
  it('shows a track with its artist and album, as the dashboard does', async () => {
    stubFetch([
      page([
        {
          id: 't1',
          name: 'Nails In The Coffin',
          ms: 499_500,
          plays: 2,
          artist: 'Five Finger Death Punch',
          album: 'Legacy',
        },
      ]),
    ])
    render(<Explorer />)
    expect(await screen.findByText('Nails In The Coffin')).toBeTruthy()
    expect(screen.getByText('Five Finger Death Punch · Legacy')).toBeTruthy()
  })

  it('shows an album with just its artist', async () => {
    stubFetch([page([{ id: 'al1', name: 'Legacy', ms: 1000, plays: 1, artist: 'Five Finger Death Punch' }])])
    render(<Explorer />)
    expect(await screen.findByText('Legacy')).toBeTruthy()
    expect(screen.getByText('Five Finger Death Punch')).toBeTruthy()
  })

  it('leaves an artist row without a context line', async () => {
    stubFetch([page([{ id: 'ar1', name: 'Sabaton', ms: 1000, plays: 1 }])])
    const { container } = render(<Explorer />)
    expect(await screen.findByText('Sabaton')).toBeTruthy()
    expect(container.querySelector('.entity__context')).toBeNull()
  })
})
