import { useMemo, useState } from 'react'
import { Boxes, Ellipsis, Info, Pause, Play, RotateCw, ScrollText, Search, Square, Trash2, WifiOff, Zap } from 'lucide-react'
import { can, isUnhealthy, mem, type Container, type Host } from '../api'
import { loadPref, savePref, sortFns, useContainerAction, useFleet, useOpenDetails, useOpenLogs, type Action } from '../fleet'
import { IconButton, Menu, cx } from '../ui'
import { useMe } from './AppLayout'

type View = 'all' | 'running' | 'stopped' | 'issues'
type Group = 'host' | 'stack' | 'state'

type Column = { key: string; title: string; subtitle: string; count: number; tone: 'host' | 'offline' | 'stack' | 'good' | 'bad' | 'idle'; items: Container[]; host?: Host }

const headerTone: Record<Column['tone'], string> = {
  host: 'bg-gradient-to-br from-emerald-500 to-sky-500 shadow-emerald-900/40',
  offline: 'bg-white/[0.07]',
  stack: 'bg-gradient-to-br from-fuchsia-500 to-purple-600 shadow-purple-900/40',
  good: 'bg-gradient-to-br from-emerald-500 to-teal-500 shadow-emerald-900/40',
  bad: 'bg-gradient-to-br from-rose-500 to-orange-500 shadow-rose-900/40',
  idle: 'bg-gradient-to-br from-slate-500 to-slate-600',
}

