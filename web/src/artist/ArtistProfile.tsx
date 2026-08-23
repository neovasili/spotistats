import { useEffect, useState } from 'react'
import { ApiError, fetchArtistProfile, type ArtistProfile, type ProfileMember } from '../lib/api'
import { estimatedCaveat, formatDate, formatNumber, formatPrecisionDate } from '../lib/format'
import { Duration } from '../components/Duration'
import { Artwork, SpotifyLink } from '../components/Artwork'
import { ROUTE_PATHS, navigateTo } from '../lib/router'

type State =
  | { status: 'loading' }
  /** `never` is the 404: this artist has not been through enrichment at all. */
  | { status: 'never' }
  | { status: 'error'; message: string }
  | { status: 'ready'; profile: ArtistProfile }

/**
 * One artist's profile: external facts and prose beside the listening figures Spotistats owns
 * (docs/SPECS.md 7.7).
 *
 * Three rules here come from the data rather than from taste.
 *
 * **The two genre vocabularies never share a row.** MusicBrainz and Spotify tag artists under
 * different taxonomies, so a merged row of chips would present an agreement that does not
 * exist, and no reader could tell which source said what. MusicBrainz's genres are also
 * CC-BY-NC-SA, which makes the separate label a licence obligation rather than a nicety.
 *
 * **Dates are rendered at the precision they were stored at.** `beganAt` may be `2008`,
 * `2008-04` or `2008-04-17`; showing "1 January 2008" for the first would invent a day the
 * source does not claim.
 *
 * **A missing profile is a missing profile.** An unresolved artist gets the listening block and
 * one plain sentence -- never a loading skeleton that implies data is on its way, and never
 * placeholder prose.
 */
export function ArtistProfilePage({ id }: { id: string }) {
  const [state, setState] = useState<State>({ status: 'loading' })

  useEffect(() => {
    const controller = new AbortController()
    setState({ status: 'loading' })
    fetchArtistProfile(id, controller.signal)
      .then((profile) => setState({ status: 'ready', profile }))
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return
        // A 404 is not a failure. It means enrichment has never looked at this artist, which is
        // a different fact from "looked and found nothing" and gets different words below.
        if (err instanceof ApiError && err.status === 404) {
          setState({ status: 'never' })
          return
        }
        setState({
          status: 'error',
          message: err instanceof ApiError ? err.message : 'Could not load this artist.',
        })
      })
    return () => controller.abort()
  }, [id])

  return (
    <>
      <p className="crumb">
        <a
          href={ROUTE_PATHS.explore}
          onClick={(e) => {
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return
            e.preventDefault()
            navigateTo(ROUTE_PATHS.explore)
          }}
        >
          ← Explorer
        </a>
      </p>

      {state.status === 'loading' && <p className="empty">Loading…</p>}

      {state.status === 'never' && (
        <section className="card">
          <h2 className="card__title">No profile yet</h2>
          <p className="card__sub">
            This artist has not been through external enrichment, so there is nothing to show
            beyond the listening figures on the Explorer. Enrichment runs nightly and works
            through artists by listening time.
          </p>
          <p className="card__sub" style={{ marginTop: '0.75rem' }}>
            <SpotifyLink kind="artist" id={id}>
              Open on Spotify
            </SpotifyLink>
          </p>
        </section>
      )}

      {state.status === 'error' && (
        <section className="card">
          <h2 className="card__title">Nothing to show</h2>
          <p className="card__sub">{state.message}</p>
        </section>
      )}

      {state.status === 'ready' && <Profile profile={state.profile} />}
    </>
  )
}

