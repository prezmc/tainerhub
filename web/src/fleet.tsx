import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, can, errorText, type Activity, type Container, type DetailResponse, type Host, type Me, type VolumeRes } from './api'
import { cx, useConfirm, useToast } from './ui'

export type Action = 'start' | 'stop' | 'restart' | 'pause' | 'unpause' | 'kill' | 'remove' | 'recreate'
export const ACTIONS: Action[] = ['start', 'stop', 'restart', 'pause', 'unpause', 'kill', 'remove']

const pastTense: Record<Action, string> = {
  start: 'started',
  stop: 'stopped',
  restart: 'restarted',
  pause: 'paused',
  unpause: 'resumed',
  kill: 'killed',
  remove: 'removed',
  recreate: 'updated',
}

/** True while AppLayout's live fleet stream is connected. */
export const LiveContext = createContext(false)
export const useLive = () => useContext(LiveContext)

/** The query options for the fleet's hosts: the live stream keeps them fresh, polling only while it's down. */
export function useHostsQuery() {
  const live = useLive()
  return useQuery({ queryKey: ['hosts'], queryFn: () => api<Host[]>('/api/hosts'), refetchInterval: live ? false : 30000 })
}

/** Live data: kept fresh by the SSE stream in AppLayout; polling is only a fallback while it's down. */
export function useFleet() {
  const live = useLive()
  const containers = useQuery({ queryKey: ['containers'], queryFn: () => api<Container[]>('/api/containers'), refetchInterval: live ? false : 30000 })
  const hosts = useHostsQuery()
  const activity = useQuery<Activity[]>({ queryKey: ['activity'], queryFn: () => Promise.resolve([]), staleTime: Infinity })
  // An error only matters while there's nothing to show: stale data beats an error.
  const error = (!containers.data && containers.error) || (!hosts.data && hosts.error) || null
  return {
    containers: containers.data ?? [],
    hosts: hosts.data ?? [],
    activity: activity.data ?? [],
    loading: (containers.isPending || hosts.isPending) && !error,
    error: error ? errorText(error) : null,
  }
}

/** Describes TainerHub's own containers, which are risky to stop or remove from TainerHub. */
function ownContainer(c: Container): string | null {
  if (c.name === 'tainerhub') return "TainerHub's hub"
  if (c.name === 'tainerhub-postgres') return "TainerHub's database"
  if (c.name === 'tainerhub-agent' || c.image.includes('tainerhub-agent')) return `the agent that connects ${c.hostName} to TainerHub`
  return null
}

type VolumeChoice = { on: boolean; names: string[] }
type RemoveVolumesResult = { anonymous: number; removed: string[]; kept: { name: string; reason: string }[] }

const isAnonymous = (name: string) => /^[0-9a-f]{64}$/.test(name)

/**
 * The "also delete its volumes" part of the Remove dialog (admins only):
 * lists each volume the container uses and what would happen to it.
 */
