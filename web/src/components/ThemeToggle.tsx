import { useEffect, useState } from 'react'

type Theme = 'system' | 'light' | 'dark'
const KEY = 'spotistats:theme'

/**
 * Light/dark/system toggle.
 *
 * Dark mode is a selected set of steps rather than an inversion, so this only sets the scope the
 * tokens key off. `system` removes the attribute entirely so the OS preference applies.
 */
export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(() => {
    const stored = localStorage.getItem(KEY)
    return stored === 'light' || stored === 'dark' ? stored : 'system'
  })

  useEffect(() => {
    const root = document.documentElement
    if (theme === 'system') {
      root.removeAttribute('data-theme')
      localStorage.removeItem(KEY)
    } else {
      root.setAttribute('data-theme', theme)
      localStorage.setItem(KEY, theme)
    }
  }, [theme])

  const next: Record<Theme, Theme> = { system: 'light', light: 'dark', dark: 'system' }
  const icon: Record<Theme, string> = { system: 'Auto', light: 'Light', dark: 'Dark' }

  return (
    <button
      type="button"
      className="ghost-button"
      onClick={() => setTheme(next[theme])}
      aria-label={`Theme: ${icon[theme]}. Click to change.`}
    >
      {icon[theme]}
    </button>
  )
}