function Profile({ profile: p }: { profile: ArtistProfile }) {
  const name = p.name || p.id
  const resolved = Boolean(p.mbid)

  return (
    <article className="profile">
      <Banner profile={p} name={name} />

      {/* The one-line statement of what is and is not known, placed where a reader looks first
          rather than at the foot of the page. An unresolved artist is a visible gap they can
          interpret; a silent one reads as a bug. */}
      {!resolved && (
        <p className="profile__note">
          No external profile is linked to this artist
          {p.refreshedAt ? ` (checked ${formatDate(p.refreshedAt)})` : ''}. MusicBrainz has no
          Spotify link for them, and Spotistats does not guess by name — a close name match
          attaches the wrong biography and members to a real band, and nothing downstream could
          detect it. The listening figures below are unaffected.
        </p>
      )}

      {resolved && <Facts profile={p} />}

      <Listening profile={p} />

      {p.biography && <Biography text={p.biography} lang={p.biographyLang} />}

      {p.members && p.members.length > 0 && <Members members={p.members} />}

      <Genres mb={p.mbGenres} spotify={p.listening.spotifyGenres} />

      <Attribution profile={p} />
    </article>
  )
}

/**
 * The header: a fanart or wide-thumb banner with the portrait inset.
 *
 * This is the one place on the site a large image is worth the bytes. Fanart is purely
 * decorative and carries `alt=""` — the name is the adjacent heading — and the banner box keeps
 * its aspect ratio whether an image loads or not, so nothing reflows.
 *
 * `strArtistLogo` is deliberately not rendered. It is frequently a trademarked wordmark that
 * TheAudioDB's terms require be shown unmodified, and there is no slot on this layout where it
 * would sit at its own size without recolouring or compositing.
 */
function Banner({ profile: p, name }: { profile: ArtistProfile; name: string }) {
  const banner = p.images.banner || p.images.fanart?.[0] || p.images.wideThumb
  return (
    <header className={`profile__banner${banner ? '' : ' profile__banner--plain'}`}>
      {banner && <img className="profile__bannerimg" src={banner} alt="" decoding="async" />}
      <div className="profile__id">
        <SpotifyLink kind="artist" id={p.id}>
          <Artwork
            thumbUrl={p.images.thumb || p.images.spotify}
            imageUrl={p.images.spotify}
            name={name}
            size="lg"
          />
        </SpotifyLink>
        <div>
          <h2 className="profile__name">{name}</h2>
          <p className="profile__kicker">{summaryLine(p)}</p>
        </div>
      </div>
    </header>
  )
}

/** The kicker under the name: type, origin and formation, in whatever is actually known. */
function summaryLine(p: ArtistProfile): string {
  const parts = [
    p.artistType,
    [p.beginAreaName, p.areaName || p.country].filter(Boolean).join(', '),
    p.beganAt ? `formed ${formatPrecisionDate(p.beganAt)}` : undefined,
  ].filter(Boolean)
  return parts.length > 0 ? parts.join(' · ') : 'Artist'
}

function Facts({ profile: p }: { profile: ArtistProfile }) {
  const rows: [string, string][] = []
  if (p.artistType) rows.push(['Type', p.artistType])
  // Formed-in city and country are different facts: a band can be Dutch and have formed in
  // Waddinxveen, so both are shown rather than one standing in for the other.
  if (p.beginAreaName) rows.push(['Formed in', p.beginAreaName])
  if (p.areaName || p.country) rows.push(['Country', p.areaName || p.country!])
  // Rendered at the stored precision, never past it: a stored "1996" stays "1996".
  if (p.beganAt) rows.push([p.artistType === 'Person' ? 'Born' : 'Formed', formatPrecisionDate(p.beganAt)])
  if (p.endedAt) rows.push([p.artistType === 'Person' ? 'Died' : 'Disbanded', formatPrecisionDate(p.endedAt)])
  else if (p.ended) rows.push(['Status', 'No longer active'])
  if (p.members) rows.push(['Members', formatNumber(p.members.length)])

  if (rows.length === 0) return null

  return (
    <section className="card">
      <div className="card__head">
        <div>
          <h3 className="card__title">Facts</h3>
          <p className="card__sub">From MusicBrainz</p>
        </div>
      </div>
      <dl className="facts">
        {rows.map(([k, v]) => (
          <div key={k} className="facts__row">
            <dt>{k}</dt>
            <dd>{v}</dd>
          </div>
        ))}
      </dl>
    </section>
  )
}

