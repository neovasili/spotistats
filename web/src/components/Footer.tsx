import type { Dashboard } from '../lib/types'
import { formatDate, formatDateTime } from '../lib/format'

/**
 * Provenance and caveats.
 *
 * These are not fine print. The dataset has real limits -- estimated durations, no podcasts,
 * genre labels that do not partition -- and the honest place for them is on the page rather than
 * in a document nobody reads. The notes come from the snapshot, so the backend decides what needs
 * disclosing and the frontend cannot quietly drop one.
 */
export function Footer({ data }: { data: Dashboard }) {
  const { coverage, notes, generatedAt, timezone } = data
  return (
    <footer className="footer">
      <dl className="footer__meta">
        <div>
          <dt>Covers</dt>
          <dd>
            {formatDate(coverage.firstPlayedAt)} — {formatDate(coverage.lastPlayedAt)}
            {coverage.approximate && (
              <span
                className="footer__flag"
                title="These bounds come from write-time attributes rather than a full pass over history, so they are approximate until the next nightly reconcile."
              >
                approximate
              </span>
            )}
          </dd>
        </div>
        <div>
          <dt>Updated</dt>
          <dd>{formatDateTime(generatedAt)}</dd>
        </div>
        <div>
          <dt>Timezone</dt>
          <dd>{timezone}</dd>
        </div>
      </dl>

      {notes.length > 0 && (
        <ul className="footer__notes">
          {notes.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      )}
    </footer>
  )
}
