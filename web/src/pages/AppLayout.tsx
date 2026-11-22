import { Link, Navigate, Outlet, useNavigate } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createContext, useCallback, useContext, useEffect, useState } from 'react'
import { Fingerprint, LogOut, Search, TriangleAlert } from 'lucide-react'
import { api, ApiError, can, type Activity, type Container, type Host, type Me } from '../api'
import { DetailsContext, LogsContext, PaletteContext } from '../fleet'
import { Kbd, Menu, cx } from '../ui'
import { CommandPalette } from './CommandPalette'
import { Logo } from './Logo'
import { LogsPanel } from './LogsPanel'
import { DetailsPanel } from './DetailsPanel'
import { JobContext, JobPanel } from '../stacks'

const MeContext = createContext<Me | null>(null)
export const useMe = () => useContext(MeContext)!

/** Subscribes to the hub's live fleet stream and writes it into the query cache. */
function useFleetStream() {
  const qc = useQueryClient()
  const [live, setLive] = useState(false)
  useEffect(() => {
    let es: EventSource | null = null
    let retry: ReturnType<typeof setTimeout>
    let delay = 1000
    const connect = () => {
      es = new EventSource('/api/events')
      es.addEventListener('fleet', (e) => {
        const d = JSON.parse((e as MessageEvent).data) as { hosts: Host[]; containers: Container[]; activity: Activity[] }
        qc.setQueryData(['hosts'], d.hosts)
        qc.setQueryData(['containers'], d.containers)
        qc.setQueryData(['activity'], d.activity)
        setLive(true)
        delay = 1000
      })
      es.onerror = () => {
        setLive(false)
        es?.close()
        retry = setTimeout(connect, delay)
        delay = Math.min(delay * 2, 30000)
      }
    }
    connect()
    return () => {
      clearTimeout(retry)
      es?.close()
    }
  }, [qc])
  return live
}

type NavTo = '/' | '/containers' | '/stacks' | '/resources' | '/hosts' | '/audit'

function NavPill({ to, label, exact }: { to: NavTo; label: string; exact?: boolean }) {
  return (
    <Link
      to={to}
      activeOptions={{ exact }}
      className="whitespace-nowrap rounded-full px-4 py-2 text-sm font-medium text-indigo-100/70 transition hover:text-white"
      activeProps={{ className: '!bg-gradient-to-r !from-sky-500 !to-purple-500 !font-semibold !text-white shadow-lg shadow-purple-900/40' }}
    >
      {label}
    </Link>
  )
}

