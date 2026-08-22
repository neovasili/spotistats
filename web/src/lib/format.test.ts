import { describe, expect, it } from 'vitest'
import {
  formatDate, formatDuration, formatHours, formatNumber, formatPercent, hourLabel, weekdayName,
} from './format'

describe('formatDuration', () => {
  it('scales the unit to the magnitude', () => {
    // Listening totals span six orders of magnitude, so a fixed unit is unreadable at one end.
    expect(formatDuration(0)).toBe('0m')
    expect(formatDuration(59_000)).toBe('0m')
    expect(formatDuration(90_000)).toBe('1m')
    expect(formatDuration(59 * 60_000)).toBe('59m')
    expect(formatDuration(60 * 60_000)).toBe('1h')
    expect(formatDuration(90 * 60_000)).toBe('1h 30m')
    expect(formatDuration(48 * 3_600_000)).toBe('2d')
    expect(formatDuration(50 * 3_600_000)).toBe('2d 2h')
  })

  it('does not produce NaN or negatives for bad input', () => {
    expect(formatDuration(-1)).toBe('0m')
    expect(formatDuration(Number.NaN)).toBe('0m')
    expect(formatDuration(Number.POSITIVE_INFINITY)).toBe('0m')
  })
})

describe('formatPercent', () => {
  it('rounds to whole points', () => {
    // More precision than this is false confidence: the ratio derives from estimated durations.
    expect(formatPercent(0)).toBe('0%')
    expect(formatPercent(0.3979)).toBe('40%')
    expect(formatPercent(1)).toBe('100%')
  })
})

describe('formatDate', () => {
  it('renders a placeholder rather than throwing on missing or invalid input', () => {
    // The coverage window is null before any play is recorded.
    expect(formatDate(null)).toBe('—')
    expect(formatDate(undefined)).toBe('—')
    expect(formatDate('not a date')).toBe('—')
  })

  it('parses the timestamp format the backend emits', () => {
    expect(formatDate('2026-02-10T13:00:00.000Z')).not.toBe('—')
  })
})

describe('bucket labels', () => {
  it('treats weekday 0 as Sunday, matching the backend', () => {
    // Go's time.Weekday starts at Sunday; an off-by-one here would mislabel every bar.
    expect(weekdayName(0)).toBe('Sun')
    expect(weekdayName(6)).toBe('Sat')
    expect(weekdayName(99)).toBe('99')
  })

  it('zero-pads hours', () => {
    expect(hourLabel(0)).toBe('00:00')
    expect(hourLabel(9)).toBe('09:00')
    expect(hourLabel(23)).toBe('23:00')
  })
})

describe('formatHours / formatNumber', () => {
  it('rounds hours and groups thousands', () => {
    expect(formatHours(3_600_000)).toBe('1')
    expect(formatHours(5_400_000)).toBe('2')
    expect(formatNumber(1234)).toBe((1234).toLocaleString())
  })
})
