/**
 * Period selector: all time, a year, or a month within that year.
 *
 * The API accepts ALL, yyyy and yyyy-mm (docs/SPECS.md 5.2), so the control is built from those
 * three shapes directly rather than offering a free-text field that can produce a 400.
 */
interface Props {
  value: string
  onChange: (period: string) => void
  /**
   * The earliest year the stored history covers, from /meta's coverage window.
   *
   * This used to be a hardcoded 2015, justified in a comment as "honest and cheap" on the
   * grounds that Spotify's API retains only ~50 plays. That reasoning expired the moment the
   * GDPR export was imported: the data reached back to 2009 and six years of it were
   * unreachable from this control, with nothing on screen to suggest the list was the limiting
   * factor rather than the data.
   *
   * Undefined while /meta is in flight, or if it fails. The fallback covers only the API-capture
   * era, because that is the one span this app can be sure exists without asking.
   */
  firstYear?: number
}

/** Used until /meta answers. Deliberately NOT a guess at the history's true start: a floor that
 *  is too low shows empty years, which is harmless, while one that is too high hides real data,
 *  which is the bug this replaced. */
const FALLBACK_FIRST_YEAR = 2015

export function PeriodPicker({ value, onChange, firstYear }: Props) {
  const now = new Date()
  const thisYear = now.getUTCFullYear()
  // Clamp: a bad coverage value must not generate thousands of options.
  const floor = Math.min(
    thisYear,
    Math.max(1970, firstYear ?? FALLBACK_FIRST_YEAR),
  )
  const years: number[] = []
  for (let y = thisYear; y >= floor; y--) years.push(y)

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
