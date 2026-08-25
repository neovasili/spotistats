import { formatDurationFull, formatNumber } from '../lib/format'
import { fraction, maxOf, sequentialColor } from '../lib/scale'
import { useTooltip, Tooltip } from './Tooltip'

/** One point of a time series: a period key and its figures. */
export interface TrendPoint {
  period: string
  plays: number
  msPlayed: number
}

interface Props {
  points: TrendPoint[]
  /** Shown when there is nothing to draw. */
  emptyLabel: string
  /** The accessible description of the whole series. */
  ariaLabel: string
  /**
   * How a period key becomes a column label. `'2026-03'` reads better as `Mar` under a heading
   * that already names the year; `'2026'` is the whole label.
   */
  label?: (period: string) => string
}

/**
 * A vertical bar per period, for a time series.
 *
 * Vertical rather than horizontal because time is the axis: a reader scans left to right for
 * "when", where a ranked list is scanned top to bottom for "which". That is also why the columns
 * are NOT sorted by size — the order is chronological and rearranging it would destroy the only
 * thing the chart is for.
 *
 * Shared by the Explorer's drill-down (months of a year, or years of an entity's life) and the
 * dashboard's by-year card. It was private to EntityDetail and purely props-driven, so lifting it
 * here needed no refactor — but two things had to change on the way: the CSS hard-coded a
 * twelve-column grid, which a seventeen-year series does not fit, and it had only a native
 * `title` where every dashboard card has a real tooltip.
 */
export function Trend({ points, emptyLabel, ariaLabel, label }: Props) {
  const { containerRef, tip, marks } = useTooltip()
  const max = maxOf(points, (p) => p.msPlayed)

  if (max === 0) {
    return <p className="empty">{emptyLabel}</p>
  }

  return (
    <div className="chart-plot" ref={containerRef}>
      <div className="trend" role="img" aria-label={ariaLabel}>
        {points.map((p) => (
          <div key={p.period} className="trend__col">
            <div className="trend__track">
              <div
                className="trend__fill"
                style={{
                  height: `${Math.max(fraction(p.msPlayed, max) * 100, p.msPlayed > 0 ? 2 : 0)}%`,
                  // Sequential, as the heatmap is: the encoding is magnitude, so a taller bar
                  // being also a darker one reinforces rather than competes.
                  background: sequentialColor(p.msPlayed, max),
                }}
              />
            </div>
            <span
              className="trend__label"
              // The native title stays as the fallback, and is what a long series relies on when
              // the pointer is between columns.
              title={`${p.period}: ${formatDurationFull(p.msPlayed)}`}
              {...marks(
                <>
                  <div className="tooltip__label">{p.period}</div>
                  {p.msPlayed > 0 ? (
                    <>
                      <div className="tooltip__row">{formatDurationFull(p.msPlayed)}</div>
                      <div className="tooltip__row">{formatNumber(p.plays)} plays</div>
                    </>
                  ) : (
                    <div className="tooltip__row">nothing</div>
                  )}
                </>,
              )}
            >
              {label ? label(p.period) : p.period}
            </span>
          </div>
        ))}
      </div>
      <Tooltip tip={tip} />
    </div>
  )
}
