import { useId, useState, type ReactNode } from 'react'

interface CardProps {
  title: string
  subtitle?: string
  /** The table view. Every chart has one: it is the accessibility fallback and the relief for
   *  any mark whose contrast against the surface falls below 3:1. */
  table: ReactNode
  children: ReactNode
}

/**
 * A chart container with a chart/table toggle.
 *
 * The toggle is not a nicety. Identity and value must be available without relying on colour, so
 * every chart here ships a tabular equivalent rather than only a visual one.
 */
export function Card({ title, subtitle, table, children }: CardProps) {
  const [showTable, setShowTable] = useState(false)
  const bodyId = useId()

  return (
    <section className="card">
      <div className="card__head">
        <div>
          <h2 className="card__title">{title}</h2>
          {subtitle && <p className="card__sub">{subtitle}</p>}
        </div>
        <button
          type="button"
          className="ghost-button card__toggle"
          aria-expanded={showTable}
          aria-controls={bodyId}
          onClick={() => setShowTable((v) => !v)}
        >
          {showTable ? 'Chart' : 'Table'}
        </button>
      </div>
      <div id={bodyId}>{showTable ? table : children}</div>
    </section>
  )
}
