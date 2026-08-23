/**
 * An entity's name with the context that makes it identifiable.
 *
 * Album and track titles repeat heavily across artists — "Bleed Out", "Mad World", "Legacy"
 * name nothing on their own — so a title is shown with its artist, and a track with both its
 * artist and its album.
 *
 * The composition lives here rather than at each call site so the dashboard cards, the Explorer
 * table and the drill-down panel cannot disagree about it. Context is rendered in muted ink,
 * never in a series colour: a coloured mark beside the name carries identity, the words do not.
 */
interface Props {
  name: string
  artistName?: string
  albumName?: string
}

/** Joins whichever context is present. Nothing is emitted when none is. */
export function entityContext(artistName?: string, albumName?: string): string {
  // Artist first: it is the stronger identifier, and it is the part that survives when the
  // album row has not been enriched yet.
  return [artistName, albumName].filter(Boolean).join(' · ')
}

export function EntityName({ name, artistName, albumName }: Props) {
  const context = entityContext(artistName, albumName)
  return (
    <span className="entity">
      <span className="entity__name">{name}</span>
      {context && <span className="entity__context">{context}</span>}
    </span>
  )
}
