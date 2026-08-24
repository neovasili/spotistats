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

/**
 * Records every URL requested and answers from a queue of responses.
 *
 * The queue feeds /list only. The Explorer also calls /meta once, to learn which years the data
 * covers, and that answer is fixed here so it cannot consume a queued /list page -- which is
 * exactly what broke these tests when /meta was introduced: assertions written against
 * "the first request" silently started inspecting the wrong one.
 */
function stubFetch(responses: unknown[], status = 200) {
  const urls: string[] = []
  let i = 0
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      urls.push(url)
      if (url.includes('/meta')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            Promise.resolve({
              metrics: { plays: 0, playsExact: 0, msPlayed: 0, msPlayedExact: 0, estimatedRatio: 0 },
              coverage: { firstPlayedAt: '2009-11-01T00:00:00.000Z', lastPlayedAt: null, approximate: false },
              timezone: 'Europe/Madrid',
            }),
        } as Response)
      }
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

/** The /list requests only, which is what every query assertion here is actually about. */
function listUrls(urls: string[]): string[] {
  return urls.filter((u) => u.includes('/list'))
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
    await waitFor(() => expect(listUrls(urls).length).toBe(1))

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
    await waitFor(() => expect(listUrls(urls).length).toBe(2))
    expect(listUrls(urls)[1]).toContain('q=wit')
  })

  it('does not send an empty q parameter', async () => {
    // The API rejects malformed parameters outright, so an empty value must be omitted, not sent.
    const urls = stubFetch([page([])])
    render(<Explorer />)
    await waitFor(() => expect(listUrls(urls).length).toBe(1))
    expect(listUrls(urls)[0]).not.toContain('q=')
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
    await waitFor(() => expect(listUrls(urls).length).toBeGreaterThan(0))
    const sent = listUrls(urls)[0]!
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
    await waitFor(() => expect(listUrls(urls).length).toBeGreaterThan(0))
    expect(listUrls(urls)[0]).toContain('dim=TRACK')
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

describe('Explorer period range', () => {
  it('offers every year the data covers, not a hardcoded floor', async () => {
    // The floor used to be a literal 2015, which hid six years of imported history with
    // nothing on screen to suggest the CONTROL was the limit rather than the data.
    stubFetch([page([])]) // /meta reports firstPlayedAt 2009-11-01
    render(<Explorer />)

    const yearSelect = await screen.findByLabelText('Year')
    await waitFor(() => {
      const years = [...yearSelect.querySelectorAll('option')].map((o) => o.getAttribute('value'))
      expect(years).toContain('2009')
    })
    const years = [...yearSelect.querySelectorAll('option')].map((o) => o.getAttribute('value'))
    expect(years).toContain('2010')
    expect(years).toContain('2015')
    // Nothing before the data starts: an empty year is harmless, but a list running to 1970
    // is noise.
    expect(years).not.toContain('2008')
    // All time stays first.
    expect(years[0]).toBe('ALL')
  })

  it('still offers a usable range when /meta fails', async () => {
    // A coverage lookup failure must not leave the reader with no year control at all.
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) =>
        url.includes('/meta')
          ? Promise.resolve({ ok: false, status: 500, json: () => Promise.resolve({}) } as Response)
          : Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(page([])) } as Response),
      ),
    )
    render(<Explorer />)

    const yearSelect = await screen.findByLabelText('Year')
    const years = [...yearSelect.querySelectorAll('option')].map((o) => o.getAttribute('value'))
    expect(years.length).toBeGreaterThan(1)
    expect(years).toContain('2015')
  })
})

describe('Explorer drill-down placement', () => {
  it('opens the detail panel above the results list, not below it', async () => {
    // The list runs to the bottom of the viewport, so a panel after it opened entirely
    // off-screen and selecting a row looked like nothing had happened.
    stubFetch([page([{ id: 'ar1', name: 'Within Temptation', ms: 3_600_000, plays: 12 }])])
    render(<Explorer />)

    await screen.findByText('Within Temptation')
    fireEvent.click(screen.getByRole('button', { name: /Within Temptation/ }))

    const panel = await screen.findByText('Listening', { selector: 'dt' })
    const detailCard = panel.closest('.card')!
    const table = document.querySelector('.datatable--interactive')!.closest('.card')!

    // DOCUMENT_POSITION_FOLLOWING means the table comes after the detail card.
    expect(detailCard.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING)
      .toBeTruthy()
  })

  it('keeps the list constrained while the panel is open', async () => {
    // The list stays a bounded, self-scrolling region whether or not the panel is open. That is
    // only sound because the panel has a FIXED height: filters + panel + list then adds up to
    // exactly the viewport, and there is one scroll region rather than a page scroll fighting a
    // nested one.
    stubFetch([page([{ id: 'ar1', name: 'Within Temptation', ms: 3_600_000, plays: 12 }])])
    render(<Explorer />)

    await screen.findByText('Within Temptation')
    // The height is applied from an effect, so wait for it rather than assuming the commit is
    // synchronous.
    await waitFor(() =>
      expect((document.querySelector('.results__scroll') as HTMLElement).style.maxHeight)
        .not.toBe(''),
    )

    fireEvent.click(screen.getByRole('button', { name: /Within Temptation/ }))
    await screen.findByText('Listening', { selector: 'dt' })

    await waitFor(() =>
      expect((document.querySelector('.results__scroll') as HTMLElement).style.maxHeight)
        .not.toBe(''),
    )
  })

  it('puts the panel in a fixed-height slot, so its content cannot resize the list', async () => {
    // The failure this prevents: a track's play log is far taller than an artist's figures, and
    // the chart/table toggle changes height again. Any of that reaching the layout would
    // collapse the results list, which is the bug that prompted the fixed slot.
    stubFetch([page([{ id: 'ar1', name: 'Within Temptation', ms: 3_600_000, plays: 12 }])])
    render(<Explorer />)

    await screen.findByText('Within Temptation')
    fireEvent.click(screen.getByRole('button', { name: /Within Temptation/ }))
    await screen.findByText('Listening', { selector: 'dt' })

    const slot = document.querySelector('.detailslot')
    expect(slot).not.toBeNull()
    // The panel is INSIDE the slot, so the slot's height governs rather than the card's.
    expect(slot!.querySelector('.card')).not.toBeNull()
  })

  it('closes on the close button and restores the list', async () => {
    stubFetch([page([{ id: 'ar1', name: 'Within Temptation', ms: 3_600_000, plays: 12 }])])
    render(<Explorer />)

    await screen.findByText('Within Temptation')
    fireEvent.click(screen.getByRole('button', { name: /Within Temptation/ }))
    await screen.findByText('Listening', { selector: 'dt' })

    fireEvent.click(screen.getByRole('button', { name: /^Close / }))

    await waitFor(() => expect(document.querySelector('.detailslot')).toBeNull())
    // And the selection leaves the URL, or a shared link would reopen what was just dismissed.
    expect(new URLSearchParams(window.location.search).get('id')).toBeNull()
  })

  it('closes on Escape', async () => {
    // A panel dismissable only by a small target is a panel people leave open.
    stubFetch([page([{ id: 'ar1', name: 'Within Temptation', ms: 3_600_000, plays: 12 }])])
    render(<Explorer />)

    await screen.findByText('Within Temptation')
    fireEvent.click(screen.getByRole('button', { name: /Within Temptation/ }))
    await screen.findByText('Listening', { selector: 'dt' })

    fireEvent.keyDown(window, { key: 'Escape' })
    await waitFor(() => expect(document.querySelector('.detailslot')).toBeNull())
  })
})

