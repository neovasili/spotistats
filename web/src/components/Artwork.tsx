import { useState } from 'react'

export type ArtworkSize = 'sm' | 'md' | 'lg'

interface Props {
  /** The small asset. Falls back to `imageUrl` when a row predates thumbnail capture. */
  thumbUrl?: string
  imageUrl?: string
  /** Used only for the fallback tile's initial. Never rendered as alt text — see below. */
  name: string
  size?: ArtworkSize
}

/**
 * An entity's artwork, with a fallback that is the same shape and size as the image.
 *
 * # Absence and failure are one case
 *
 * A never-enriched entity, a tombstoned one, and a CDN error all render the identical tile: a
 * neutral surface with the entity's first initial. Most rows genuinely have no artwork — it
 * arrives only once the API has resolved the entity — so the fallback is the common path, not
 * an error state. Never a broken-image glyph, and never a gap that changes the row height.
 *
 * # The box is always reserved
 *
 * Fixed size with `aspect-ratio: 1`, so a row of thumbnails resolving at different times causes
 * no layout shift. Album art is square at source; artist photos are not, hence `object-fit:
 * cover`.
 *
 * # alt is deliberately empty
 *
 * The entity name is always adjacent text, so the image is decorative. Repeating the name in
 * `alt` makes a screen reader announce it twice. Artwork is decoration; the name is the
 * identity, and every row stays fully legible with every image blocked.
 */
export function Artwork({ thumbUrl, imageUrl, name, size = 'sm' }: Props) {
  const src = thumbUrl || imageUrl
  const [failed, setFailed] = useState(false)

  if (!src || failed) {
    return (
      <span className={`artwork artwork--${size} artwork--empty`} aria-hidden="true">
        {initialOf(name)}
      </span>
    )
  }
  return (
    <img
      className={`artwork artwork--${size}`}
      src={src}
      alt=""
      // Lazy below the fold: thirty images on the dashboard is fine eagerly, a hundred-row
      // Explorer table is not.
      loading="lazy"
      decoding="async"
      onError={() => setFailed(true)}
    />
  )
}

/** The first character of a name, for the fallback tile. */
function initialOf(name: string): string {
  const trimmed = name.trim()
  if (!trimmed) return '?'
  // Iterate by code point so an emoji or an accented letter is not split mid-character.
  return [...trimmed][0]!.toUpperCase()
}

/**
 * A link to the entity on Spotify.
 *
 * Not decorative: Spotify's Developer Policy requires cover art and metadata to be
 * "accompanied by a link back to the applicable album, content or playlist on the Spotify
 * Service" (docs/SPECS.md 2.7). The URL is derivable from the ID already stored, so nothing
 * extra needs capturing.
 *
 * Name-keyed entities have no Spotify identity yet, so there is nothing to link to; children
 * render unwrapped rather than as a link that 404s.
 */
export function SpotifyLink({
  kind,
  id,
  children,
  className,
}: {
  kind: 'artist' | 'album' | 'track'
  id: string
  children: React.ReactNode
  className?: string
}) {
  if (!id || id.startsWith('nm:')) {
    return <>{children}</>
  }
  return (
    <a
      className={className}
      href={`https://open.spotify.com/${kind}/${id}`}
      target="_blank"
      // noreferrer as well as noopener: opener alone still leaks the referrer.
      rel="noopener noreferrer"
    >
      {children}
    </a>
  )
}
