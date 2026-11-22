import { useMemo, useState } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { ArrowDown, Boxes, X, ChevronDown, Download, RefreshCw, SquareTerminal, ChevronsDownUp, ChevronsUpDown, Ellipsis, Info, Pause, Play, Plus, RotateCw, ScrollText, Search, Square, Trash2, WifiOff, Zap } from 'lucide-react'
import { api, can, hostAddress, isUnhealthy, mem, publishedPort, type Container, type Host } from '../api'
import { loadPref, savePref, sortFns, useContainerAction, useFleet, useOpenDetails, useOpenLogs, useOpenTerminal, type Action, type SortKey } from '../fleet'
import { Button, IconButton, Menu, StatusDot, cx, toneOf, useToast } from '../ui'
import { useMe } from './AppLayout'
import { StackLink } from '../stacks'

type View = 'all' | 'running' | 'stopped' | 'issues' | 'updates'
type Group = 'host' | 'stack' | 'state'

type Section = { key: string; title: string; subtitle: string; items: Container[]; host?: Host; accent: string }

// One grid for the header and every row, so columns line up.
const COLS = 'md:grid md:grid-cols-[minmax(170px,1.5fr)_minmax(140px,1.3fr)_minmax(110px,1fr)_120px_64px_76px_168px] md:items-center md:gap-4'

/** Folded sections, remembered per browser (keys like "host:<id>", "stack:<name>"). */
function useCollapsed() {
  const [set, setSet] = useState<Set<string>>(() => {
    try {
      return new Set(JSON.parse(localStorage.getItem('th.collapsed') ?? '[]') as string[])
    } catch {
      return new Set()
    }
  })
  const save = (next: Set<string>) => {
    setSet(next)
    try {
      localStorage.setItem('th.collapsed', JSON.stringify([...next]))
    } catch {
      /* storage unavailable: fine */
    }
  }
  const toggle = (k: string) => {
    const next = new Set(set)
    if (next.has(k)) next.delete(k)
    else next.add(k)
    save(next)
  }
  return { collapsed: set, toggle, setAll: (keys: string[], on: boolean) => save(on ? new Set([...set, ...keys]) : new Set([...set].filter((k) => !keys.includes(k)))) }
}