describe('Explorer drill-down trend', () => {
  /** Answers /list, /meta and /timeline, recording the timeline query. */
  function stubWithTimeline(item: { firstPlayedAt?: string; lastPlayedAt?: string }) {
    const timelineUrls: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url.includes('/meta')) {
          return Promise.resolve({
            ok: true, status: 200,
            json: () => Promise.resolve({
              metrics: { plays: 0, playsExact: 0, msPlayed: 0, msPlayedExact: 0, estimatedRatio: 0 },
              coverage: { firstPlayedAt: '2009-11-01T00:00:00.000Z', lastPlayedAt: null, approximate: false },
              timezone: 'Europe/Madrid',
            }),
          } as Response)
        }
        if (url.includes('/timeline')) {
          timelineUrls.push(url)
          return Promise.resolve({
            ok: true, status: 200,
            json: () => Promise.resolve({
              dim: 'ARTIST', id: 'ar1', name: 'X', bucket: 'year', from: '2009', to: '2026',
              points: [{ period: '2009', metrics: { plays: 3, playsExact: 3, msPlayed: 600_000, msPlayedExact: 600_000, estimatedRatio: 0 } }],
            }),
          } as Response)
        }
        const p = page([{ id: 'ar1', name: 'Within Temptation', ms: 3_600_000, plays: 12 }])
        p.items[0] = { ...p.items[0]!, ...item }
        return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(p) } as Response)
      }),
    )
    return timelineUrls
  }

  it('draws a YEARLY trend for an all-time selection instead of a "pick a year" nag', async () => {
    // The panel has a fixed height, so an empty state is a large blank band asking the reader to
    // narrow a query they deliberately widened. All time has an obvious trend of its own.
    const urls = stubWithTimeline({
      firstPlayedAt: '2009-11-01T00:00:00.000Z',
      lastPlayedAt: '2026-08-01T00:00:00.000Z',
    })
    render(<Explorer />)

    await screen.findByText('Within Temptation')
    fireEvent.click(screen.getByRole('button', { name: /Within Temptation/ }))

    await waitFor(() => expect(urls.length).toBeGreaterThan(0))
    expect(urls[0]).toContain('bucket=year')
    // The span is the ENTITY's own, not a guessed window.
    expect(urls[0]).toContain('from=2009')
    expect(urls[0]).toContain('to=2026')
    expect(screen.queryByText(/Pick a year/)).toBeNull()
  })

  it('still draws months when a year is selected', async () => {
    window.history.replaceState(null, '', '/explore?period=2026')
    const urls = stubWithTimeline({
      firstPlayedAt: '2009-11-01T00:00:00.000Z',
      lastPlayedAt: '2026-08-01T00:00:00.000Z',
    })
    render(<Explorer />)

    await screen.findByText('Within Temptation')
    fireEvent.click(screen.getByRole('button', { name: /Within Temptation/ }))

    await waitFor(() => expect(urls.length).toBeGreaterThan(0))
    expect(urls[0]).toContain('bucket=month')
    expect(urls[0]).toContain('from=2026-01')
  })

  it('says so plainly when there are no play dates to span', async () => {
    // Without first/last there is no honest range, and inventing one would draw a chart of a
    // window the reader never chose.
    const urls = stubWithTimeline({ firstPlayedAt: undefined, lastPlayedAt: undefined })
    render(<Explorer />)

    await screen.findByText('Within Temptation')
    fireEvent.click(screen.getByRole('button', { name: /Within Temptation/ }))

    await screen.findByText(/no trend to draw/)
    expect(urls.length).toBe(0)
  })
})
