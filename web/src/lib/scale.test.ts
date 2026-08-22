import { describe, expect, it } from 'vitest'
import { fraction, maxOf, sequentialColor, toWeeks } from './scale'

describe('fraction', () => {
  it('clamps and never divides by zero', () => {
    // An empty dataset gives a maximum of 0; returning NaN would collapse every bar's width.
    expect(fraction(5, 0)).toBe(0)
    expect(fraction(5, Number.NaN)).toBe(0)
    expect(fraction(5, 10)).toBe(0.5)
    expect(fraction(20, 10)).toBe(1)
    expect(fraction(-5, 10)).toBe(0)
  })
})

describe('maxOf', () => {
  it('handles an empty series and ignores non-finite values', () => {
    expect(maxOf([], (n: number) => n)).toBe(0)
    expect(maxOf([1, 5, 3], (n) => n)).toBe(5)
    expect(maxOf([Number.NaN, 2], (n) => n)).toBe(2)
  })
})

describe('sequentialColor', () => {
  it('distinguishes nothing from a little', () => {
    // A day with no listening must read as absent, not merely faint, or the heatmap implies
    // activity where there was none.
    expect(sequentialColor(0, 100)).toBe('var(--surface-2)')
    expect(sequentialColor(1, 100)).not.toBe('var(--surface-2)')
  })

  it('is monotonic: more is darker', () => {
    const steps = [1, 25, 50, 75, 100].map((v) => sequentialColor(v, 100))
    expect(new Set(steps).size).toBeGreaterThan(1)
    // The largest value must reach the darkest step in the ramp.
    expect(steps[steps.length - 1]).toBe('var(--blue-600)')
  })
})

describe('toWeeks', () => {
  it('pads the first week so weekdays line up in rows', () => {
    // 2026-01-01 is a Thursday (UTC day 4), so the first column needs four empty cells above it.
    // Without the padding the whole grid shears and "Mondays are busy" becomes unreadable.
    const days = Array.from({ length: 10 }, (_, i) => ({
      date: `2026-01-${String(i + 1).padStart(2, '0')}`,
    }))
    const weeks = toWeeks(days)

    expect(weeks[0]?.slice(0, 4)).toEqual([null, null, null, null])
    expect(weeks[0]?.[4]).toEqual({ date: '2026-01-01' })
    for (const w of weeks) expect(w).toHaveLength(7)
  })

  it('returns nothing for an empty series', () => {
    expect(toWeeks([])).toEqual([])
  })

  it('pads the final week too, so every column is full height', () => {
    const days = Array.from({ length: 3 }, (_, i) => ({
      date: `2026-03-${String(i + 1).padStart(2, '0')}`,
    }))
    const weeks = toWeeks(days)
    expect(weeks.every((w) => w.length === 7)).toBe(true)
  })
})