function Listening({ profile: p }: { profile: ArtistProfile }) {
  const m = p.listening.metrics
  return (
    <section className="card">
      <div className="card__head">
        <div>
          <h3 className="card__title">Listening</h3>
          <p className="card__sub">From your own history — the figures Spotistats owns</p>
        </div>
      </div>
      {m.plays === 0 ? (
        <p className="empty">No plays recorded for this artist.</p>
      ) : (
        <dl className="detail__figures">
          <div>
            <dt>Listening</dt>
            <dd className="detail__big"><Duration ms={m.msPlayed} /></dd>
          </div>
          <div>
            <dt>Plays</dt>
            <dd className="detail__big">{formatNumber(m.plays)}</dd>
          </div>
          <div>
            <dt>First played</dt>
            <dd className="detail__big">{formatDate(p.listening.firstPlayedAt)}</dd>
          </div>
          <div>
            <dt>Last played</dt>
            <dd className="detail__big">{formatDate(p.listening.lastPlayedAt)}</dd>
          </div>
        </dl>
      )}
      {m.plays > 0 && estimatedCaveat(m.estimatedRatio) && (
        <p className="detail__caveat">{estimatedCaveat(m.estimatedRatio)}</p>
      )}
    </section>
  )
}

/** Roughly six lines before the expand control; the clamp itself is CSS. */
function Biography({ text, lang }: { text: string; lang?: string }) {
  const [expanded, setExpanded] = useState(false)
  return (
    <section className="card">
      <div className="card__head">
        <div>
          <h3 className="card__title">Biography</h3>
          <p className="card__sub">
            From TheAudioDB{lang && lang !== 'en' ? ` · ${lang.toUpperCase()}` : ''}
          </p>
        </div>
        <button
          type="button"
          className="ghost-button card__toggle"
          aria-expanded={expanded}
          onClick={() => setExpanded((v) => !v)}
        >
          {expanded ? 'Less' : 'More'}
        </button>
      </div>
      {/* whiteSpace: pre-line, because the source prose is paragraph-broken with newlines and
          collapsing them runs several paragraphs into one wall of text. */}
      <p className={`prose${expanded ? '' : ' prose--clamped'}`}>{text}</p>
    </section>
  )
}

/**
 * Members, current first and past ones dimmed.
 *
 * Sorted here rather than at write time: "current" is a property of the data, not of the
 * order MusicBrainz happened to return, and the store keeps the source order deliberately.
 */
function Members({ members }: { members: ProfileMember[] }) {
  const sorted = [...members].sort((a, b) => Number(a.ended ?? false) - Number(b.ended ?? false))
  return (
    <section className="card">
      <div className="card__head">
        <div>
          <h3 className="card__title">Members</h3>
          <p className="card__sub">From MusicBrainz · past members dimmed</p>
        </div>
      </div>
      <ul className="members">
        {sorted.map((m) => (
          <li key={`${m.mbid || m.name}`} className={`members__row${m.ended ? ' members__row--past' : ''}`}>
            <span className="members__name">{m.name}</span>
            {m.instruments && m.instruments.length > 0 && (
              <span className="members__inst">{m.instruments.join(', ')}</span>
            )}
            <span className="members__tenure">{tenure(m)}</span>
          </li>
        ))}
      </ul>
    </section>
  )
}

/**
 * A member's tenure, at the precision stored.
 *
 * An open range renders as "1996–" only when the membership is current. A member with no end
 * date who HAS ended is a real MusicBrainz state -- the departure is known, its date is not --
 * and "1996–" would read as "still in the band", so it gets an explicit word instead.
 */
