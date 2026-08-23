// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { Artwork, SpotifyLink } from './Artwork'

afterEach(cleanup)

describe('Artwork', () => {
  it('prefers the thumbnail over the full-size asset', () => {
    // The whole reason ThumbURL is captured: a hundred-row table must not pull a hundred 640px
    // covers to paint them at 28px.
    const { container } = render(<Artwork thumbUrl="/thumb.jpg" imageUrl="/big.jpg" name="A" />)
    expect(container.querySelector('img')?.getAttribute('src')).toBe('/thumb.jpg')
  })

  it('falls back to the full-size asset for rows captured before thumbnails existed', () => {
    const { container } = render(<Artwork imageUrl="/big.jpg" name="A" />)
    expect(container.querySelector('img')?.getAttribute('src')).toBe('/big.jpg')
  })

  it('renders a same-sized tile when there is no artwork at all', () => {
    // Absence is the COMMON case, not an error: artwork arrives only once the API resolves the
    // entity. It must not be a broken-image glyph or a gap that changes the row height.
    const { container } = render(<Artwork name="Disturbed" />)
    expect(container.querySelector('img')).toBeNull()
    const tile = container.querySelector('.artwork--empty')
    expect(tile?.textContent).toBe('D')
  })

  it('treats a failed load exactly like absence', () => {
    const { container } = render(<Artwork thumbUrl="/gone.jpg" name="Nightwish" />)
    fireEvent.error(container.querySelector('img')!)
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('.artwork--empty')?.textContent).toBe('N')
  })

  it('keeps alt empty, because the name is adjacent text', () => {
    // A name in alt makes a screen reader announce the entity twice.
    const { container } = render(<Artwork thumbUrl="/t.jpg" name="Sabaton" />)
    expect(container.querySelector('img')?.getAttribute('alt')).toBe('')
  })

  it('lazy-loads, since a long table is mostly below the fold', () => {
    const { container } = render(<Artwork thumbUrl="/t.jpg" name="A" />)
    const img = container.querySelector('img')!
    expect(img.getAttribute('loading')).toBe('lazy')
    expect(img.getAttribute('decoding')).toBe('async')
  })

  it('handles names that give no usable initial', () => {
    for (const [name, want] of [['', '?'], ['   ', '?'], ['éclair', 'É']] as const) {
      cleanup()
      const { container } = render(<Artwork name={name} />)
      expect(container.querySelector('.artwork--empty')?.textContent).toBe(want)
    }
  })

  it('does not split a multi-byte first character', () => {
    // Iterating by code unit would emit half a surrogate pair and render a replacement glyph.
    const { container } = render(<Artwork name="🎸 Guitar" />)
    expect(container.querySelector('.artwork--empty')?.textContent).toBe('🎸')
  })
})

describe('SpotifyLink', () => {
  it('links to the entity on Spotify', () => {
    // Required by Spotify's Developer Policy, not decorative: displayed cover art must be
    // accompanied by a link back to the entity.
    render(
      <SpotifyLink kind="artist" id="3hE8S8ohRErocpkY7uJW4a">
        <span>Within Temptation</span>
      </SpotifyLink>,
    )
    const a = screen.getByRole('link')
    expect(a.getAttribute('href')).toBe('https://open.spotify.com/artist/3hE8S8ohRErocpkY7uJW4a')
    expect(a.getAttribute('target')).toBe('_blank')
    // noreferrer as well as noopener: opener alone still leaks the referrer.
    expect(a.getAttribute('rel')).toContain('noopener')
    expect(a.getAttribute('rel')).toContain('noreferrer')
  })

  it('renders children unwrapped for a name-keyed entity', () => {
    // Those have no Spotify identity yet, so a link would 404.
    render(
      <SpotifyLink kind="artist" id="nm:alter bridge">
        <span>Alter Bridge</span>
      </SpotifyLink>,
    )
    expect(screen.queryByRole('link')).toBeNull()
    expect(screen.getByText('Alter Bridge')).toBeTruthy()
  })

  it('renders children unwrapped when there is no id', () => {
    render(
      <SpotifyLink kind="track" id="">
        <span>Unknown</span>
      </SpotifyLink>,
    )
    expect(screen.queryByRole('link')).toBeNull()
  })
})
