import { describe, expect, it } from 'vitest'
import { daysInWindow, weekdayOccurrences } from './window'

describe('daysInWindow', () => {
  it('counts both ends, because a single day is one day and not zero', () => {
    expect(daysInWindow('2026-03-01T00:00:00.000Z', '2026-03-01T23:00:00.000Z')).toBe(1)
    expect(daysInWindow('2026-01-01T00:00:00.000Z', '2026-04-10T00:00:00.000Z')).toBe(100)
  })

  it('ignores the time of day, so a late last play does not add a day', () => {
    const early = daysInWindow('2026-03-01T00:00:00.000Z', '2026-03-10T00:01:00.000Z')
    const late = daysInWindow('2026-03-01T23:59:00.000Z', '2026-03-10T23:59:00.000Z')
    expect(early).toBe(10)
    expect(late).toBe(10)
  })

  it('spans a daylight-saving change without dropping or inventing a day', () => {
    // Europe/Madrid springs forward on 2026-03-29. Local-clock arithmetic would return 89.958
    // days here and floor to 89; the UTC date arithmetic is exact.
    expect(daysInWindow('2026-02-01T00:00:00.000Z', '2026-04-30T00:00:00.000Z')).toBe(89)
  })

  it('counts a leap day', () => {
    expect(daysInWindow('2024-02-01T00:00:00.000Z', '2024-03-01T00:00:00.000Z')).toBe(30)
  })

  it('returns zero for a window that does not exist yet', () => {
    // An unpopulated coverage row looks exactly like this, and the callers turn a zero into no
    // figure at all rather than dividing by it.
    expect(daysInWindow(null, null)).toBe(0)
    expect(daysInWindow('2026-01-01T00:00:00.000Z', null)).toBe(0)
    expect(daysInWindow(undefined, '2026-01-01T00:00:00.000Z')).toBe(0)
    expect(daysInWindow('not a date', '2026-01-01T00:00:00.000Z')).toBe(0)
  })

  it('returns zero rather than a negative count when the bounds are inverted', () => {
    expect(daysInWindow('2026-04-10T00:00:00.000Z', '2026-01-01T00:00:00.000Z')).toBe(0)
  })
})

describe('weekdayOccurrences', () => {
  it('accounts for every day in the window', () => {
    // The invariant that matters: dividing each bucket by its own count must not lose or
    // double-count any of the period the buckets were accumulated over.
    const occ = weekdayOccurrences('2026-01-01T00:00:00.000Z', '2026-04-10T00:00:00.000Z')
    expect(occ).toHaveLength(7)
    expect(occ.reduce((a, b) => a + b, 0)).toBe(100)
  })

  it('gives the remainder to the weekdays the window actually starts on', () => {
    // 2026-01-01 was a Thursday and 100 days is 14 weeks plus two, so Thursday and Friday get
    // fifteen and the rest fourteen. Indexed 0 = Sunday, matching Go's time.Weekday.
    expect(weekdayOccurrences('2026-01-01T00:00:00.000Z', '2026-04-10T00:00:00.000Z'))
      .toEqual([14, 14, 14, 14, 15, 15, 14])
  })

  it('spreads a whole number of weeks evenly', () => {
    expect(weekdayOccurrences('2026-01-01T00:00:00.000Z', '2026-01-28T00:00:00.000Z'))
      .toEqual([4, 4, 4, 4, 4, 4, 4])
  })

  it('names a single day as its own weekday and nothing else', () => {
    // 2026-08-20 was a Thursday.
    expect(weekdayOccurrences('2026-08-20T00:00:00.000Z', '2026-08-20T00:00:00.000Z'))
      .toEqual([0, 0, 0, 0, 1, 0, 0])
  })

  it('is all zeroes for a window that does not exist yet', () => {
    expect(weekdayOccurrences(null, null)).toEqual([0, 0, 0, 0, 0, 0, 0])
  })
})
