/** Formatting helpers. Kept pure and separately tested: these are the numbers people read. */

/**
 * Renders a duration the way a person would say it.
 *
 * Listening totals span six orders of magnitude -- a single track is minutes, a lifetime is
 * months -- so a fixed unit is unreadable at one end or the other.
 */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0m'

  const minutes = Math.floor(ms / 60_000)
  if (minutes < 60) return `${minutes}m`

  const hours = Math.floor(minutes / 60)
  if (hours < 48) {
    const rem = minutes % 60
    return rem === 0 ? `${hours}h` : `${hours}h ${rem}m`
  }

  const days = Math.floor(hours / 24)
  const remHours = hours % 24
  return remHours === 0 ? `${days}d` : `${days}d ${remHours}h`
}

/** Whole hours, for a hero figure where a trailing "43m" is noise. */
export function formatHours(ms: number): string {
  return `${Math.round(ms / 3_600_000).toLocaleString()}`
}

export function formatNumber(n: number): string {
  return n.toLocaleString()
}

/** A percentage, rounded to whole points -- more precision than that is false confidence. */
export function formatPercent(fraction: number): string {
  return `${Math.round(fraction * 100)}%`
}

/** An ISO instant as a readable date. Invalid input passes through rather than throwing. */
export function formatDate(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString(undefined, {
    year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'] as const

/** Weekday buckets are 0 = Sunday, matching Go's time.Weekday. */
export function weekdayName(bucket: number): string {
  return WEEKDAYS[bucket] ?? String(bucket)
}

/** Hour buckets are local to the listener's timezone, not UTC. */
export function hourLabel(bucket: number): string {
  return `${String(bucket).padStart(2, '0')}:00`
}
