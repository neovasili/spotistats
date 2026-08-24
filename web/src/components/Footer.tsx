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
  // Only claim a MusicBrainz credit when MusicBrainz data is actually on the page.
  const genresAttributed = data.genresAvailable && data.top.genres.length > 0
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

      {/*
        Not decoration, and not one obligation but two.

        Spotify's Developer Policy requires cover art and metadata to be attributed to Spotify,
        alongside the per-entity link-back that every artwork and name already carries.

        MusicBrainz genre data is licensed CC-BY-NC-SA 3.0 -- only their CORE data is CC0, and
        tags, which genres derive from, are explicitly not. So the moment genres began feeding
        the genre chart rather than only the artist profile, the credit had to appear HERE too:
        attribution follows the data onto whatever page displays it. It is rendered only when
        there are genres to attribute, so it cannot become a claim about data that is absent.
      */}
      <p className="footer__attribution">
        Metadata and cover art from{' '}
        <a href="https://spotify.com" target="_blank" rel="noopener noreferrer">
          Spotify
        </a>
        . Artwork and names link back to Spotify.
        {genresAttributed && (
          <>
            {' '}Genres from{' '}
            <a href="https://musicbrainz.org" target="_blank" rel="noopener noreferrer">
              MusicBrainz
            </a>
            , licensed{' '}
            <a
              href="https://creativecommons.org/licenses/by-nc-sa/3.0/"
              target="_blank"
              rel="noopener noreferrer"
            >
              CC BY-NC-SA 3.0
            </a>
            .
          </>
        )}
      </p>
    </footer>
  )
}
