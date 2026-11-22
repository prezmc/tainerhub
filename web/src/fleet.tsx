import { createContext, useCallback, useContext } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, errorText, type Activity, type Container, type Host } from './api'
import { useConfirm, useToast } from './ui'

export type Action = 'start' | 'stop' | 'restart' | 'pause' | 'unpause' | 'kill' | 'remove'
export const ACTIONS: Action[] = ['start', 'stop', 'restart', 'pause', 'unpause', 'kill', 'remove']

const pastTense: Record<Action, string> = {
  start: 'started',
  stop: 'stopped',
  restart: 'restarted',
  pause: 'paused',
  unpause: 'resumed',
  kill: 'killed',
  remove: 'removed',
}

/** Live data: kept fresh by the SSE stream in AppLayout; polling is only a fallback. */
export function useFleet() {
  const containers = useQuery({ queryKey: ['containers'], queryFn: () => api<Container[]>('/api/containers'), refetchInterval: 30000 })
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: () => api<Host[]>('/api/hosts'), refetchInterval: 30000 })
  const activity = useQuery<Activity[]>({ queryKey: ['activity'], queryFn: () => Promise.resolve([]), staleTime: Infinity })
  return { containers: containers.data ?? [], hosts: hosts.data ?? [], activity: activity.data ?? [], loading: containers.isPending || hosts.isPending }
}

/** Runs a container action with confirmation for destructive ones and a toast for the result. */
/** Describes TainerHub's own containers, which are risky to stop or remove from TainerHub. */
function ownContainer(c: Container): string | null {
  if (c.name === 'tainerhub') return "TainerHub's hub"
  if (c.name === 'tainerhub-postgres') return "TainerHub's database"
  if (c.name === 'tainerhub-agent' || c.image.includes('tainerhub-agent')) return `the agent that connects ${c.hostName} to TainerHub`
  return null
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
  const run = useCallback(
    async (c: Container, action: Action): Promise<boolean> => {
      const own = ownContainer(c)
      const fromStack = c.project && !own ? ` It belongs to the ${c.project} stack, so redeploying that stack recreates it.` : ''
      const destructive: Partial<Record<Action, { title: string; body: string; confirm: string }>> = {
        stop: { title: `Stop ${c.name}?`, body: `It stays stopped on ${c.hostName} until you start it again.`, confirm: 'Stop' },
        kill: { title: `Kill ${c.name}?`, body: 'Sends SIGKILL immediately. The process gets no chance to shut down cleanly.', confirm: 'Kill' },
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
        if (!(await confirm({ ...d, body, danger: true }))) return false
      }
      m.mutate({ c, action })
      return true
    },
    [confirm, m],
  )
  return { run, pendingId: m.isPending ? m.variables?.c.id : undefined }
}

// ---- logs drawer, opened from anywhere ----

export const LogsContext = createContext<(c: Container) => void>(() => {})
export const useOpenLogs = () => useContext(LogsContext)

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
