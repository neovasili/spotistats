import { describe, expect, it } from 'vitest'
import { estimatedCaveat, formatDate, formatDuration, formatDurationFull, formatHours, formatMinutes, formatNumber, formatPercent, formatPrecisionDate, hourLabel, weekdayName } from './format'

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

describe('formatMinutes', () => {
  it('states any duration purely in minutes, with thousands separators', () => {
    // The point of showing this alongside formatDuration: "3d 11h" cannot be compared or summed,
    // a minute count can.
    expect(formatMinutes(300_000)).toBe('5m')
    expect(formatMinutes(3_600_000)).toBe('60m')
    expect(formatMinutes(303_060_000)).toBe(formatMinutes(303_060_000))
    expect(formatMinutes(303_060_000).endsWith('m')).toBe(true)
    expect(formatMinutes(303_060_000).replace(/\D/g, '')).toBe('5051')
  })

  it('rounds rather than truncating, so a near-minute is not lost', () => {
    expect(formatMinutes(59_000)).toBe('1m')
  })

  it('treats absent or nonsensical input as zero', () => {
    for (const v of [0, -1, NaN, Infinity]) {
      expect(formatMinutes(v)).toBe('0m')
    }
  })
})

describe('formatDurationFull', () => {
  it('gives both renderings for anything an hour or longer', () => {
    expect(formatDurationFull(3_660_000)).toBe('1h 1m (61m)')
  })

  it('does not repeat itself under an hour', () => {
    // The readable form IS the minutes there, so "(45m)" would just say it twice.
    expect(formatDurationFull(2_700_000)).toBe('45m')
    expect(formatDurationFull(0)).toBe('0m')
  })
})

describe('formatPrecisionDate', () => {
  it('never invents precision the source does not claim', () => {
    // A stored "2008" means the year is known and the month is not. Rendering "1 January 2008"
    // would assert two facts MusicBrainz never stated.
    expect(formatPrecisionDate('2008')).toBe('2008')
    expect(formatPrecisionDate('2008-04')).toBe('April 2008')
    expect(formatPrecisionDate('2008-04-17')).toBe('April 17, 2008')
  })

  it('passes through anything it does not recognise', () => {
    // Better a verbatim oddity than a confidently wrong date.
    expect(formatPrecisionDate('circa 1970')).toBe('circa 1970')
    expect(formatPrecisionDate('2008-13')).toBe('2008-13')
    expect(formatPrecisionDate('')).toBe('—')
    expect(formatPrecisionDate(undefined)).toBe('—')
  })

  it('does not shift a date across a timezone boundary', () => {
    // Constructed in UTC and formatted in UTC. Parsed as local instead, a 1 January would
    // render as 31 December for any reader west of Greenwich.
    expect(formatPrecisionDate('2008-01-01')).toBe('January 1, 2008')
  })
})

describe('estimatedCaveat', () => {
  it('says nothing when nothing is estimated', () => {
    expect(estimatedCaveat(0)).toBeUndefined()
  })

  it('uses words, not "0%", for a ratio that rounds to zero', () => {
    // "About 0% of this listening time is estimated" reads as a bug and tells the reader
    // nothing, which is worse than either the truth or silence.
    expect(estimatedCaveat(0.0004)).toBe('Under 1% of this listening time is estimated rather than exact.')
    expect(estimatedCaveat(0.004)).toBe('Under 1% of this listening time is estimated rather than exact.')
  })

  it('reports a real share as a percentage', () => {
    expect(estimatedCaveat(0.27)).toContain('About 27%')
  })

  it('has separate wording for entirely estimated totals', () => {
    // "About 100% estimated" invites the reader to wonder about the other 0%.
    expect(estimatedCaveat(1)).toContain('All of this listening time is estimated')
  })
})
