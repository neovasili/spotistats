/**
 * Period selector: all time, a year, or a month within that year.
 *
 * The API accepts ALL, yyyy and yyyy-mm (docs/SPECS.md 5.2), so the control is built from those
 * three shapes directly rather than offering a free-text field that can produce a 400.
 */
interface Props {
  value: string
  onChange: (period: string) => void
}

/** Years the data can plausibly cover. Spotify's API retains ~50 plays, so history before the
 *  first capture only exists once the GDPR export is imported; a fixed floor is honest and
 *  cheap, and an empty year simply reads as zero. */
const FIRST_YEAR = 2015

export function PeriodPicker({ value, onChange }: Props) {
  const now = new Date()
  const thisYear = now.getUTCFullYear()
  const years: number[] = []
  for (let y = thisYear; y >= FIRST_YEAR; y--) years.push(y)

  const isAll = value === 'ALL'
  const year = isAll ? '' : value.slice(0, 4)
  const month = value.length === 7 ? value.slice(5, 7) : ''

  return (
    <>
      <label className="field">
        <span className="field__label">Year</span>
        <select
          className="field__input"
          value={isAll ? 'ALL' : year}
          onChange={(e) => onChange(e.target.value === 'ALL' ? 'ALL' : e.target.value)}
        >
          <option value="ALL">All time</option>
          {years.map((y) => (
            <option key={y} value={String(y)}>
              {y}
            </option>
          ))}
        </select>
      </label>

      <label className="field">
        <span className="field__label">Month</span>
        <select
          className="field__input"
          value={month}
          // Disabled rather than hidden: a month is meaningless without a year, and a control
          // that vanishes is harder to understand than one that is visibly unavailable.
          disabled={isAll}
          onChange={(e) => onChange(e.target.value === '' ? year : `${year}-${e.target.value}`)}
        >
          <option value="">Whole year</option>
          {MONTHS.map((m, i) => (
            <option key={m} value={String(i + 1).padStart(2, '0')}>
              {m}
            </option>
          ))}
        </select>
      </label>
    </>
  )
}

const MONTHS = [
  'January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December',
]
