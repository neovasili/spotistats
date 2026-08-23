import { describe, expect, it } from 'vitest'
import { toCsv } from './csv'
import type { ListItem } from '../lib/api'

function item(name: string, over: Partial<ListItem> = {}): ListItem {
  return {
    rank: 1,
    id: 't1',
    name,
    metrics: { plays: 3, playsExact: 0, msPlayed: 600_000, msPlayedExact: 0, estimatedRatio: 1 },
    firstPlayedAt: '2026-01-01T00:00:00.000Z',
    lastPlayedAt: '2026-08-01T00:00:00.000Z',
    ...over,
  }
}

describe('toCsv', () => {
  it('exports raw milliseconds alongside minutes', () => {
    // A spreadsheet needs the exact integer to sum; "10.0" minutes has already lost precision.
    const csv = toCsv([item('Song')])
    expect(csv).toContain('600000')
    expect(csv).toContain('10.0')
  })

  it('quotes fields containing commas, quotes or newlines', () => {
    // Track and artist names routinely contain these; an unquoted comma shifts every later
    // column silently.
    expect(toCsv([item('Hello, Goodbye')])).toContain('"Hello, Goodbye"')
    expect(toCsv([item('Say "Yes"')])).toContain('"Say ""Yes"""')
    expect(toCsv([item('Two\r\nLines')])).toContain('"Two\r\nLines"')
  })

  it('neutralises names a spreadsheet would evaluate as a formula', () => {
    // A name starting with = , + , - or @ is executed by Excel/Sheets on open. Prefixing a tab
    // keeps it inert and visible rather than becoming a live formula or an error cell.
    for (const lead of ['=', '+', '-', '@']) {
      const csv = toCsv([item(`${lead}cmd`)])
      expect(csv).not.toContain(`,${lead}cmd`)
      expect(csv).toContain('\t' + lead + 'cmd')
    }
  })

  it('emits a header row and CRLF line endings', () => {
    const csv = toCsv([item('A'), item('B', { rank: 2 })])
    const lines = csv.split('\r\n')
    expect(lines[0]).toMatch(/^rank,id,name/)
    expect(lines).toHaveLength(3)
  })

  it('handles an empty result set without producing a bare header-less file', () => {
    expect(toCsv([])).toBe('rank,id,name,plays,msPlayed,minutes,firstPlayedAt,lastPlayedAt')
  })
})
