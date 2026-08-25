import { useEffect, useId, useMemo, useRef, useState } from 'react'
import type { GenreMatch } from '../lib/api'
import { formatDuration } from '../lib/format'

export interface GenreOption {
  name: string
  msPlayed: number
}

interface Props {
  options: GenreOption[]
  selected: string[]
  onChange: (next: string[]) => void
  match: GenreMatch
  onMatchChange: (next: GenreMatch) => void
  /** Set while the vocabulary is still loading, so the control can say so rather than look empty. */
  loading?: boolean
}

/**
 * The genre filter: a searchable multi-select over the tags this library actually uses.
 *
 * # Why a popover and not a row of checkboxes
 *
 * There are 350 genres. Anything that renders them all inline is the whole page, and a plain
 * `<select multiple>` shows four rows at a time and requires ctrl-click to add a second value --
 * a gesture most people never discover and no touch device has. So: a button showing the current
 * selection, a panel with a search box, and checkboxes inside it.
 *
 * # Why search, and why it is the first thing focused
 *
 * With 350 tags ordered by listening time, anything outside the top twenty is unreachable by
 * scrolling in any reasonable time. Typing is how a reader finds "power metal", so the box takes
 * focus the moment the panel opens.
 *
 * # Ordering
 *
 * By listening time, descending -- never alphabetically. The tags a reader recognises as theirs
 * are the ones they listened to most, and those must be the ones visible before any typing.
 * Selected tags are pulled to the top so the current filter stays readable while searching for
 * the next addition.
 */
export function GenrePicker({
  options,
  selected,
  onChange,
  match,
  onMatchChange,
  loading,
}: Props) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const rootRef = useRef<HTMLDivElement | null>(null)
  const searchRef = useRef<HTMLInputElement | null>(null)
  const panelId = useId()

  // Click-outside and Escape both close it. A popover that can only be dismissed by finding the
  // button again is a popover people leave open, which then covers the list they opened it for.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    const onOutside = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false)
    }
    window.addEventListener('keydown', onKey)
    document.addEventListener('mousedown', onOutside)
    return () => {
      window.removeEventListener('keydown', onKey)
      document.removeEventListener('mousedown', onOutside)
    }
  }, [open])

  // The search box is the point of the panel, so it starts focused.
  useEffect(() => {
    if (open) searchRef.current?.focus()
  }, [open])

  const chosen = useMemo(() => new Set(selected), [selected])

  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase()
    const matches = needle ? options.filter((o) => o.name.includes(needle)) : options
    // Selected first, then by listening time (the order `options` already arrives in).
    return [...matches].sort((a, b) => Number(chosen.has(b.name)) - Number(chosen.has(a.name)))
  }, [options, query, chosen])

  const toggle = (name: string) => {
    onChange(chosen.has(name) ? selected.filter((g) => g !== name) : [...selected, name])
  }

  return (
    <div className="field genrepicker" ref={rootRef}>
      <span className="field__label">Genres</span>
      <button
        type="button"
        className="field__input genrepicker__button"
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="genrepicker__summary">{summarise(selected, loading)}</span>
        <span className="genrepicker__caret" aria-hidden="true">
          ▾
        </span>
      </button>

      {open && (
        <div className="genrepicker__panel" id={panelId}>
          <input
            ref={searchRef}
            type="search"
            className="field__input genrepicker__search"
            value={query}
            placeholder="find a genre…"
            aria-label="Search genres"
            onChange={(e) => setQuery(e.target.value)}
          />

          {/*
            The any/all rule lives inside the panel rather than in the filter row. It only means
            anything once two genres are picked, and the row already carries four controls -- a
            fifth that is inert most of the time is clutter that has to be read every visit.
          */}
          {selected.length > 1 && (
            <div className="genrepicker__match" role="group" aria-label="Match rule">
              {(['any', 'all'] as GenreMatch[]).map((m) => (
                <button
                  key={m}
                  type="button"
                  className="segmented__item"
                  aria-pressed={match === m}
                  onClick={() => onMatchChange(m)}
                >
                  {m === 'any' ? 'Any of these' : 'All of these'}
                </button>
              ))}
            </div>
          )}

          <div className="genrepicker__list">
            {visible.length === 0 && (
              <p className="empty genrepicker__empty">
                {loading ? 'Loading genres…' : 'No genre matches that.'}
              </p>
            )}
            {visible.map((o) => (
              <label key={o.name} className="genrepicker__option">
                <input
                  type="checkbox"
                  checked={chosen.has(o.name)}
                  onChange={() => toggle(o.name)}
                />
                <span className="genrepicker__name">{o.name}</span>
                {/* The magnitude, so a reader can tell a defining tag from a stray one. */}
                <span className="genrepicker__ms">{formatDuration(o.msPlayed)}</span>
              </label>
            ))}
          </div>

          {selected.length > 0 && (
            <div className="genrepicker__foot">
              <button type="button" className="ghost-button" onClick={() => onChange([])}>
                Clear {selected.length}
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * What the closed button says.
 *
 * Names the genres while they fit, because "2 genres" forces the panel open again to remember
 * which two. Past three it stops fitting in a filter row, so it counts instead.
 */
function summarise(selected: string[], loading?: boolean): string {
  if (loading && selected.length === 0) return 'Loading…'
  if (selected.length === 0) return 'Any genre'
  if (selected.length <= 3) return selected.join(', ')
  return `${selected.length} genres`
}
