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

/**
 * The same duration expressed purely in minutes, e.g. "5,051m".
 *
 * Shown alongside formatDuration everywhere because the two answer different questions: "3d 11h"
 * is how long it FEELS, and minutes is what you can compare, sum or paste into a spreadsheet.
 */
export function formatMinutes(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0m'
  return `${Math.round(ms / 60_000).toLocaleString()}m`
}

/**
 * Both renderings in one string, for titles, tooltips and aria labels where markup is not an
 * option: "3d 11h (5,051m)".
 *
 * Under an hour the primary rendering is ALREADY minutes, so the suffix would just repeat it.
 */
export function formatDurationFull(ms: number): string {
  const primary = formatDuration(ms)
  const minutes = formatMinutes(ms)
  return primary === minutes ? primary : `${primary} (${minutes})`
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

/**
 * A variable-precision date, rendered at exactly the precision it was stored at.
 *
 * MusicBrainz records life-spans and memberships as `2008`, `2008-04` or `2008-04-17`, and the
 * shorter forms are a real claim about what is known -- not a truncation. So `2008` renders as
 * "2008" and `2008-04` as "April 2008"; neither invents a day the source does not assert. Only
 * the full form gets one.
 *
 * The precision is read from the string's length rather than a companion field, because member
 * dates carry no precision of their own and the shape is unambiguous.
 */
export function formatPrecisionDate(value: string | null | undefined): string {
  if (!value) return '—'
  const [y, m, d] = value.split('-')
  if (!y || !/^\d{4}$/.test(y)) return value // not a shape we recognise: show it verbatim
  if (!m) return y

  const month = Number(m)
  if (!Number.isInteger(month) || month < 1 || month > 12) return value
  // Day 1 is a placeholder for constructing the date; it is never rendered for month precision.
  const at = new Date(Date.UTC(Number(y), month - 1, d ? Number(d) : 1))
  if (Number.isNaN(at.getTime())) return value

  return d
    ? at.toLocaleDateString(undefined, { year: 'numeric', month: 'long', day: 'numeric', timeZone: 'UTC' })
    : at.toLocaleDateString(undefined, { year: 'numeric', month: 'long', timeZone: 'UTC' })
}

/**
 * The sentence to show when some of a total's listening time is estimated rather than measured.
 *
 * Returns undefined at zero, so the caveat disappears rather than becoming a permanent
 * disclaimer. A ratio that rounds down to 0% gets words instead of the figure: "about 0% is
 * estimated" reads as a bug, and it is the one phrasing that tells the reader nothing.
 */
export function estimatedCaveat(ratio: number): string | undefined {
  if (!(ratio > 0)) return undefined
  if (ratio >= 0.999) {
    return (
      'All of this listening time is estimated: the recently-played endpoint reports no ' +
      'duration, so each play counts the track’s full length and skips are over-counted.'
    )
  }
  const pct = Math.round(ratio * 100)
  return pct === 0
    ? 'Under 1% of this listening time is estimated rather than exact.'
    : `About ${pct}% of this listening time is estimated rather than exact.`
}
