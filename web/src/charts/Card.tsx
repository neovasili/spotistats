import { useId, useState, type ReactNode } from 'react'

interface CardProps {
  title: string
  subtitle?: string
  /**
   * The table view. Every chart has one: it is the accessibility fallback and the relief for
   * any mark whose contrast against the surface falls below 3:1.
   *
   * Omit it ONLY when the card draws no chart at all -- the "this data cannot exist" state,
   * where the body is already a plain sentence and a table view would offer an empty table as
   * an alternative to nothing. The invariant is "every chart has a table", and a card with no
   * chart does not weaken it. Omitting it hides the toggle.
   */
  table?: ReactNode
  /**
   * Renders a close button in the header when supplied.
   *
   * Optional because most cards are permanent furniture; only a card the reader OPENED can be
   * meaningfully dismissed, and offering to close one they cannot reopen would be a trap.
   */
  onClose?: () => void
  children: ReactNode
}

/**
 * A chart container with a chart/table toggle.
 *
 * The toggle is not a nicety. Identity and value must be available without relying on colour, so
 * every chart here ships a tabular equivalent rather than only a visual one.
 */
export function Card({ title, subtitle, table, onClose, children }: CardProps) {
  const [showTable, setShowTable] = useState(false)
  const bodyId = useId()

  return (
    <section className="card">
      <div className="card__head">
        <div>
          <h2 className="card__title">{title}</h2>
          {subtitle && <p className="card__sub">{subtitle}</p>}
        </div>
        <div className="card__actions">
          {table && (
            <button
              type="button"
              className="ghost-button card__toggle"
              aria-expanded={showTable}
              aria-controls={bodyId}
              onClick={() => setShowTable((v) => !v)}
            >
              {showTable ? 'Chart' : 'Table'}
            </button>
          )}
          {onClose && (
            <button
              type="button"
              className="ghost-button card__close"
              // A glyph needs a label: "×" is announced as "times" or skipped entirely.
              aria-label={`Close ${title}`}
              onClick={onClose}
            >
              ×
            </button>
          )}
        </div>
      </div>
      <div id={bodyId} className="card__body">
        {table && showTable ? table : children}
      </div>
    </section>
  )
}
