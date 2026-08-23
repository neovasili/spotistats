// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { EntityName, entityContext } from './EntityName'

afterEach(cleanup)

describe('entityContext', () => {
  it('shows artist before album', () => {
    // Artist first: it is the stronger identifier and the part that survives when the album
    // row has not been enriched.
    expect(entityContext('Within Temptation', 'Bleed Out')).toBe('Within Temptation · Bleed Out')
  })

  it('degrades to whichever part exists', () => {
    expect(entityContext('Within Temptation', undefined)).toBe('Within Temptation')
    expect(entityContext(undefined, 'Bleed Out')).toBe('Bleed Out')
  })

  it('emits nothing when there is no context', () => {
    // An artist entry has no context to add, and a separator with nothing around it would read
    // as missing data.
    expect(entityContext(undefined, undefined)).toBe('')
    expect(entityContext('', '')).toBe('')
  })
})

describe('EntityName', () => {
  it('renders the title and its context', () => {
    render(<EntityName name="Bad Things" artistName="Within Temptation" albumName="Bleed Out" />)
    expect(screen.getByText('Bad Things')).toBeTruthy()
    expect(screen.getByText('Within Temptation · Bleed Out')).toBeTruthy()
  })

  it('renders no context element at all for a bare name', () => {
    // An empty element would still occupy its line height and misalign the row against
    // neighbours that do have context.
    const { container } = render(<EntityName name="Sabaton" />)
    expect(container.querySelector('.entity__context')).toBeNull()
    expect(screen.getByText('Sabaton')).toBeTruthy()
  })
})
