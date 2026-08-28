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

      {/*
        The credit line, last: the attributions above are obligations and this is not, so it
        does not get to precede them.

        aria-hidden on the heart, with the sentiment carried in the link text instead. A screen
        reader otherwise announces "made with red heart by neovasili", which is noise -- the
        emoji is decoration on a sentence that already reads correctly without it.
      */}
      <p className="footer__credit">
        <span>
          made with <span aria-hidden="true">❤️</span> by{' '}
          <a href="https://github.com/neovasili" target="_blank" rel="noopener noreferrer">
            neovasili
          </a>
        </span>
        <a
          className="footer__repo"
          href="https://github.com/neovasili/spotistats"
          target="_blank"
          rel="noopener noreferrer"
        >
          <GitHubMark />
          github
        </a>
      </p>
    </footer>
  )
}

/**
 * The GitHub mark, inline.
 *
 * Inline rather than an <img> or an icon dependency: it is one path, it must inherit the link's
 * colour in both themes (currentColor does that for free), and a separate request for 900 bytes
 * would be the only image the footer loads. aria-hidden because the link's own text says
 * "github" -- announcing the logo too would say it twice.
 */
function GitHubMark() {
  return (
    <svg
      className="footer__icon"
      viewBox="0 0 16 16"
      width="14"
      height="14"
      fill="currentColor"
      aria-hidden="true"
      focusable="false"
    >
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-2.91-.88-2.91-3.9 0-.86.31-1.56.82-2.11-.04-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.42 7.42 0 0 1 2-.27c.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.12 1.92.08 2.12.51.55.82 1.24.82 2.11 0 3.03-1.14 3.7-2.92 3.9.33.29.62.86.62 1.74 0 1.25-.01 2.27-.01 2.58 0 .21.15.46.55.38A7.995 7.995 0 0 0 16 8c0-4.42-3.58-8-8-8Z" />
    </svg>
  )
}