function tenure(m: ProfileMember): string {
  // Years only in this column. A tenure is a span, and mixing "1996" with "22 February 2011"
  // down one column makes the spans impossible to compare at a glance -- the full dates are
  // still there in the source, and the facts strip is where precision is the point.
  const from = m.begin?.slice(0, 4)
  const to = m.end?.slice(0, 4)
  if (from && to) return `${from}–${to}`
  if (from) return m.ended ? `${from}–, former` : `${from}–`
  if (to) return `until ${to}`
  return m.ended ? 'former' : ''
}

/**
 * The two genre vocabularies, in two labelled rows.
 *
 * They are NEVER merged. See the component docs: different taxonomies, and MusicBrainz's are
 * CC-BY-NC-SA, so the labelling is a licence obligation.
 */
function Genres({ mb, spotify }: { mb?: string[]; spotify?: string[] }) {
  const hasMB = Boolean(mb && mb.length > 0)
  const hasSpotify = Boolean(spotify && spotify.length > 0)
  if (!hasMB && !hasSpotify) return null

  return (
    <section className="card">
      <div className="card__head">
        <div>
          <h3 className="card__title">Genres</h3>
          <p className="card__sub">
            Two vocabularies, shown separately: they disagree, and neither is authoritative.
          </p>
        </div>
      </div>

      {hasMB && (
        <div className="chiprow">
          <span className="chiprow__label">MusicBrainz</span>
          <span className="chiprow__chips">
            {mb!.map((g) => (
              <span key={g} className="chip">{g}</span>
            ))}
          </span>
        </div>
      )}

      {hasSpotify && (
        <div className="chiprow">
          <span className="chiprow__label">Spotify</span>
          <span className="chiprow__chips">
            {spotify!.map((g) => (
              <span key={g} className="chip">{g}</span>
            ))}
          </span>
        </div>
      )}

      {!hasSpotify && (
        <p className="card__sub" style={{ marginTop: '0.75rem' }}>
          Spotify removed artist genres from its Web API in February 2026, so only artists
          resolved before then carry them.
        </p>
      )}
    </section>
  )
}

/**
 * Source credits.
 *
 * Not a courtesy. TheAudioDB's terms require the credit and a link back explicitly, and
 * MusicBrainz's genre data is CC-BY-NC-SA, so attribution is a licence condition of showing
 * the chips at all.
 */
function Attribution({ profile: p }: { profile: ArtistProfile }) {
  return (
    <footer className="attribution">
      <p>
        Listening figures are Spotistats’ own, from your Spotify history. Artist name
        {p.images.spotify ? ' and portrait' : ''}:{' '}
        <a href="https://open.spotify.com/" target="_blank" rel="noopener noreferrer">Spotify</a>.
      </p>
      {p.sources.facts && (
        <p>
          Facts, members and MusicBrainz genres:{' '}
          <a
            href={p.mbid ? `https://musicbrainz.org/artist/${p.mbid}` : 'https://musicbrainz.org/'}
            target="_blank"
            rel="noopener noreferrer"
          >
            MusicBrainz
          </a>
          . Core data is in the public domain (CC0); genres are licensed{' '}
          <a
            href="https://creativecommons.org/licenses/by-nc-sa/3.0/"
            target="_blank"
            rel="noopener noreferrer"
          >
            CC BY-NC-SA 3.0
          </a>
          .
        </p>
      )}
      {(p.sources.prose || p.sources.images) && (
        <p>
          Biography and artwork:{' '}
          <a href="https://www.theaudiodb.com/" target="_blank" rel="noopener noreferrer">
            TheAudioDB
          </a>
          .
        </p>
      )}
      {p.refreshedAt && (
        <p>
          External data last checked {formatDate(p.refreshedAt)}
          {p.resolvedVia === 'override' ? ' · matched by manual override' : ''}
          {p.resolvedVia === 'link' ? ' · matched via the Spotify link on MusicBrainz' : ''}
        </p>
      )}
    </footer>
  )
}
