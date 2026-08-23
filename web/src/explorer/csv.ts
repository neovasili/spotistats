import type { ListItem } from '../lib/api'

/**
 * CSV export of the current result set (docs/SPECS.md 7.2).
 *
 * Milliseconds are exported alongside the human-readable minutes: a spreadsheet needs the exact
 * integer to sum, and "1h 24m" is not a number. Only the rows actually loaded are exported, and
 * the caller names the file after the query so several exports stay distinguishable.
 */
export function toCsv(items: ListItem[]): string {
  const head = [
    'rank', 'id', 'name', 'artist', 'album',
    'plays', 'msPlayed', 'minutes', 'firstPlayedAt', 'lastPlayedAt',
  ]
  const rows = items.map((i) => [
    String(i.rank),
    i.id,
    i.name,
    // Own columns rather than one joined label: a spreadsheet groups and filters on these.
    i.artistName ?? '',
    i.albumName ?? '',
    String(i.metrics.plays),
    String(i.metrics.msPlayed),
    (i.metrics.msPlayed / 60000).toFixed(1),
    i.firstPlayedAt ?? '',
    i.lastPlayedAt ?? '',
  ])
  return [head, ...rows].map((r) => r.map(escapeField).join(',')).join('\r\n')
}

/**
 * RFC 4180 quoting. Track and artist names routinely contain commas and quotes, and a name
 * beginning with =, +, - or @ is treated as a formula by spreadsheet applications, so those are
 * prefixed with a tab to keep them inert.
 */
function escapeField(value: string): string {
  const safe = /^[=+\-@\t\r]/.test(value) ? `\t${value}` : value
  return /[",\r\n\t]/.test(safe) ? `"${safe.replace(/"/g, '""')}"` : safe
}

export function downloadCsv(filename: string, items: ListItem[]): void {
  // The BOM makes Excel read it as UTF-8; without it, accented artist names arrive mangled.
  const blob = new Blob(['﻿', toCsv(items)], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
