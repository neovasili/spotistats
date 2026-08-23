import { useCallback, useEffect, useState } from 'react'

/**
 * A tiny path router with URL-backed query state, hand-rolled rather than pulled in as a
 * dependency.
 *
 * The site has three pages and exactly one path parameter, so a router library would be more
 * bytes than the pages it serves. Path-based (not hash-based) because the CloudFront
 * viewer-request function already rewrites extensionless paths to /index.html -- see
 * docs/SPECS.md 9.1 -- so deep links work without a fragment.
 */
export type Route =
  | { name: 'dashboard' }
  | { name: 'explore' }
  | { name: 'artist'; id: string }

/** The page names that appear in the navigation, i.e. the ones with no parameter. */
export type NavRoute = 'dashboard' | 'explore'

export const ROUTE_PATHS: Record<NavRoute, string> = {
  dashboard: '/',
  explore: '/explore',
}

/** The path for one artist's profile. Ids are opaque, so they are encoded. */
export function artistPath(id: string): string {
  return `/artist/${encodeURIComponent(id)}`
}

function routeOf(pathname: string): Route {
  const path = pathname.replace(/\/+$/, '')
  if (path === '/explore') return { name: 'explore' }
  if (path.startsWith('/artist/')) {
    // decodeURIComponent throws on a malformed escape ("/artist/%zz"), which a hand-typed or
    // truncated URL will produce. Falling back to the raw segment means the page renders a
    // "no such artist" state rather than the whole app failing to mount.
    const raw = path.slice('/artist/'.length)
    let id = raw
    try {
      id = decodeURIComponent(raw)
    } catch {
      // keep the raw segment
    }
    if (id) return { name: 'artist', id }
  }
  return { name: 'dashboard' }
}

/**
 * Subscribers to in-app navigation.
 *
 * navigateTo is module-level rather than a value handed down from the app root because the
 * link to an artist profile sits on a leaderboard row, four components deep inside a chart
 * that otherwise knows nothing about routing. Threading a callback through RankedBars and the
 * Explorer's row list -- neither of which has any other reason to care -- buys nothing over a
 * subscription, and a context provider is the same indirection with more ceremony.
 *
 * pushState does not fire popstate, so subscribers are notified explicitly rather than by
 * dispatching a synthetic event: a fake popstate would also wake any other listener on the
 * page and tell it the user pressed Back, which is not what happened.
 */
const listeners = new Set<() => void>()

export function navigateTo(path: string) {
  if (path === window.location.pathname + window.location.search) return
  window.history.pushState(null, '', path)
  // A route change is a new page as far as the reader is concerned, so start at the top.
  window.scrollTo({ top: 0 })
  for (const l of [...listeners]) l()
}

export function useRoute(): Route {
  const [route, setRoute] = useState<Route>(() => routeOf(window.location.pathname))

  useEffect(() => {
    const sync = () => setRoute(routeOf(window.location.pathname))
    // popstate covers the browser's own back/forward; the listener set covers in-app links.
    window.addEventListener('popstate', sync)
    listeners.add(sync)
    return () => {
      window.removeEventListener('popstate', sync)
      listeners.delete(sync)
    }
  }, [])

  return route
}

/**
 * Query state held in the URL, so any view of the Explorer is a shareable link
 * (docs/SPECS.md 7.2).
 *
 * Writes use replaceState, never pushState. A filter row where every keystroke and every
 * dropdown pushes a history entry turns the back button into "undo my last character",
 * stranding the reader dozens of entries from the page they arrived on. The URL stays
 * copyable either way; only the history trap is avoided.
 */
export function useUrlParams(): [URLSearchParams, (next: Record<string, string | undefined>) => void] {
  const [search, setSearch] = useState(() => window.location.search)

  useEffect(() => {
    const onPop = () => setSearch(window.location.search)
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  const update = useCallback((next: Record<string, string | undefined>) => {
    const params = new URLSearchParams(window.location.search)
    for (const [k, v] of Object.entries(next)) {
      if (v === undefined || v === '') params.delete(k)
      else params.set(k, v)
    }
    const qs = params.toString()
    window.history.replaceState(null, '', qs ? `${window.location.pathname}?${qs}` : window.location.pathname)
    setSearch(qs ? `?${qs}` : '')
  }, [])

  return [new URLSearchParams(search), update]
}
