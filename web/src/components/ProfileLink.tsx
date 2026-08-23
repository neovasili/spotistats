import type { ReactNode } from 'react'
import { artistPath, navigateTo } from '../lib/router'

/**
 * A link to an artist's profile page.
 *
 * A real `href`, not a click handler on a span: the link stays shareable, middle-clickable and
 * openable in a new tab, and a modified click falls through to the browser. The handler only
 * avoids a full reload for a same-origin move.
 *
 * A name-keyed artist -- one that exists in the imported history as text and has no Spotify ID
 * yet -- has no profile to reach, so the children render unwrapped. Linking would produce a
 * page whose only honest content is "this artist has no external profile", which is worse than
 * no link at all.
 */
export function ArtistProfileLink({
  id,
  children,
  className,
}: {
  id: string
  children: ReactNode
  className?: string
}) {
  if (!id || id.startsWith('nm:')) return <>{children}</>
  const path = artistPath(id)
  return (
    <a
      className={className}
      href={path}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return
        e.preventDefault()
        navigateTo(path)
      }}
    >
      {children}
    </a>
  )
}
