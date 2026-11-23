import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Lightbulb } from 'lucide-react'
import { api, bytes, stackProblem, type Host, type HostLoad, type Placement, type PlacementHost } from './api'
import { useFleet } from './fleet'
import { cx } from './ui'

/** Hosts ranked by room for new work, or for moving a stack (stackId) or a single container. */
export function usePlacement(stackId?: string, container?: { hostId: string; id: string }) {
  const q = stackId
    ? `?stack=${encodeURIComponent(stackId)}`
    : container
      ? `?host=${encodeURIComponent(container.hostId)}&container=${encodeURIComponent(container.id)}`
      : ''
  return useQuery({
    queryKey: ['placement', q],
    queryFn: () => api<Placement>(`/api/placement${q}`),
    refetchInterval: 30000,
  })
}

/** Like bytes(), but in TB for large disks. */
const space = (n: number) => (n >= 1024 ** 4 ? `${(n / 1024 ** 4).toFixed(1)} TB` : bytes(n))

const pct = (n: number | undefined) => (n === undefined ? '–' : `${Math.round(n)}%`)

/** Short summary for a host option, e.g. "CPU 20% · memory 38%". */
function figures(p: PlacementHost) {
  if (p.cpu === undefined && p.mem === undefined) return ''
  return `CPU ${pct(p.cpu)} · memory ${pct(p.mem)}`
}

function optionText(p: PlacementHost, h: Host | undefined) {
  const parts = [p.hostName]
  if (!h?.online) parts.push('(offline)')
  else if (stackProblem(h)) parts.push('(stacks not set up)')
  else if (!p.eligible && p.reason) parts.push(`(${p.reason})`)
  const f = figures(p)
  if (f) parts.push(`— ${f}`)
  if (p.suggested) parts.push('★ suggested')
  return parts.join(' ')
}

/**
 * Picks the suggested host once, when nothing is chosen yet. Falls back to
 * the first host that can run stacks while placement figures load or if
 * none has any.
 */
export function useDefaultHost(hostId: string, setHostId: (id: string) => void, enabled = true) {
  const { hosts } = useFleet()
  const placement = usePlacement()
  useEffect(() => {
    if (!enabled || hostId || hosts.length === 0) return
    if (placement.isPending) return
    const best = placement.data?.hosts.find((p) => p.suggested)?.hostId
    setHostId(best ?? (hosts.find((h) => !stackProblem(h)) ?? hosts[0]).id)
  }, [enabled, hostId, hosts, placement.isPending, placement.data, setHostId])
}

/**
 * The Host field: hosts ordered by room for more work, with the suggested
 * one marked and a note on the choice. Without figures (older agents) it
 * lists hosts as before.
 */
export function HostSelect({
  value,
  onChange,
  disabled,
  className,
}: {
  value: string
  onChange: (id: string) => void
  disabled?: boolean
  className?: string
}) {
  const { hosts } = useFleet()
  const placement = usePlacement()
  const ranked = placement.data?.hosts ?? []
  const byId = new Map(hosts.map((h) => [h.id, h]))
  // Hosts the ranking doesn't know yet (just added) go last, as before.
  const rows: PlacementHost[] = [...ranked, ...hosts.filter((h) => !ranked.some((p) => p.hostId === h.id)).map((h) => ({ hostId: h.id, hostName: h.name, eligible: false }))]
  const chosen = ranked.find((p) => p.hostId === value)
  const suggested = ranked.find((p) => p.suggested)

  return (
    <div className="space-y-1.5">
      <select value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled} className={className}>
        {rows.map((p) => (
          <option key={p.hostId} value={p.hostId} className="bg-panel">
            {optionText(p, byId.get(p.hostId))}
          </option>
        ))}
      </select>
      <PlacementNote chosen={chosen} suggested={suggested} />
    </div>
  )
}

function PlacementNote({ chosen, suggested }: { chosen?: PlacementHost; suggested?: PlacementHost }) {
  if (!chosen) return null
  if (chosen.eligible && chosen.suggested) {
    return (
      <p className="flex items-start gap-1.5 text-xs text-ink-100/50">
        <Lightbulb className="mt-0.5 size-3 shrink-0 text-lime-400" />
        <span>Most room right now ({figures(chosen)}, 15-minute average).</span>
      </p>
    )
  }
  if (chosen.reason === 'not enough free memory' || chosen.reason?.startsWith('less than')) {
    return (
      <p className="rounded-[3px] bg-amber-500/10 px-2.5 py-1.5 text-xs leading-relaxed text-amber-100 ring-1 ring-inset ring-amber-500/20">
        {chosen.hostName} is short on {chosen.reason === 'not enough free memory' ? 'memory' : 'disk'}
        {chosen.reason === 'not enough free memory' ? ` (${pct(chosen.mem)} in use)` : chosen.diskFree ? ` (${space(chosen.diskFree)} free)` : ''}.
        {suggested && ` ${suggested.hostName} has more room.`}
      </p>
    )
  }
  if (suggested && suggested.hostId !== chosen.hostId) {
    return (
      <p className="text-xs text-ink-100/50">
        {suggested.hostName} has the most room ({figures(suggested)}).
      </p>
    )
  }
  return null
}

/** CPU, memory and disk bars for a host card. */
export function LoadBars({ load, memTotal }: { load: HostLoad; memTotal: number }) {
  const memUsed = memTotal > 0 && load.memAvailable > 0 ? ((memTotal - load.memAvailable) / memTotal) * 100 : undefined
  const diskUsed = load.diskTotal && load.diskFree !== undefined ? ((load.diskTotal - load.diskFree) / load.diskTotal) * 100 : undefined
  const minutes = Math.max(1, Math.round((new Date(load.at).getTime() - new Date(load.since).getTime()) / 60000))
  const span = load.samples > 1 ? `average of the last ${minutes} min` : 'latest sample'
  return (
    <div className="mt-4 space-y-2 text-xs" title={`CPU and memory: ${span}. Load average ${load.load1.toFixed(2)}.`}>
      <Bar label="CPU" value={load.cpu} detail={load.cpu === undefined ? 'measuring…' : pct(load.cpu)} />
      <Bar label="Memory" value={memUsed} detail={memUsed === undefined ? '–' : `${pct(memUsed)} · ${bytes(load.memAvailable)} free`} />
      {diskUsed !== undefined && <Bar label="Disk" value={diskUsed} detail={`${pct(diskUsed)} · ${space(load.diskFree ?? 0)} free`} />}
    </div>
  )
}

function Bar({ label, value, detail }: { label: string; value?: number; detail: string }) {
  const v = Math.max(0, Math.min(100, value ?? 0))
  const tone = v >= 90 ? 'bg-rose-400' : v >= 75 ? 'bg-amber-400' : 'bg-lime-400'
  return (
    <div className="grid grid-cols-[56px_minmax(0,1fr)_auto] items-center gap-2">
      <span className="text-ink-100/50">{label}</span>
      <div
        className="h-1.5 overflow-hidden bg-white/[0.06]"
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={value === undefined ? undefined : Math.round(v)}
      >
        {value !== undefined && <div className={cx('h-full transition-all duration-500', tone)} style={{ width: `${v}%` }} />}
      </div>
      <span className="truncate text-right tabular-nums text-ink-100/70">{detail}</span>
    </div>
  )
}