export function ContainersPage() {
  const me = useMe()
  const { containers, hosts } = useFleet()
  const [view, setView] = useState<View>('all')
  const [group, setGroup] = useState<Group>(() => loadPref('th.group', ['host', 'stack', 'state'] as const, 'host'))
  const [q, setQ] = useState('')
  const operator = can(me.user, 'operator')

  const counts = {
    all: containers.length,
    running: containers.filter((c) => c.state === 'running').length,
    stopped: containers.filter((c) => c.state !== 'running').length,
    issues: containers.filter(isUnhealthy).length,
  }

  const columns = useMemo<Column[]>(() => {
    const f = q.trim().toLowerCase()
    const list = containers
      .filter((c) => {
        if (view === 'running' && c.state !== 'running') return false
        if (view === 'stopped' && c.state === 'running') return false
        if (view === 'issues' && !isUnhealthy(c)) return false
        return !f || [c.name, c.image, c.project ?? '', c.hostName].some((s) => s.toLowerCase().includes(f))
      })
      .sort(sortFns.status)
    if (group === 'host') {
      return hosts.map((h) => {
        const items = list.filter((c) => c.hostId === h.id)
        const meta = h.hello ? `${h.hello.os} · ${h.hello.ncpu} cores · ${mem(h.hello.mem_total)}` : 'offline'
        return { key: h.id, title: h.name, subtitle: h.online ? meta : 'offline', count: items.length, tone: h.online ? 'host' : 'offline', items, host: h }
      })
    }
    if (group === 'stack') {
      const by = new Map<string, Container[]>()
      for (const c of list) by.set(c.project || '', [...(by.get(c.project || '') ?? []), c])
      return [...by.entries()]
        .sort(([a], [b]) => (a === '' ? 1 : b === '' ? -1 : a.localeCompare(b)))
        .map(([k, items]) => ({
          key: k || '_none',
          title: k || 'No stack',
          subtitle: k ? `compose stack · ${new Set(items.map((c) => c.hostName)).size} host(s)` : 'standalone containers',
          count: items.length,
          tone: 'stack',
          items,
        }))
    }
    const states: [string, string, Column['tone'], (c: Container) => boolean][] = [
      ['issues', 'Needs attention', 'bad', isUnhealthy],
      ['running', 'Running', 'good', (c) => c.state === 'running' && !isUnhealthy(c)],
      ['stopped', 'Stopped & paused', 'idle', (c) => c.state !== 'running' && !isUnhealthy(c)],
    ]
    return states.map(([k, title, tone, fn]) => {
      const items = list.filter(fn)
      return { key: k, title, subtitle: `${items.length} container${items.length === 1 ? '' : 's'}`, count: items.length, tone, items }
    })
  }, [containers, hosts, view, group, q])

  return (
    <div className="animate-[rise_300ms_ease-out]">
      <div className="mb-6 flex flex-wrap items-center gap-2.5">
        <h1 className="mr-3 font-display text-3xl font-bold tracking-tight text-white">Containers</h1>
        {(
          [
            ['all', 'All', ''],
            ['running', 'Running', 'text-emerald-300'],
            ['stopped', 'Stopped', 'text-slate-300'],
            ['issues', 'Issues', 'text-rose-300'],
          ] as const
        ).map(([k, label, dot]) => (
          <button
            key={k}
            onClick={() => setView(k)}
            className={cx(
              'flex items-center gap-1.5 rounded-full px-4 py-1.5 text-sm font-semibold transition',
              view === k ? 'bg-gradient-to-r from-sky-500 to-purple-500 text-white shadow-lg shadow-purple-900/40' : 'bg-white/[0.06] text-indigo-100/80 ring-1 ring-white/15 hover:bg-white/[0.1]',
            )}
          >
            {dot && <span className={cx('text-[10px]', view === k ? 'text-white' : dot)}>●</span>}
            {label}
            <span className="tabular-nums opacity-80">{counts[k]}</span>
          </button>
        ))}
        <div className="relative ml-auto">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-indigo-100/50" />
          <input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Filter"
            className="w-48 rounded-full bg-white/[0.06] py-1.5 pl-9 pr-3 text-sm text-white ring-1 ring-white/15 outline-none placeholder:text-indigo-100/40 focus:ring-2 focus:ring-purple-400/60"
          />
        </div>
        <div className="flex items-center gap-1 rounded-full bg-white/[0.06] p-1 text-xs ring-1 ring-white/10">
          <span className="px-2 text-indigo-100/60">Group by</span>
          {(['host', 'stack', 'state'] as const).map((g) => (
            <button
              key={g}
              onClick={() => {
                setGroup(g)
                savePref('th.group', g)
              }}
              className={cx('rounded-full px-3 py-1 font-semibold capitalize transition', group === g ? 'bg-white text-[#0a0720]' : 'text-indigo-100/80 hover:text-white')}
            >
              {g}
            </button>
          ))}
        </div>
      </div>

      <div className="flex snap-x gap-5 overflow-x-auto pb-4">
        {columns.map((col) => (
          <section key={col.key} className="w-[min(100%,380px)] shrink-0 snap-start rounded-3xl bg-white/[0.04] p-3.5 ring-1 ring-white/10 backdrop-blur-xl">
            <div className={cx('rounded-2xl p-4 text-white shadow-lg', headerTone[col.tone])}>
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="truncate font-display text-xl font-bold">{col.title}</div>
                  <div className="mt-0.5 truncate text-xs text-white/85">{col.subtitle}</div>
                </div>
                <div className="font-display text-3xl font-bold tabular-nums">{col.count}</div>
              </div>
            </div>
            <div className="mt-3 space-y-2.5">
              {col.host && !col.host.online ? (
                <div className="flex flex-col items-center gap-2 py-10 text-center text-sm text-indigo-100/50">
                  <WifiOff className="size-5" />
                  Agent offline
                </div>
              ) : col.items.length === 0 ? (
                <div className="py-10 text-center text-sm text-indigo-100/50">Nothing here</div>
              ) : (
                col.items.map((c) => <ContainerCard key={c.hostId + c.id} c={c} operator={operator} showHost={group !== 'host'} />)
              )}
            </div>
          </section>
        ))}
      </div>
    </div>
  )
}

const pill: Record<string, string> = {
  bad: 'bg-rose-400/15 text-rose-300',
  running: 'bg-emerald-400/15 text-emerald-300',
  paused: 'bg-amber-400/15 text-amber-300',
  stopped: 'bg-white/10 text-slate-300',
}

