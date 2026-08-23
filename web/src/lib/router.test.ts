// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { artistPath, navigateTo, useRoute } from './router'

beforeEach(() => {
  window.history.replaceState(null, '', '/')
})

describe('routeOf, via useRoute', () => {
  it('reads the artist id out of the path', () => {
    window.history.replaceState(null, '', '/artist/3TOqt5oJwL9BE2NG9MEwDa')
    const { result } = renderHook(() => useRoute())
    expect(result.current).toEqual({ name: 'artist', id: '3TOqt5oJwL9BE2NG9MEwDa' })
  })

  it('decodes an id that needed encoding', () => {
    // Name-keyed artists carry a colon, and imported names carry spaces.
    window.history.replaceState(null, '', artistPath('nm:within temptation'))
    const { result } = renderHook(() => useRoute())
    expect(result.current).toEqual({ name: 'artist', id: 'nm:within temptation' })
  })

  it('survives a malformed escape rather than failing to mount', () => {
    // decodeURIComponent throws on "%zz", which a hand-typed or truncated URL will produce.
    // The page must render a "no such artist" state, not take the whole app down.
    window.history.replaceState(null, '', '/artist/%zz')
    const { result } = renderHook(() => useRoute())
    expect(result.current).toEqual({ name: 'artist', id: '%zz' })
  })

  it('treats a bare /artist/ as the dashboard', () => {
    window.history.replaceState(null, '', '/artist/')
    const { result } = renderHook(() => useRoute())
    expect(result.current).toEqual({ name: 'dashboard' })
  })

  it('ignores a trailing slash', () => {
    window.history.replaceState(null, '', '/explore/')
    const { result } = renderHook(() => useRoute())
    expect(result.current).toEqual({ name: 'explore' })
  })

  it('falls back to the dashboard for anything unrecognised', () => {
    window.history.replaceState(null, '', '/nope')
    const { result } = renderHook(() => useRoute())
    expect(result.current).toEqual({ name: 'dashboard' })
  })
})

describe('navigateTo', () => {
  it('updates every mounted subscriber, not just the caller', () => {
    // pushState does not fire popstate, so a subscription is what keeps the route in sync.
    // Without it an in-app link would change the URL and render nothing new.
    const a = renderHook(() => useRoute())
    const b = renderHook(() => useRoute())

    act(() => navigateTo(artistPath('ar1')))

    expect(a.result.current).toEqual({ name: 'artist', id: 'ar1' })
    expect(b.result.current).toEqual({ name: 'artist', id: 'ar1' })
    expect(window.location.pathname).toBe('/artist/ar1')
  })

  it('pushes history, so Back returns to the previous page', () => {
    const before = window.history.length
    act(() => navigateTo('/explore'))
    expect(window.history.length).toBeGreaterThan(before)
  })

  it('does not stack a duplicate entry for the current path', () => {
    act(() => navigateTo('/explore'))
    const len = window.history.length
    act(() => navigateTo('/explore'))
    expect(window.history.length).toBe(len)
  })

  it('unsubscribes on unmount', () => {
    const { unmount, result } = renderHook(() => useRoute())
    unmount()
    // No throw, and no update to an unmounted hook -- which React would warn about.
    act(() => navigateTo('/explore'))
    expect(result.current).toEqual({ name: 'dashboard' })
  })
})