function RemoveVolumesOption({ c, onChange }: { c: Container; onChange: (v: VolumeChoice) => void }) {
  const [on, setOn] = useState(false)
  const detail = useContainerDetail(c)
  const volumes = useQuery({ queryKey: ['volumes', c.hostId], queryFn: () => api<VolumeRes[]>(`/api/hosts/${c.hostId}/volumes`) })
  const mounts = (detail.data?.detail.mounts ?? []).filter((m) => m.type === 'volume' && m.name)
  const rows = mounts.map((m) => {
    const name = m.name!
    const others = (volumes.data?.find((v) => v.name === name)?.usedBy ?? []).filter((u) => u !== c.name)
    return { name, path: m.destination, anonymous: isAnonymous(name), others }
  })
  const deletable = rows.filter((r) => !r.anonymous && r.others.length === 0).map((r) => r.name)
  useEffect(() => {
    onChange({ on, names: on ? deletable : [] })
  }, [on, deletable.join(',')]) // eslint-disable-line react-hooks/exhaustive-deps

  if (detail.isPending || volumes.isPending) return <p className="mt-3 text-xs text-ink-100/50">Checking its volumes…</p>
  if (detail.isError) return null
  if (rows.length === 0) return <p className="mt-3 text-xs text-ink-100/50">It has no volumes.</p>
  return (
    <div className="mt-4 space-y-2">
      <label className="flex items-start gap-2.5 rounded-[3px] bg-rose-500/[0.07] p-3 ring-1 ring-inset ring-rose-500/20">
        <input type="checkbox" checked={on} onChange={(e) => setOn(e.target.checked)} className="mt-0.5 accent-rose-500" />
        <span>
          Also delete its volumes. <strong className="text-rose-200">Their data is gone for good.</strong>
        </span>
      </label>
      <ul className="space-y-1 text-xs">
        {rows.map((r) => (
          <li key={r.name} className="flex items-baseline gap-2">
            <span className={cx('mono truncate', r.others.length ? 'text-ink-100/60' : on ? 'text-rose-200 line-through decoration-rose-400/60' : 'text-ink-50')}>
              {r.anonymous ? `${r.name.slice(0, 12)}… (unnamed)` : r.name}
            </span>
            <span className="mono shrink-0 text-ink-100/40">{r.path}</span>
            <span className="ml-auto shrink-0 text-ink-100/50">
              {r.others.length ? `kept: also used by ${r.others.join(', ')}` : on ? 'will be deleted' : 'kept'}
            </span>
          </li>
        ))}
      </ul>
    </div>
  )
}

export function useContainerAction() {
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const m = useMutation({
    mutationFn: ({ c, action }: { c: Container; action: Action }) => api(`/api/hosts/${c.hostId}/containers/${encodeURIComponent(c.id)}/${action}`, { method: 'POST' }),
    onSuccess: (_, { c, action }) => {
      toast('good', `${c.name} ${pastTense[action]}`)
      qc.invalidateQueries({ queryKey: ['containers'] })
    },
    onError: (e, { c }) => toast('bad', `${c.name}: ${errorText(e)}`),
  })
  const volumeChoice = useRef<VolumeChoice>({ on: false, names: [] })
  const removeVolumes = useMutation({
    mutationFn: ({ c, names }: { c: Container; names: string[] }) =>
      api<RemoveVolumesResult>(`/api/hosts/${c.hostId}/containers/${encodeURIComponent(c.id)}/remove-with-volumes`, { method: 'POST', body: { volumes: names } }),
    onSuccess: (r, { c }) => {
      const n = r.removed.length + r.anonymous
      toast('good', `${c.name} removed with ${n} volume${n === 1 ? '' : 's'}`)
      for (const k of r.kept) toast('bad', `Kept volume ${k.name}: ${k.reason}`)
      qc.invalidateQueries({ queryKey: ['containers'] })
      qc.invalidateQueries({ queryKey: ['volumes', c.hostId] })
    },
    onError: (e, { c }) => toast('bad', `${c.name}: ${errorText(e)}`),
  })
  const mutate = m.mutate
  const removeWithVolumes = removeVolumes.mutate
  const run = useCallback(
    async (c: Container, action: Action): Promise<boolean> => {
      const own = ownContainer(c)
      const fromStack = c.project && !own ? ` It belongs to the ${c.project} stack, so redeploying that stack recreates it.` : ''
      const destructive: Partial<Record<Action, { title: string; body: string; confirm: string }>> = {
        stop: { title: `Stop ${c.name}?`, body: `It stays stopped on ${c.hostName} until you start it again.`, confirm: 'Stop' },
        kill: { title: `Kill ${c.name}?`, body: 'Sends SIGKILL immediately. The process gets no chance to shut down cleanly.', confirm: 'Kill' },
        recreate: {
          title: `Pull and recreate ${c.name}?`,
          body: `Pulls the newest ${c.image} and replaces the container with an identical one on the new image. It restarts; if the new one doesn't start, the old one is put back automatically.`,
          confirm: 'Pull & recreate',
        },
        remove: {
          title: `Remove ${c.name}?`,
          body: `The container is deleted from ${c.hostName}, even if it's running. Its named volumes and image are kept.${fromStack}`,
          confirm: 'Remove',
        },
      }
      const d = destructive[action]
      if (d) {
        // Stopping TainerHub's own containers from TainerHub cuts it off.
        const body = own ? `${d.body} This is ${own}: TainerHub stops working until you start it again on the server with docker compose.` : d.body
        const me = qc.getQueryData<Me>(['me'])
        if (action === 'remove' && can(me?.user, 'admin')) {
          volumeChoice.current = { on: false, names: [] }
          // The checkbox below decides about volumes, so don't promise they're kept.
          const adminBody = body.replace(' Its named volumes and image are kept.', ' Its image is kept.')
          const full = (
            <>
              {adminBody}
              <RemoveVolumesOption
                c={c}
                onChange={(v) => {
                  volumeChoice.current = v
                }}
              />
            </>
          )
          if (!(await confirm({ ...d, body: full, danger: true }))) return false
          if (volumeChoice.current.on) {
            removeWithVolumes({ c, names: volumeChoice.current.names })
            return true
          }
          mutate({ c, action })
          return true
        }
        if (!(await confirm({ ...d, body, danger: action !== 'recreate' }))) return false
      }
      mutate({ c, action })
      return true
    },
    [confirm, mutate, qc, removeWithVolumes],
  )
  const pendingId = m.isPending ? m.variables?.c.id : removeVolumes.isPending ? removeVolumes.variables?.c.id : undefined
  return { run, pendingId }
}