export function AppLayout() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const me = useQuery({ queryKey: ['me'], queryFn: () => api<Me>('/api/me') })
  const live = useFleetStream()
  const [logsFor, setLogsFor] = useState<Container | null>(null)
  const [detailsFor, setDetailsFor] = useState<Container | null>(null)
  const [paletteOpen, setPaletteOpen] = useState(false)
  const openPalette = useCallback(() => setPaletteOpen(true), [])
  const [jobId, setJobId] = useState<string | null>(null)
  // The logs and job drawers share the bottom of the screen: one at a time.
  const openJob = useCallback((id: string) => {
    setLogsFor(null)
    setJobId(id)
  }, [])
  const openLogs = useCallback((c: Container) => {
    setJobId(null)
    setLogsFor(c)
  }, [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const typing = ['INPUT', 'TEXTAREA'].includes((document.activeElement as HTMLElement | null)?.tagName ?? '')
      if ((e.key === 'k' && (e.metaKey || e.ctrlKey)) || (e.key === '/' && !typing)) {
        e.preventDefault()
        setPaletteOpen(true)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  if (me.isPending) return <div className="flex min-h-screen items-center justify-center text-sm text-indigo-100/60">Loading…</div>
  if (me.isError) {
    if (me.error instanceof ApiError && me.error.status === 401) return <Navigate to="/login" />
    return <div className="p-8 text-sm text-rose-300">{me.error.message}</div>
  }

  const logout = async () => {
    await api('/api/logout', { method: 'POST' })
    qc.clear()
    navigate({ to: '/login' })
  }
  const initials = me.data.user.displayName
    .split(/\s+/)
    .map((p) => p[0])
    .join('')
    .slice(0, 2)
    .toUpperCase()

  return (
    <MeContext.Provider value={me.data}>
      <LogsContext.Provider value={openLogs}>
        <JobContext.Provider value={openJob}>
        <DetailsContext.Provider value={setDetailsFor}>
        <PaletteContext.Provider value={openPalette}>
          <div className={cx('min-h-screen', (logsFor || jobId) && 'pb-[48vh]')}>
            <header className="sticky top-0 z-30 border-b border-white/[0.06] bg-[#0a0720]/60 backdrop-blur-xl">
              <div className="mx-auto flex max-w-[1500px] flex-wrap items-center gap-3 px-4 py-3 sm:px-8">
                <Link to="/" aria-label="TainerHub home">
                  <Logo />
                </Link>
                <nav className="order-3 flex w-full gap-1 overflow-x-auto rounded-full bg-white/[0.06] p-1 ring-1 ring-white/10 md:order-none md:ml-4 md:w-auto">
                  <NavPill to="/" label="Overview" exact />
                  <NavPill to="/containers" label="Containers" />
                  <NavPill to="/stacks" label="Stacks" />
                  <NavPill to="/resources" label="Resources" />
                  <NavPill to="/hosts" label="Hosts" />
                  {can(me.data.user, 'admin') && <NavPill to="/audit" label="Audit" />}
                </nav>
                <button
                  onClick={openPalette}
                  className="group ml-auto flex min-w-0 flex-1 items-center gap-2 rounded-full bg-white/[0.06] px-4 py-2.5 text-left text-sm text-indigo-100/60 ring-1 ring-white/10 transition hover:bg-white/[0.09] md:ml-0 md:max-w-xl"
                >
                  <Search className="size-4 shrink-0" />
                  <span className="truncate">
                    Search or run a command, e.g. <span className="mono text-sky-300">restart pihole</span>
                  </span>
                  <span className="ml-auto hidden sm:block">
                    <Kbd>⌘K</Kbd>
                  </span>
                </button>
                <span
                  className={cx('hidden size-2 rounded-full lg:block', live ? 'bg-emerald-400 shadow-[0_0_8px_2px] shadow-emerald-500/60' : 'animate-pulse bg-amber-400')}
                  title={live ? 'Live: receiving updates from the hub' : 'Reconnecting to the live update stream'}
                />
                <Menu
                  trigger={(toggle) => (
                    <button
                      onClick={toggle}
                      aria-label="Account menu"
                      className="flex size-10 items-center justify-center rounded-full bg-gradient-to-br from-fuchsia-500 via-purple-500 to-sky-400 text-sm font-bold text-white shadow-lg shadow-purple-900/40 ring-2 ring-white/15"
                    >
                      {initials}
                    </button>
                  )}
                  items={[
                    { label: 'Account & passkeys', icon: Fingerprint, onClick: () => navigate({ to: '/account' }) },
                    { label: 'Sign out', icon: LogOut, onClick: logout },
                  ]}
                />
              </div>
            </header>

            {me.data.passkeys.length === 0 && (
              <div className="flex items-center justify-center gap-2 bg-amber-500/15 px-4 py-2 text-center text-sm text-amber-100">
                <TriangleAlert className="size-4" />
                You have no passkeys.{' '}
                <Link to="/account" className="font-semibold underline underline-offset-2">
                  Add one now
                </Link>
              </div>
            )}
            <main className="mx-auto max-w-[1500px] px-4 py-8 sm:px-8">
              <Outlet />
            </main>
          </div>
          {detailsFor && <LiveDetails c={detailsFor} onClose={() => setDetailsFor(null)} />}
          {logsFor && <LogsPanel container={logsFor} onClose={() => setLogsFor(null)} />}
          {jobId && <JobPanel jobId={jobId} onClose={() => setJobId(null)} />}
          <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} onLogs={openLogs} onDetails={setDetailsFor} />
        </PaletteContext.Provider>
        </DetailsContext.Provider>
        </JobContext.Provider>
      </LogsContext.Provider>
    </MeContext.Provider>
  )
}

/** Feeds the details panel the latest live copy of the container (state, CPU, memory). */
function LiveDetails({ c, onClose }: { c: Container; onClose: () => void }) {
  const live = useQuery({ queryKey: ['containers'], queryFn: () => api<Container[]>('/api/containers'), enabled: false }).data?.find(
    (x) => x.hostId === c.hostId && x.id === c.id,
  )
  return <DetailsPanel container={live ?? c} onClose={onClose} />
}
