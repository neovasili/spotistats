/**
 * Calendar arithmetic over the archive's coverage window.
 *
 * The rhythm charts show a TOTAL per bucket, and a total is unreadable as a habit: "4d 7h at
 * 20:00" says nothing until you know it is spread over seventeen years. These give the tooltips
 * their denominator.
 *
 * Every day in the window counts, including the silent ones, so the figure answers "a typical
 * day" rather than "a day you happened to listen". That is the honest reading of a bucket total
 * divided by the period it was accumulated over, and it needs nothing the snapshot does not
 * already carry.
 *
 * All arithmetic is on the date portion in UTC, so daylight saving never enters and the count is
 * exact. The buckets themselves are local days (Europe/Madrid), so the window's UTC edges can be
 * off by one day out of roughly six thousand -- which does not move an average, and is a better
 * trade than plumbing a timezone through two charts to chase it.
 */

/** The UTC midnight of an ISO instant's date portion, or NaN if it is not a date we recognise. */
function dayStart(iso?: string | null): number {
  if (!iso) return NaN
  const [y, m, d] = iso.slice(0, 10).split('-').map(Number)
  if (!y || !m || !d) return NaN
  return Date.UTC(y, m - 1, d)
}

const DAY_MS = 86_400_000

/**
 * How many calendar days the window spans, counting both ends.
 *
 * Zero when either bound is missing or malformed -- which is what an unpopulated coverage row
 * looks like, and the callers turn a zero into no figure at all rather than a division by it.
 */
export function daysInWindow(from?: string | null, to?: string | null): number {
  const a = dayStart(from)
  const b = dayStart(to)
  if (Number.isNaN(a) || Number.isNaN(b) || b < a) return 0
  return Math.round((b - a) / DAY_MS) + 1
}

/**
 * How many times each weekday falls inside the window, indexed 0 = Sunday to match the buckets
 * (and Go's `time.Weekday`, which is where the bucket numbering comes from). This is a lookup
 * keyed by bucket number, so it is unaffected by the Monday-first order charts display in.
 *
 * Closed form rather than a loop over six thousand days: every weekday occurs `floor(days / 7)`
 * times, and the remainder is claimed by the `days % 7` weekdays starting at the first day of
 * the window.
 */
export function weekdayOccurrences(from?: string | null, to?: string | null): number[] {
  const days = daysInWindow(from, to)
  const out = new Array<number>(7).fill(0)
  if (days === 0) return out

  const whole = Math.floor(days / 7)
  out.fill(whole)

  const first = new Date(dayStart(from)).getUTCDay()
  for (let i = 0; i < days % 7; i++) out[(first + i) % 7]!++
  return out
}
