import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import { AlertTriangle } from 'lucide-react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, can, errorText, pullErrorText, type Activity, type Container, type DetailResponse, type Host, type Me, type VolumeRes } from './api'
import { cx, useConfirm, useToast } from './ui'
import { ownConsequence, ownContainer, ownServerCommands, type OwnContainer } from './own'
import { networkUsers, sharedNetConsequence } from './sharednet'
import { byStorage } from './storage'

/** Actions that leave a container down (the agent's self-guard covers these, plus recreate). */
const takesDown = (a: Action) => a === 'stop' || a === 'kill' || a === 'pause' || a === 'remove' || a === 'recreate'

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

/** The compose project the host's agent runs in, from the cached hosts list. */
export function useOwnProject() {
  const qc = useQueryClient()
  return useCallback((hostId: string) => qc.getQueryData<Host[]>(['hosts'])?.find((h) => h.id === hostId)?.hello?.own_project, [qc])
}

/** The commands to run on the server, set apart from the text. */
function Cmd({ children }: { children: string }) {
  return <code className="mono mt-1 block rounded-[2px] bg-ink-950/70 px-2.5 py-1.5 text-[12px] text-ink-50 ring-1 ring-inset ring-white/10">{children}</code>
}

/** The warning block shown for TainerHub's own containers. */
function OwnWarning({ children }: { children: ReactNode }) {
  return (
    <div className="mt-3 flex gap-2.5 rounded-[3px] bg-amber-400/[0.07] p-3 text-amber-100 ring-1 ring-inset ring-amber-400/25">
      <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-300" />
      <div className="space-y-2">{children}</div>
    </div>
  )
}

const verbs: Partial<Record<Action, { verb: string; ing: string }>> = {
  stop: { verb: 'Stop', ing: 'Stopping' },
  kill: { verb: 'Kill', ing: 'Killing' },
  pause: { verb: 'Pause', ing: 'Pausing' },
  remove: { verb: 'Remove', ing: 'Removing' },
  recreate: { verb: 'Pull & recreate', ing: 'Recreating' },
}

/**
 * The dialog for an action the agent refuses on TainerHub's own containers:
 * says why up front and how to do it on purpose, instead of letting it fail.
 */
function refusedDialog(c: Container, action: Action, own: OwnContainer) {
  const v = verbs[action]!
  if (action === 'recreate') {
    return {
      title: `Update ${c.name} from Hosts → Updates`,
      body: (
        <>
          {c.name} is {own.what}. Recreating it from here would stop it halfway through, so TainerHub's own containers are updated from the update card on the Hosts page.
        </>
      ),
      confirm: null,
    }
  }
  const cmds = ownServerCommands(c, action, own)
  return {
    title: `${v.verb} ${c.name} on the server instead`,
    body: (
      <>
        {c.name} is {own.what}.
        <OwnWarning>
          <p>
            {v.ing} it: {ownConsequence(own, c.hostName, action === 'remove')}
          </p>
          <p>That's why the agent on {c.hostName} won't do it from here.</p>
        </OwnWarning>
        <p className="mt-3">To do it on purpose, run this on {c.hostName}:</p>
        <Cmd>{cmds.run}</Cmd>
        <p className="mt-3">To bring it back:</p>
        <Cmd>{cmds.back}</Cmd>
      </>
    ),
    confirm: null,
  }
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
  const ownProject = useOwnProject()
  const m = useMutation({
    mutationFn: ({ c, action }: { c: Container; action: Action }) => api(`/api/hosts/${c.hostId}/containers/${encodeURIComponent(c.id)}/${action}`, { method: 'POST' }),
    onSuccess: (_, { c, action }) => {
      toast('good', `${c.name} ${pastTense[action]}`)
      qc.invalidateQueries({ queryKey: ['containers'] })
    },
    onError: (e, { c }) => toast('bad', `${c.name}: ${pullErrorText(e)}`),
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
      const own = takesDown(action) ? ownContainer(c, ownProject(c.hostId)) : null
      if (own && (own.refused || action === 'recreate')) {
        await confirm(refusedDialog(c, action, own))
        return false
      }
      // Containers in this one's network (a VPN gateway's apps) lose it
      // when it goes down, and can't follow it to a replacement.
      const netAction = action === 'stop' || action === 'kill' || action === 'restart' || action === 'remove' ? action : null
      const users = netAction || action === 'recreate' ? networkUsers(c, qc.getQueryData<Container[]>(['containers']) ?? []) : []
      if (users.length > 0 && action === 'recreate' && (!c.project || c.solo)) {
        await confirm({
          title: `Recreate ${c.name} together with ${users.length === 1 ? users[0] : 'the containers in its network'}`,
          body: (
            <>
              {users.join(', ')} {users.length === 1 ? 'runs' : 'run'} in {c.name}'s network. A new {c.name} would get a new network, and Docker can't move {users.length === 1 ? 'it' : 'them'} over, so the agent won't replace it on its own.
              <p className="mt-3">Put them in one compose stack (the gateway with <span className="mono">network_mode: "service:{c.name}"</span> on the others) and use the stack's Pull & redeploy, which recreates them together.</p>
            </>
          ),
          confirm: null,
        })
        return false
      }
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
        // Pausing only asks for TainerHub's own containers (below).
        ...(own ? { pause: { title: `Pause ${c.name}?`, body: 'Its processes are frozen until you resume it.', confirm: 'Pause' } } : {}),
        // Restarting only asks when other containers use its network (below).
        ...(users.length > 0 ? { restart: { title: `Restart ${c.name}?`, body: `It stops and starts again on ${c.hostName}.`, confirm: 'Restart' } } : {}),
      }
      const d = destructive[action]
      if (d) {
        // TainerHub's own containers the agent would let through (it can't
        // tell they're TainerHub's): say what breaks, and ask to tick a box.
        const warn = (own || (netAction && users.length > 0)) && (
          <OwnWarning>
            {own && (
              <p>
                This is {own.what}. {ownConsequence(own, c.hostName, action === 'remove')}
              </p>
            )}
            {netAction && users.length > 0 && <p>{sharedNetConsequence(users, netAction)}</p>}
          </OwnWarning>
        )
        const ack = own ? (own.kind === 'agent' ? `I understand this cuts ${c.hostName} off from TainerHub.` : 'I understand this can take TainerHub down for everyone.') : undefined
        const me = qc.getQueryData<Me>(['me'])
        if (action === 'remove' && can(me?.user, 'admin')) {
          volumeChoice.current = { on: false, names: [] }
          // The checkbox below decides about volumes, so don't promise they're kept.
          const adminBody = d.body.replace(' Its named volumes and image are kept.', ' Its image is kept.')
          const full = (
            <>
              {adminBody}
              {warn}
              <RemoveVolumesOption
                c={c}
                onChange={(v) => {
                  volumeChoice.current = v
                }}
              />
            </>
          )
          if (!(await confirm({ ...d, body: full, danger: true, ack }))) return false
          if (volumeChoice.current.on) {
            removeWithVolumes({ c, names: volumeChoice.current.names })
            return true
          }
          mutate({ c, action })
          return true
        }
        const body = (
          <>
            {d.body}
            {warn}
          </>
        )
        if (!(await confirm({ ...d, body, danger: action !== 'recreate' || !!own, ack }))) return false
      }
      mutate({ c, action })
      return true
    },
    [confirm, mutate, qc, removeWithVolumes, ownProject],
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
  storage: byStorage,
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
