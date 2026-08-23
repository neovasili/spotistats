import { useCallback, useEffect, useState } from 'react'

/**
 * A two-route path router with URL-backed query state, hand-rolled rather than pulled in as a
 * dependency.
 *
 * The site has exactly two pages and no nested or parameterised routes, so a router library
 * would be more bytes than the pages it serves. Path-based (not hash-based) because the
 * CloudFront viewer-request function already rewrites unknown paths to /index.html -- see
 * docs/SPECS.md 9.1 -- so deep links work without a fragment.
 */
export type Route = 'dashboard' | 'explore'

export const ROUTE_PATHS: Record<Route, string> = {
  dashboard: '/',
  explore: '/explore',
}

function routeOf(pathname: string): Route {
  return pathname.replace(/\/+$/, '') === '/explore' ? 'explore' : 'dashboard'
}

export function useRoute(): [Route, (r: Route) => void] {
  const [route, setRoute] = useState<Route>(() => routeOf(window.location.pathname))

  // popstate covers the browser's own back/forward; navigate() below handles in-app links.
  useEffect(() => {
    const onPop = () => setRoute(routeOf(window.location.pathname))
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  const navigate = useCallback(
    (r: Route) => {
      if (r === routeOf(window.location.pathname)) return
      window.history.pushState(null, '', ROUTE_PATHS[r])
      setRoute(r)
      // A route change is a new page as far as the reader is concerned, so start at the top.
      window.scrollTo({ top: 0 })
    },
    [],
  )

  return [route, navigate]
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
