// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { ArtistProfileLink } from './ProfileLink'

afterEach(cleanup)

describe('ArtistProfileLink', () => {
  it('is a real href, so it stays shareable and middle-clickable', () => {
    render(<ArtistProfileLink id="ar1">Within Temptation</ArtistProfileLink>)
    const a = screen.getByText('Within Temptation').closest('a')!
    expect(a.getAttribute('href')).toBe('/artist/ar1')
  })

  it('does not link a name-keyed artist', () => {
    // An artist that exists only as text in the imported history has no profile to reach; the
    // page would say nothing but "no external profile", which is worse than no link.
    render(<ArtistProfileLink id="nm:disturbed">Disturbed</ArtistProfileLink>)
    expect(screen.getByText('Disturbed').closest('a')).toBeNull()
  })

  it('does not link an empty id', () => {
    render(<ArtistProfileLink id="">Unknown</ArtistProfileLink>)
    expect(screen.getByText('Unknown').closest('a')).toBeNull()
  })
})