export function ContainersPage() {
  const me = useMe()
  const navigate = useNavigate()
  const { containers, hosts } = useFleet()
  const search = useSearch({ strict: false }) as { host?: string; view?: string }
  const views: View[] = ['all', 'running', 'stopped', 'issues', 'updates']
  const [view, setView] = useState<View>(views.includes(search.view as View) ? (search.view as View) : 'all')
  const onlyHost = hosts.find((h) => h.id === search.host)
  const [group, setGroup] = useState<Group>(() => loadPref('th.group', ['host', 'stack', 'state'] as const, 'host'))
  const [sort, setSort] = useState<SortKey>(() => loadPref('th.csort', ['status', 'name', 'cpu', 'memory'] as const, 'status'))
  const [q, setQ] = useState('')
  const { collapsed, toggle, setAll } = useCollapsed()
  const operator = can(me.user, 'operator')
  const admin = can(me.user, 'admin')

  const scoped = onlyHost ? containers.filter((c) => c.hostId === onlyHost.id) : containers
  const counts = {
    all: scoped.length,
    running: scoped.filter((c) => c.state === 'running').length,
    stopped: scoped.filter((c) => c.state !== 'running').length,
    issues: scoped.filter(isUnhealthy).length,
    updates: scoped.filter((c) => c.update).length,
  }

  const sections = useMemo<Section[]>(() => {
    const f = q.trim().toLowerCase()
    const list = containers
      .filter((c) => {
        if (onlyHost && c.hostId !== onlyHost.id) return false
        if (view === 'running' && c.state !== 'running') return false
        if (view === 'stopped' && c.state === 'running') return false
        if (view === 'issues' && !isUnhealthy(c)) return false
        if (view === 'updates' && !c.update) return false
        return !f || [c.name, c.image, c.project ?? '', c.hostName].some((s) => s.toLowerCase().includes(f))
      })
      .sort(sortFns[sort])
    if (group === 'host') {
      return hosts.filter((h) => !onlyHost || h.id === onlyHost.id).map((h) => ({
        key: 'host:' + h.id,
        title: h.name,
        subtitle: h.online && h.hello ? [hostAddress(h), h.hello.os, `${h.hello.ncpu} cores`, mem(h.hello.mem_total)].filter(Boolean).join(' · ') : 'offline',
        items: list.filter((c) => c.hostId === h.id),
        host: h,
        accent: h.online ? 'bg-lime-400' : 'bg-ink-600',
      }))
    }
    if (group === 'stack') {
      const by = new Map<string, Container[]>()
      for (const c of list) by.set(c.project || '', [...(by.get(c.project || '') ?? []), c])
      return [...by.entries()]
        .sort(([a], [b]) => (a === '' ? 1 : b === '' ? -1 : a.localeCompare(b)))
        .map(([k, items]) => ({
          key: 'stack:' + (k || '_none'),
          title: k || 'Not in a stack',
          subtitle: k ? `on ${[...new Set(items.map((c) => c.hostName))].join(', ')}` : 'standalone containers',
          items,
          accent: k ? 'bg-cyan-400' : 'bg-ink-600',
        }))
    }
    const states: [string, string, string, (c: Container) => boolean][] = [
      ['issues', 'Needs attention', 'bg-rose-400', isUnhealthy],
      ['running', 'Running', 'bg-emerald-400', (c) => c.state === 'running' && !isUnhealthy(c)],
      ['stopped', 'Stopped and paused', 'bg-ink-500', (c) => c.state !== 'running' && !isUnhealthy(c)],
    ]
    return states.map(([key, title, accent, fn]) => ({ key: 'state:' + key, title, subtitle: '', items: list.filter(fn), accent })).filter((s) => s.items.length > 0)
  }, [containers, hosts, view, group, q, sort, onlyHost])

  const pickSort = (k: SortKey) => {
    setSort(k)
    savePref('th.csort', k)
  }
  const SortHead = ({ k, label, right }: { k: SortKey; label: string; right?: boolean }) => (
    <button onClick={() => pickSort(k)} className={cx('flex items-center gap-1 uppercase tracking-wider hover:text-white', right && 'justify-end', sort === k && 'text-lime-300')}>
      {label}
      {sort === k && <ArrowDown className="size-3" />}
    </button>
  )

  return (
    <div className="animate-[rise_300ms_ease-out]">
      <div className="mb-5 flex flex-wrap items-center gap-2.5">
        <h1 className="mr-3 font-display text-3xl font-bold tracking-tight text-white">Containers</h1>
        {(
          [
            ['all', 'All', ''],
            ['running', 'Running', 'text-emerald-300'],
            ['stopped', 'Stopped', 'text-ink-300'],
            ['issues', 'Issues', 'text-rose-300'],
            ['updates', 'Updates', 'text-lime-300'],
          ] as const
        )
          .filter(([k]) => k !== 'updates' || counts.updates > 0 || view === 'updates')
          .map(([k, label, dot]) => (
          <button
            key={k}
            onClick={() => setView(k)}
            className={cx(
              'flex items-center gap-1.5 rounded-[2px] px-3 py-1.5 font-display text-[12px] font-semibold uppercase tracking-[0.08em] transition',
              view === k ? 'bg-lime-400 text-ink-950' : 'text-ink-100/75 ring-1 ring-inset ring-line hover:text-white hover:ring-lime-400/40',
            )}
          >
            {dot && <span className={cx('text-[10px]', view === k ? 'text-ink-950' : dot)}>●</span>}
            {label}
            <span className="tabular-nums opacity-80">{counts[k]}</span>
          </button>
        ))}
        {onlyHost && (
          <button
            onClick={() => navigate({ to: '/containers', search: {} })}
            title="Show all hosts"
            className="flex items-center gap-1.5 rounded-[2px] bg-lime-400/10 px-3 py-1.5 font-display text-[12px] font-semibold uppercase tracking-[0.08em] text-lime-200 ring-1 ring-inset ring-lime-400/40 hover:bg-lime-400/20"
          >
            Host: {onlyHost.name}
            <X className="size-3.5" />
          </button>
        )}
        <div className="ml-auto flex flex-wrap items-center gap-2.5">
          <div className="relative">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-ink-100/50" />
            <input
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Filter"
              aria-label="Filter containers"
              className="w-44 rounded-[2px] bg-ink-950/70 py-1.5 pl-9 pr-3 text-sm text-white ring-1 ring-inset ring-white/12 outline-none placeholder:text-ink-100/35 focus:ring-lime-400/60"
            />
          </div>
          <div className="flex items-center gap-1 rounded-[3px] bg-ink-950/60 p-0.5 text-xs ring-1 ring-line">
            <span className="px-2 text-ink-100/60">Group by</span>
            {(['host', 'stack', 'state'] as const).map((g) => (
              <button
                key={g}
                onClick={() => {
                  setGroup(g)
                  savePref('th.group', g)
                }}
                className={cx('rounded-[2px] px-3 py-1 font-semibold capitalize transition', group === g ? 'bg-lime-400 text-ink-950' : 'text-ink-100/80 hover:text-white')}
              >
                {g}
              </button>
            ))}
          </div>
          {operator && <CheckUpdatesButton hosts={hosts} />}
          {admin && (
            <Button variant="primary" icon={Plus} onClick={() => navigate({ to: '/containers/new' })}>
              New container
            </Button>
          )}
        </div>
      </div>

      <div className="overflow-hidden rounded-md ring-1 ring-line">
        <div className={cx('hidden border-b border-line bg-ink-900/80 px-4 py-2 font-mono text-[10.5px] text-ink-100/50', COLS)}>
          <SortHead k="name" label="Container" />
          <span className="uppercase tracking-wider">Image</span>
          <span className="uppercase tracking-wider">Ports</span>
          <SortHead k="status" label="Status" />
          <SortHead k="cpu" label="CPU" right />
          <SortHead k="memory" label="Memory" right />
          <span className="flex justify-end">
            {sections.length > 1 && (
              <button
                onClick={() => {
                  const keys = sections.map((s) => s.key)
                  setAll(keys, !keys.every((k) => collapsed.has(k)))
                }}
                className="flex items-center gap-1 uppercase tracking-wider hover:text-white"
              >
                {sections.every((s) => collapsed.has(s.key)) ? <ChevronsUpDown className="size-3" /> : <ChevronsDownUp className="size-3" />}
                {sections.every((s) => collapsed.has(s.key)) ? 'Expand all' : 'Collapse all'}
              </button>
            )}
          </span>
        </div>
        {sections.length === 0 && <p className="px-4 py-10 text-center text-sm text-ink-100/50">{containers.length ? 'Nothing matches.' : 'No containers yet.'}</p>}
        {sections.map((s) => {
          const folded = collapsed.has(s.key)
          const issues = s.items.filter(isUnhealthy).length
          return (
          <section key={s.key}>
            <button
              onClick={() => toggle(s.key)}
              aria-expanded={!folded}
              className="flex w-full items-center gap-3 border-b border-line bg-ink-900/50 px-4 py-2.5 text-left transition hover:bg-ink-900/80"
            >
              <ChevronDown className={cx('size-4 shrink-0 text-ink-100/50 transition', folded && '-rotate-90')} />
              <span className={cx('h-4 w-[3px] shrink-0', s.accent)} />
              <h2 className="truncate font-display text-[15px] font-bold text-white">{s.title}</h2>
              {s.subtitle && <span className="hidden truncate text-xs text-ink-100/50 sm:inline">{s.subtitle}</span>}
              {folded && issues > 0 && <span className="shrink-0 font-mono text-[11px] uppercase text-rose-300">{issues} need attention</span>}
              <span className="ml-auto shrink-0 font-mono text-xs tabular-nums text-ink-100/60">
                {s.items.filter((c) => c.state === 'running').length}/{s.items.length} running
              </span>
            </button>
            {folded ? null : s.host && !s.host.online ? (
              <div className="flex items-center gap-2 border-b border-line px-4 py-4 text-sm text-ink-100/50">
                <WifiOff className="size-4" /> Agent offline: its containers aren't visible right now.
              </div>
            ) : s.items.length === 0 ? (
              <div className="border-b border-line px-4 py-4 text-sm text-ink-100/50">No containers{q || view !== 'all' ? ' match' : ''}.</div>
            ) : (
              s.items.map((c) => <Row key={c.hostId + c.id} c={c} operator={operator} showHost={group !== 'host'} />)
            )}
          </section>
          )
        })}
      </div>
    </div>
  )
}