/** Everything Docker knows about one container (the same query the details panel uses). */
export function useContainerDetail(c: Container, refetchInterval?: number) {
  return useQuery({
    queryKey: ['detail', c.hostId, c.id],
    queryFn: () => api<DetailResponse>(`/api/hosts/${c.hostId}/containers/${encodeURIComponent(c.id)}`),
    refetchInterval,
  })
}

// ---- logs drawer, opened from anywhere ----

export const LogsContext = createContext<(c: Container) => void>(() => {})
export const useOpenLogs = () => useContext(LogsContext)

// ---- terminal drawer, opened from anywhere (admins) ----

export const TerminalContext = createContext<(c: Container) => void>(() => {})
export const useOpenTerminal = () => useContext(TerminalContext)

// ---- container details panel, opened from anywhere ----

export const DetailsContext = createContext<(c: Container) => void>(() => {})
export const useOpenDetails = () => useContext(DetailsContext)

// ---- command palette, opened from anywhere ----

export const PaletteContext = createContext<() => void>(() => {})
export const useOpenPalette = () => useContext(PaletteContext)

export const sortFns = {
  cpu: (a: Container, b: Container) => (b.cpu ?? -1) - (a.cpu ?? -1) || a.name.localeCompare(b.name),
  memory: (a: Container, b: Container) => (b.mem ?? -1) - (a.mem ?? -1) || a.name.localeCompare(b.name),
  name: (a: Container, b: Container) => a.name.localeCompare(b.name),
  status: (a: Container, b: Container) => rank(a) - rank(b) || a.name.localeCompare(b.name),
}
export type SortKey = keyof typeof sortFns

function rank(c: Container) {
  if (c.status.includes('(unhealthy)') || c.state === 'restarting' || c.state === 'dead') return 0
  if (c.state === 'running') return 1
  if (c.state === 'paused') return 2
  return 3
}

/** Reads a small UI preference from localStorage, tolerating storage being unavailable. */
export function loadPref<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  try {
    const v = localStorage.getItem(key) as T | null
    return v && allowed.includes(v) ? v : fallback
  } catch {
    return fallback
  }
}
export function savePref(key: string, v: string) {
  try {
    localStorage.setItem(key, v)
  } catch {
    /* storage unavailable: fine */
  }
}
