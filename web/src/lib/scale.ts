/**
 * Scales for the charts.
 *
 * Hand-rolled rather than pulled from a charting library: the visual specification pins exact
 * mark geometry -- 4px rounded ends anchored to the baseline, a 2px surface gap between adjacent
 * fills, recessive grid lines -- and most libraries fight that harder than these few functions
 * cost to write.
 */

/** Maps a value onto [0, 1] against a maximum. A zero maximum yields 0, not NaN. */
export function fraction(value: number, max: number): number {
  if (!Number.isFinite(max) || max <= 0) return 0
  return Math.min(1, Math.max(0, value / max))
}

/**
 * Picks a step from the sequential ramp for a value.
 *
 * The lightest steps are reserved for "near zero", where receding toward the surface is the
 * correct reading. Anything with data starts at step 200 so it stays visible.
 */
const SEQUENTIAL_STEPS = [
  'var(--blue-150)',
  'var(--blue-200)',
  'var(--blue-300)',
  'var(--blue-400)',
  'var(--blue-500)',
  'var(--blue-600)',
] as const

export function sequentialColor(value: number, max: number): string {
  if (value <= 0) return 'var(--surface-2)'
  const f = fraction(value, max)
  const idx = Math.min(SEQUENTIAL_STEPS.length - 1, Math.floor(f * SEQUENTIAL_STEPS.length))
  return SEQUENTIAL_STEPS[idx] ?? SEQUENTIAL_STEPS[0]
}

/** The maximum of a series, or 0 for an empty one. */
export function maxOf<T>(items: readonly T[], pick: (item: T) => number): number {
  let max = 0
  for (const it of items) {
    const v = pick(it)
    if (Number.isFinite(v) && v > max) max = v
  }
  return max
}

/**
 * Groups days into calendar weeks for the heatmap.
 *
 * Columns are weeks and rows are weekdays, so the grid is padded at the start to align the first
 * day to its weekday. Without that the whole grid is sheared by a few days and reading "Mondays
 * are busy" off it becomes impossible.
 *
 * Weeks start on MONDAY, so a heatmap column covers exactly the days the "this week" tile counts.
 * They used to start on Sunday -- `getUTCDay()` used raw -- which meant the rightmost column and
 * the tile beside it disagreed about which days belonged to this week, by one day, silently.
 */
export function toWeeks<T extends { date: string }>(days: readonly T[]): (T | null)[][] {
  if (days.length === 0) return []

  const first = days[0]
  if (!first) return []
  // getUTCDay() is 0 = Sunday; this is days since the preceding Monday.
  const firstDow = (new Date(`${first.date}T00:00:00Z`).getUTCDay() + 6) % 7

  const cells: (T | null)[] = Array.from({ length: firstDow }, () => null)
  cells.push(...days)
  while (cells.length % 7 !== 0) cells.push(null)

  const weeks: (T | null)[][] = []
  for (let i = 0; i < cells.length; i += 7) {
    weeks.push(cells.slice(i, i + 7))
  }
  return weeks
}