function stateLabel(c: Container) {
  if (isUnhealthy(c)) return c.status.includes('unhealthy') ? 'unhealthy' : c.state === 'exited' ? 'failed' : c.state
  if (c.state === 'running') return 'running'
  if (c.state === 'paused') return 'paused'
  return 'stopped'
}

function Row({ c, operator, showHost }: { c: Container; operator: boolean; showHost: boolean }) {
  const navigate = useNavigate()
  const { hosts } = useFleet()
  const host = hosts.find((h) => h.id === c.hostId)
  const openLogs = useOpenLogs()
  const openTerminal = useOpenTerminal()
  const admin = can(useMe().user, 'admin')
  const openDetails = useOpenDetails()
  const { run, pendingId } = useContainerAction()
  const bad = isUnhealthy(c)
  const running = c.state === 'running'
  const paused = c.state === 'paused'
  const busy = pendingId === c.id
  const act = (a: Action) => run(c, a)
  const ports = (c.ports ?? []).filter((p) => p.includes('->'))
  const uptime = c.status.replace(/\s*\((un)?healthy\)/, '')

  return (
    <div className={cx('group border-b border-line px-4 py-2.5 text-sm transition last:border-b-0 hover:bg-white/[0.025]', bad && 'bg-rose-500/[0.06]', COLS)}>
      {/* name, stack, host */}
      <div className="flex min-w-0 items-center gap-2.5">
        <StatusDot tone={toneOf(c.state, c.status)} />
        <div className="min-w-0">
          <button onClick={() => openDetails(c)} className="block max-w-full truncate text-left font-semibold text-white hover:text-lime-200">
            {c.name}
          </button>
          {(c.project || showHost) && (
            <div className="flex min-w-0 items-center gap-1 truncate text-xs text-ink-100/50">
              <StackLink c={c} />
              {showHost && <span className="truncate">{c.project ? ' · ' : ''}{c.hostName}</span>}
            </div>
          )}
        </div>
        {/* phones: state + actions on the first line */}
        <span className="ml-auto shrink-0 font-mono text-[11px] uppercase text-ink-100/60 md:hidden">{stateLabel(c)}</span>
      </div>
      <div className="mt-1 flex min-w-0 items-center gap-1.5 md:mt-0">
        <span className="mono truncate text-xs text-ink-100/60" title={c.image}>
          {c.image}
        </span>
        {c.update && (
          <span title="A newer image is available for this tag" className="shrink-0 rounded-[2px] bg-lime-400/15 px-1 py-[2px] font-mono text-[10px] uppercase leading-none tracking-wider text-lime-300 ring-1 ring-inset ring-lime-400/30">
            update
          </span>
        )}
      </div>
      <div className="mono hidden truncate text-xs text-ink-100/70 md:block" title={ports.join(', ')}>
        {ports.length === 0 ? (
          <span className="text-ink-100/30">–</span>
        ) : (
          <>
            {ports.slice(0, 2).map((p, i) => (
              <span key={p}>
                {i > 0 && ', '}
                <PortLink port={p} host={host} />
              </span>
            ))}
            {ports.length > 2 && ` +${ports.length - 2}`}
          </>
        )}
      </div>
      <div className="hidden min-w-0 md:block">
        <div className={cx('font-mono text-[11px] uppercase', bad ? 'text-rose-300' : running ? 'text-emerald-300' : paused ? 'text-amber-300' : 'text-ink-100/50')}>{stateLabel(c)}</div>
        <div className="truncate text-xs text-ink-100/50">{uptime}</div>
      </div>
      <div className={cx('mono hidden text-right text-xs tabular-nums md:block', (c.cpu ?? 0) >= 50 ? 'text-rose-300' : 'text-ink-100/80')}>{running && c.cpu !== undefined ? `${c.cpu.toFixed(1)}%` : '–'}</div>
      <div className="mono hidden text-right text-xs tabular-nums text-ink-100/80 md:block">{running ? mem(c.mem) : '–'}</div>
      <div className="mt-1.5 flex items-center justify-end gap-0.5 md:mt-0">
        <IconButton icon={ScrollText} label="Logs" onClick={() => openLogs(c)} />
        {operator && (
          <>
            {running ? (
              <IconButton icon={Square} label="Stop" tone="warn" loading={busy} onClick={() => act('stop')} />
            ) : (
              <IconButton icon={Play} label={paused ? 'Resume' : 'Start'} tone="good" loading={busy} onClick={() => act(paused ? 'unpause' : 'start')} />
            )}
            <IconButton icon={RotateCw} label="Restart" disabled={busy} onClick={() => act('restart')} />
            <IconButton icon={Trash2} label="Remove" tone="bad" disabled={busy} onClick={() => act('remove')} />
            <Menu
              trigger={(toggle) => <IconButton icon={Ellipsis} label="More actions" disabled={busy} onClick={toggle} />}
              items={[
                { label: 'Details', icon: Info, onClick: () => openDetails(c) },
                { label: 'Terminal', icon: SquareTerminal, onClick: () => openTerminal(c), hidden: !admin || !running },
                { label: 'Pause', icon: Pause, onClick: () => act('pause'), hidden: !running },
                { label: 'Resume', icon: Play, onClick: () => act('unpause'), hidden: !paused },
                { label: 'Kill', icon: Zap, onClick: () => act('kill'), danger: true, hidden: !running },
                { label: 'Pull & recreate', icon: Download, onClick: () => act('recreate'), hidden: !!c.project || c.name.startsWith('tainerhub') },
                {
                  label: 'Update via its stack',
                  icon: Boxes,
                  onClick: () => navigate({ to: '/stacks', search: { q: c.project } }),
                  hidden: !c.project || c.name.startsWith('tainerhub'),
                },
              ]}
            />
          </>
        )}
        {!operator && <IconButton icon={Info} label="Details" onClick={() => openDetails(c)} />}
      </div>
    </div>
  )
}