function ContainerCard({ c, operator, showHost }: { c: Container; operator: boolean; showHost: boolean }) {
  const openLogs = useOpenLogs()
  const openDetails = useOpenDetails()
  const { run, pendingId } = useContainerAction()
  const bad = isUnhealthy(c)
  const running = c.state === 'running'
  const paused = c.state === 'paused'
  const label = bad ? (c.status.includes('unhealthy') ? 'unhealthy' : c.state === 'exited' ? 'failed' : c.state) : running ? 'running' : paused ? 'paused' : 'stopped'
  const busy = pendingId === c.id
  const act = (a: Action) => run(c, a)
  return (
    <div
      className={cx(
        'group rounded-2xl p-3.5 ring-1 transition hover:-translate-y-0.5',
        bad ? 'bg-gradient-to-br from-rose-500/20 to-orange-500/10 ring-rose-400/40' : 'bg-white/[0.06] ring-white/10 hover:ring-white/20',
      )}
    >
      <div className="flex items-center justify-between gap-2">
        <button onClick={() => openDetails(c)} className="truncate text-left font-display text-[15px] font-bold text-white hover:text-sky-200">
          {c.name}
        </button>
        <span className={cx('shrink-0 rounded-full px-2.5 py-0.5 text-[11px] font-semibold', pill[bad ? 'bad' : running ? 'running' : paused ? 'paused' : 'stopped'])}>{label}</span>
      </div>
      <div className="mt-1.5 flex items-center justify-between gap-2 text-xs text-indigo-100/70">
        <span className="flex min-w-0 items-center gap-1 truncate">
          {c.project && (
            <>
              <Boxes className="size-3 shrink-0 text-fuchsia-300" />
              <span className="truncate">{c.project}</span>
            </>
          )}
          {showHost && <span className="truncate text-indigo-100/50">{c.project ? ' · ' : ''}{c.hostName}</span>}
        </span>
        <span className="shrink-0">{c.status.replace(/\s*\((un)?healthy\)/, '')}</span>
      </div>
      <div className="mt-3 flex items-center gap-3">
        {running ? (
          <div className="mono flex flex-1 items-center gap-3 text-[11px] text-indigo-100/70">
            <span className="flex items-center gap-1.5">
              <span className="h-1 w-10 overflow-hidden rounded-full bg-white/10">
                <span className="block h-full rounded-full bg-gradient-to-r from-emerald-400 to-sky-400" style={{ width: `${Math.min(c.cpu ?? 0, 100)}%` }} />
              </span>
              {c.cpu === undefined ? '–' : `${c.cpu.toFixed(1)}%`}
            </span>
            <span>{mem(c.mem)}</span>
          </div>
        ) : (
          <span className="mono flex-1 truncate text-[11px] text-indigo-100/40" title={c.image}>
            {c.image}
          </span>
        )}
        <div className="flex items-center opacity-100 transition md:opacity-0 md:group-hover:opacity-100 md:focus-within:opacity-100">
          <IconButton icon={ScrollText} label="View logs" onClick={() => openLogs(c)} />
          {operator && (
            <>
              {running ? (
                <IconButton icon={Square} label="Stop" tone="warn" loading={busy} onClick={() => act('stop')} />
              ) : (
                <IconButton icon={Play} label={paused ? 'Resume' : 'Start'} tone="good" loading={busy} onClick={() => act(paused ? 'unpause' : 'start')} />
              )}
              <IconButton icon={RotateCw} label="Restart" disabled={busy} onClick={() => act('restart')} />
              <Menu
                trigger={(toggle) => <IconButton icon={Ellipsis} label="More actions" disabled={busy} onClick={toggle} />}
                items={[
                  { label: 'Details', icon: Info, onClick: () => openDetails(c) },
                  { label: 'Pause', icon: Pause, onClick: () => act('pause'), hidden: !running },
                  { label: 'Resume', icon: Play, onClick: () => act('unpause'), hidden: !paused },
                  { label: 'Kill', icon: Zap, onClick: () => act('kill'), danger: true, hidden: !running },
                  { label: 'Remove', icon: Trash2, onClick: () => act('remove'), danger: true },
                ]}
              />
            </>
          )}
        </div>
      </div>
    </div>
  )
}
