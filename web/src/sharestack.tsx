import { useToast } from './ui'
import { mountLine } from './shares'
import { addExternalVolume } from './sharecompose'
import { ShareBadge, useShareVolumes } from './sharesui'

/**
 * Under a stack's compose editor: the host's network share volumes, each
 * of which adds itself to the file's top-level volumes as external (so
 * compose uses the existing volume instead of making an empty one).
 */
export function StackShareHelper({ hostId, online, compose, setCompose }: { hostId: string; online: boolean; compose: string; setCompose: (c: string) => void }) {
  const toast = useToast()
  const shares = useShareVolumes(hostId, online)
  if (shares.length === 0) return null
  return (
    <div className="flex flex-wrap items-center gap-2 text-xs text-ink-100/60">
      <span>Network shares on this host:</span>
      {shares.map((sv) => (
        <button
          key={sv.name}
          type="button"
          title={`Add ${sv.name} to the file's volumes as external`}
          className="inline-flex items-center gap-1.5 rounded-[2px] px-2 py-1 ring-1 ring-inset ring-line transition hover:ring-lime-400/50"
          onClick={() => {
            const next = addExternalVolume(compose, sv.name)
            if (next === null) return toast('bad', "The compose file doesn't parse (or its volumes: isn't a map), so nothing was added.")
            if (next !== compose) setCompose(next)
            toast('good', `${sv.name} is in volumes: as external. Mount it in each service that needs it: ${mountLine(sv.name)}`)
          }}
        >
          <span className="mono text-white">{sv.name}</span>
          <ShareBadge share={sv.share} />
        </button>
      ))}
    </div>
  )
}