/** Asks every online host to check for newer images now. */
function CheckUpdatesButton({ hosts }: { hosts: Host[] }) {
  const toast = useToast()
  const qc = useQueryClient()
  const [busy, setBusy] = useState(false)
  const run = async () => {
    setBusy(true)
    let found = 0
    const failed: string[] = []
    await Promise.all(
      hosts
        .filter((h) => h.online)
        .map(async (h) => {
          try {
            const r = await api<{ images: Record<string, boolean> }>(`/api/hosts/${h.id}/check-updates`, { method: 'POST' })
            found += Object.values(r.images ?? {}).filter(Boolean).length
          } catch {
            failed.push(h.name)
          }
        }),
    )
    setBusy(false)
    setTimeout(() => qc.invalidateQueries({ queryKey: ['containers'] }), 1500)
    toast(failed.length ? 'bad' : 'good', `${found ? `${found} newer image${found === 1 ? '' : 's'} available` : 'Everything is up to date'}${failed.length ? ` (couldn't check ${failed.join(', ')})` : ''}`)
  }
  return (
    <Button icon={RefreshCw} loading={busy} onClick={run} title="Ask each host's registry for newer images now (also happens every 6 hours)">
      Check for updates
    </Button>
  )
}

/** A published port as a link to http://<host>:<port>. */
export function PortLink({ port, host }: { port: string; host?: Host }) {
  const addr = hostAddress(host)
  const pub = publishedPort(port)
  if (!addr || !pub) return <>{port}</>
  return (
    <a href={`http://${addr.includes(':') ? `[${addr}]` : addr}:${pub}`} target="_blank" rel="noreferrer" className="hover:text-lime-200 hover:underline" title={`Open http://${addr}:${pub}`}>
      {port}
    </a>
  )
}
