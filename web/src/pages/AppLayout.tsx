import { Link, Navigate, Outlet, useNavigate } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createContext, useCallback, useContext, useEffect, useState } from 'react'
import { Fingerprint, LogOut, Search, TriangleAlert } from 'lucide-react'
import { api, ApiError, can, errorText, type Activity, type Container, type Host, type Me } from '../api'
import { DetailsContext, LiveContext, LogsContext, PaletteContext, TerminalContext } from '../fleet'
import { Kbd, Menu, cx, useToast } from '../ui'
import { ReauthPrompt } from '../reauth'
import { CommandPalette } from './CommandPalette'
import { Logo } from './Logo'
import { LogsPanel } from './LogsPanel'
import { DetailsPanel } from './DetailsPanel'
import { JobContext, JobPanel } from '../stacks'
import { lazy, Suspense } from 'react'

// xterm.js is big: load the terminal only when one is opened.
const TerminalPanel = lazy(() => import('./TerminalPanel').then((m) => ({ default: m.TerminalPanel })))

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
        let d: { hosts: Host[]; containers: Container[]; activity: Activity[] }
        try {
          d = JSON.parse((e as MessageEvent).data)
        } catch {
          return // a garbled event: wait for the next one
        }
        qc.setQueryData(['hosts'], d.hosts)
        qc.setQueryData(['containers'], d.containers)
        qc.setQueryData(['activity'], d.activity)
        setLive(true)
        delay = 1000
      })
      es.onerror = () => {
        setLive(false)
        es?.close()
        // EventSource can't show the status: if the session expired, /api/me
        // answers 401 and AppLayout sends the user to sign in.
        qc.invalidateQueries({ queryKey: ['me'] })
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

type NavTo = '/' | '/containers' | '/stacks' | '/resources' | '/hosts' | '/users' | '/audit' | '/settings'

function NavPill({ to, label, exact }: { to: NavTo; label: string; exact?: boolean }) {
  return (
    <Link
      to={to}
      activeOptions={{ exact }}
      className="relative whitespace-nowrap px-3 py-2 font-display text-[12.5px] font-medium uppercase tracking-[0.1em] text-ink-100/55 transition hover:text-white"
      activeProps={{ className: '!text-lime-300 after:absolute after:inset-x-3 after:bottom-0 after:h-[2px] after:bg-lime-400' }}
    >
      {label}
    </Link>
  )
}

export function AppLayout() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const toast = useToast()
  const me = useQuery({ queryKey: ['me'], queryFn: () => api<Me>('/api/me') })
  const live = useFleetStream()
  const [logsFor, setLogsFor] = useState<Container | null>(null)
  const [detailsFor, setDetailsFor] = useState<Container | null>(null)
  const [paletteOpen, setPaletteOpen] = useState(false)
  const openPalette = useCallback(() => setPaletteOpen(true), [])
  const closePalette = useCallback(() => setPaletteOpen(false), [])
  const [jobId, setJobId] = useState<string | null>(null)
  const [termFor, setTermFor] = useState<Container | null>(null)
  // The logs, job and terminal drawers share the bottom of the screen: one at a time.
  const openJob = useCallback((id: string) => {
    setLogsFor(null)
    setTermFor(null)
    setJobId(id)
  }, [])
  const openLogs = useCallback((c: Container) => {
    setJobId(null)
    setTermFor(null)
    setLogsFor(c)
  }, [])
  const openTerminal = useCallback((c: Container) => {
    setJobId(null)
    setLogsFor(null)
    setDetailsFor(null)
    setTermFor(c)
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

  if (me.isPending) return <div className="flex min-h-screen items-center justify-center text-sm text-ink-100/60">Loading…</div>
  if (me.isError && me.error instanceof ApiError && me.error.status === 401) {
    // Back to this page after signing in (Login only follows same-site paths).
    const here = window.location.pathname + window.location.search
    return <Navigate to="/login" search={{ next: here === '/' ? undefined : here }} />
  }
  // A failed refetch (a network blip) keeps showing the app with the data it has.
  if (!me.data) return <div className="p-8 text-sm text-rose-300">{me.error?.message ?? 'Loading…'}</div>

  const logout = async () => {
    try {
      await api('/api/logout', { method: 'POST' })
    } catch (e) {
      toast('bad', `Couldn't sign out: ${errorText(e)}`)
      return
    }
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
      <LiveContext.Provider value={live}>
      <LogsContext.Provider value={openLogs}>
        <JobContext.Provider value={openJob}>
        <TerminalContext.Provider value={openTerminal}>
        <DetailsContext.Provider value={setDetailsFor}>
        <PaletteContext.Provider value={openPalette}>
          <div className={cx('min-h-screen', (logsFor || jobId) && 'pb-[48vh]', termFor && 'pb-[54vh]')}>
            <header className="sticky top-0 z-30 border-b border-line bg-canvas/85 backdrop-blur-md">
              <div className="mx-auto flex max-w-[1500px] flex-wrap items-center gap-3 px-4 py-3 sm:px-8">
                <Link to="/" aria-label="TainerHub home">
                  <Logo />
                </Link>
                <nav className="order-3 flex w-full gap-0.5 overflow-x-auto lg:order-none lg:ml-4 lg:w-auto">
                  <NavPill to="/" label="Overview" exact />
                  <NavPill to="/containers" label="Containers" />
                  <NavPill to="/stacks" label="Stacks" />
                  <NavPill to="/resources" label="Resources" />
                  <NavPill to="/hosts" label="Hosts" />
                  {can(me.data.user, 'admin') && <NavPill to="/users" label="Users" />}
                  {can(me.data.user, 'admin') && <NavPill to="/audit" label="Audit" />}
                  {can(me.data.user, 'admin') && <NavPill to="/settings" label="Settings" />}
                </nav>
                <button
                  onClick={openPalette}
                  className="group ml-auto flex min-w-0 flex-1 items-center gap-2 rounded-[2px] bg-ink-950/70 px-3.5 py-2 text-left text-sm text-ink-100/55 ring-1 ring-line transition hover:ring-lime-400/40 lg:ml-0 lg:max-w-xl"
                >
                  <Search className="size-4 shrink-0" />
                  <span className="truncate">
                    Search or run a command, e.g. <span className="mono text-lime-300">restart pihole</span>
                  </span>
                  <span className="ml-auto hidden sm:block">
                    <Kbd>⌘K</Kbd>
                  </span>
                </button>
                <span
                  className={cx('hidden size-2 rounded-full lg:block', live ? 'bg-emerald-400' : 'animate-pulse bg-amber-400')}
                  title={live ? 'Live: receiving updates from the hub' : 'Reconnecting to the live update stream'}
                />
                <Menu
                  trigger={(toggle, menu) => (
                    <button
                      {...menu}
                      onClick={toggle}
                      aria-label="Account menu"
                      className="tag tag-sm flex size-9 items-center justify-center bg-lime-400 font-display text-sm font-bold text-ink-950 transition hover:bg-lime-300"
                    >
                      {initials}
                    </button>
                  )}
                  items={[
                    { label: 'Account & sign-in', icon: Fingerprint, onClick: () => navigate({ to: '/account' }) },
                    { label: 'Sign out', icon: LogOut, onClick: logout },
                  ]}
                />
              </div>
            </header>

            {me.data.passkeysEnabled && me.data.passkeys.length === 0 && (
              <div className="flex items-center justify-center gap-2 bg-amber-500/15 px-4 py-2 text-center text-sm text-amber-100">
                <TriangleAlert className="size-4" />
                Passkeys are available now: add one for faster, phishing-proof sign-in.{' '}
                {window.location.origin === me.data.publicURL ? (
                  <Link to="/account" className="font-semibold underline underline-offset-2">
                    Add one now
                  </Link>
                ) : (
                  <a href={me.data.publicURL + '/account'} className="font-semibold underline underline-offset-2">
                    Add one at {me.data.publicURL}
                  </a>
                )}
              </div>
            )}
            <main className="mx-auto max-w-[1500px] px-4 py-8 sm:px-8">
              <Outlet />
            </main>
          </div>
          {detailsFor && <LiveDetails c={detailsFor} onClose={() => setDetailsFor(null)} />}
          {logsFor && <LogsPanel container={logsFor} onClose={() => setLogsFor(null)} />}
          {jobId && <JobPanel jobId={jobId} onClose={() => setJobId(null)} />}
          {termFor && (
            <Suspense fallback={null}>
              <TerminalPanel key={termFor.hostId + termFor.id} container={termFor} onClose={() => setTermFor(null)} />
            </Suspense>
          )}
          <CommandPalette open={paletteOpen} onClose={closePalette} onLogs={openLogs} onDetails={setDetailsFor} />
          <ReauthPrompt />
        </PaletteContext.Provider>
        </DetailsContext.Provider>
        </TerminalContext.Provider>
        </JobContext.Provider>
      </LogsContext.Provider>
      </LiveContext.Provider>
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
