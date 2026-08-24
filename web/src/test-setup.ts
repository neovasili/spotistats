/**
 * Test environment shims.
 *
 * jsdom in this setup exposes a `localStorage` OBJECT with none of the Storage methods on it, so
 * any component that reads a stored preference — the theme toggle, and therefore anything that
 * renders the app shell — throws "localStorage.getItem is not a function". A minimal in-memory
 * Storage is enough and keeps the failure from dictating how components are written.
 */
function memoryStorage(): Storage {
  let map = new Map<string, string>()
  return {
    get length() {
      return map.size
    },
    clear: () => {
      map = new Map()
    },
    getItem: (k: string) => (map.has(k) ? map.get(k)! : null),
    key: (i: number) => Array.from(map.keys())[i] ?? null,
    removeItem: (k: string) => {
      map.delete(k)
    },
    setItem: (k: string, v: string) => {
      map.set(k, String(v))
    },
  }
}

for (const name of ['localStorage', 'sessionStorage'] as const) {
  const existing = globalThis[name] as Storage | undefined
  if (typeof existing?.getItem !== 'function') {
    Object.defineProperty(globalThis, name, {
      value: memoryStorage(),
      configurable: true,
      writable: true,
    })
  }
}

// matchMedia is absent in jsdom, and the tooltip checks prefers-reduced-motion.
if (typeof window !== 'undefined' && typeof window.matchMedia !== 'function') {
  window.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  })) as typeof window.matchMedia
}

// jsdom defines window.scrollTo but throws "Not implemented" when it is called. The router
// scrolls to the top on every navigation, so without this every route test prints a stack
// trace -- noise that trains the reader to ignore stderr, which is where real failures go.
if (typeof window !== 'undefined') {
  window.scrollTo = (() => {}) as typeof window.scrollTo
}

// jsdom implements no layout, so Element.prototype.scrollIntoView is simply absent. The
// Explorer's drill-down panel calls it on selection, so without this any test that clicks a
// result row dies on a missing function rather than on anything it was written to check.
if (typeof Element !== 'undefined' && typeof Element.prototype.scrollIntoView !== 'function') {
  Element.prototype.scrollIntoView = () => {}
}
