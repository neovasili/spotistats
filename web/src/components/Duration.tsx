import { formatDuration, formatMinutes } from '../lib/format'

/**
 * A duration shown twice: the readable form, and the same value in minutes beneath it.
 *
 * The two answer different questions. "3d 11h" is how long it feels; "5,051m" is what you can
 * compare across rows, sum in your head, or check against another tool. Neither alone is enough,
 * and picking one per context would make the dashboard inconsistent about which it meant.
 *
 * The minutes wear a muted text token and a smaller size, so they read as an annotation to the
 * figure rather than a second competing number.
 */
export function Duration({ ms }: { ms: number }) {
  const primary = formatDuration(ms)
  const minutes = formatMinutes(ms)
  // Under an hour the readable form IS the minutes, so the annotation would just repeat it.
  if (primary === minutes) {
    return <span className="duration">{primary}</span>
  }
  return (
    <span className="duration">
      {primary}
      <span className="duration__minutes">{minutes}</span>
    </span>
  )
}
