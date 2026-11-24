import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { ArrowRight, Cpu, MemoryStick, PartyPopper, Plus, RotateCw, ScrollText, Server, WifiOff } from 'lucide-react'
import { can, isUnhealthy, mem, timeAgo, type Activity, type Container, type Host } from '../api'
import { loadPref, savePref, sortFns, useContainerAction, useFleet, useOpenDetails, useOpenLogs, type SortKey } from '../fleet'
import { Button, Card, EmptyState, ErrorNote, cx } from '../ui'
import { useMe } from './AppLayout'

const SORTS: { key: SortKey; label: string }[] = [
  { key: 'cpu', label: 'CPU' },
  { key: 'memory', label: 'Memory' },
  { key: 'name', label: 'Name' },
  { key: 'status', label: 'Status' },
]

export function OverviewPage() {
  const me = useMe()
  const { containers, hosts, activity, loading, error } = useFleet()
  const [sort, setSort] = useState<SortKey>(() => loadPref('th.sort', ['cpu', 'memory', 'name', 'status'] as const, 'cpu'))

  const issues = containers.filter(isUnhealthy)
  const offline = hosts.filter((h) => !h.online)
  const running = containers.filter((c) => c.state === 'running').length
  const stopped = containers.length - running
  const alerts = issues.length + offline.filter((h) => h.enrolledAt).length

  if (error && hosts.length === 0) return <ErrorNote error={`Couldn't load the fleet: ${error}`} />
  if (!loading && hosts.length === 0) {
    return (
      <EmptyState icon={Server} title="Let's connect your first server">
        <p className="mb-6">Add a host and run the small TainerHub agent on it. The agent connects out to this hub, so the server needs no open ports.</p>
        <Link to="/hosts">
          <Button variant="primary" icon={Plus}>
            Add a host
          </Button>
        </Link>
      </EmptyState>
    )
  }

  return (
    <div className="animate-[rise_300ms_ease-out]">
      <section className="mb-8 flex flex-wrap items-end justify-between gap-6">
        <div>
          <div className="mono text-xs tracking-[0.3em] text-lime-300">FLEET STATUS · LIVE</div>
          <h1 className="mt-2 font-display text-4xl font-bold leading-[1.08] tracking-tight text-white sm:text-5xl">
            <Headline alerts={alerts} />
          </h1>
        </div>
        <div className="flex gap-8 text-right sm:gap-10">
          <Counter value={`${hosts.filter((h) => h.online).length}/${hosts.length}`} label="Hosts" color="text-emerald-300" />
          <Counter value={running} label="Running" color="text-lime-300" />
          <Counter value={stopped} label="Stopped" color="text-ink-300" />
          <Counter value={alerts} label="Alerts" color={alerts ? 'text-rose-400' : 'text-ink-500'} />
        </div>
      </section>

      <div className="grid grid-cols-1 gap-5 xl:grid-cols-[minmax(0,2.3fr)_minmax(320px,1fr)]">
        <Card className="p-5 sm:p-6">
          <div className="mb-5 flex flex-wrap items-center gap-2">
            <h2 className="mr-auto font-display text-xl font-bold text-white">Fleet map</h2>
            <span className="mr-1 text-xs text-ink-100/60">Sort</span>
            {SORTS.map((s) => (
              <button
                key={s.key}
                onClick={() => {
                  setSort(s.key)
                  savePref('th.sort', s.key)
                }}
                className={cx(
                  'rounded-[2px] px-3 py-1.5 font-display text-[11px] font-semibold uppercase tracking-[0.1em] transition',
                  sort === s.key ? 'bg-lime-400 text-ink-950' : 'text-ink-100/70 ring-1 ring-inset ring-line hover:text-white hover:ring-lime-400/40',
                )}
              >
                {s.label}
              </button>
            ))}
          </div>
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 2xl:grid-cols-3">
            {hosts.map((h) => (
              <Rack key={h.id} host={h} containers={containers.filter((c) => c.hostId === h.id)} sort={sort} />
            ))}
          </div>
        </Card>

        <div className="flex flex-col gap-5">
          <Attention issues={issues} offline={offline.filter((h) => h.enrolledAt)} canAct={can(me.user, 'operator')} />
          <TopConsumers containers={containers} />
          <ActivityFeed activity={activity} />
        </div>
      </div>
    </div>
  )
}

function Headline({ alerts }: { alerts: number }) {
  if (alerts === 0)
    return (
      <>
        All systems <span className="text-aurora">nominal</span>.
      </>
    )
  if (alerts === 1)
    return (
      <>
        All systems <span className="text-aurora">nominal</span>,
        <br />
        one needs a look.
      </>
    )
  return (
    <>
      <span className="text-rose-300">{alerts} things</span> need
      <br />a look.
    </>
  )
}

function Counter({ value, label, color }: { value: number | string; label: string; color: string }) {
  return (
    <div>
      <div className={cx('font-display text-4xl font-bold tabular-nums sm:text-5xl', color)}>{value}</div>
      <div className="mt-1 text-[11px] font-medium uppercase tracking-[0.18em] text-ink-100/60">{label}</div>
    </div>
  )
}

const MAX_ROWS = 10

function Rack({ host, containers, sort }: { host: Host; containers: Container[]; sort: SortKey }) {
  const openDetails = useOpenDetails()
  const [all, setAll] = useState(false)
  const sorted = useMemo(() => {
    const list = [...containers].sort(sortFns[sort])
    // Stopped containers sink to the bottom except when sorting by name.
    return sort === 'name' ? list : [...list.filter((c) => c.state !== 'exited' && c.state !== 'created'), ...list.filter((c) => c.state === 'exited' || c.state === 'created')]
  }, [containers, sort])
  const shown = all ? sorted : sorted.slice(0, MAX_ROWS)
  const cpuTotal = containers.reduce((a, c) => a + (c.cpu ?? 0), 0)
  const memTotal = containers.reduce((a, c) => a + (c.mem ?? 0), 0)
  const cores = host.hello?.ncpu ?? 1

  return (
    <div className={cx('rounded-md bg-white/[0.04] p-4 ring-1 ring-white/10', !host.online && 'opacity-60')}>
      <div className="flex items-center justify-between gap-2">
        <Link to="/containers" search={{ host: host.id }} className="truncate font-display text-base font-bold text-white hover:text-lime-200 hover:underline" title={`Show ${host.name}'s containers`}>
          {host.name}
        </Link>
        <span
          className={cx(
            'shrink-0 rounded-[2px] px-1.5 py-[3px] font-mono text-[10.5px] font-medium uppercase leading-none tracking-wider',
            host.online ? 'bg-emerald-400/15 text-emerald-300' : 'bg-white/10 text-ink-400',
          )}
        >
          {host.online ? '● online' : 'offline'}
        </span>
      </div>
      {host.online && host.hello ? (
        <div className="mt-1.5 flex flex-wrap justify-between gap-x-3 text-xs text-ink-100/60">
          <span className="truncate">{host.hello.os}</span>
          <span className="mono">
            CPU {Math.round(cpuTotal / cores)}% of {cores} · {mem(memTotal)}
          </span>
        </div>
      ) : (
        <div className="mt-1.5 text-xs text-ink-100/50">last seen {timeAgo(host.lastSeenAt)}</div>
      )}

      {host.online ? (
        <>
          <div className="mono mt-4 flex px-3 text-[10px] tracking-[0.15em] text-ink-100/50">
            <span className="flex-1 pl-3.5">CONTAINER</span>
            <span className="w-14 text-right">CPU</span>
            <span className="w-16 text-right">MEM</span>
          </div>
          <div className="mt-1.5 space-y-1.5 rounded-[3px] bg-black/25 p-2 ring-1 ring-white/[0.07]">
            {shown.map((c) => (
              <Bar key={c.id} c={c} onClick={() => openDetails(c)} />
            ))}
            {containers.length === 0 && <div className="py-4 text-center text-xs text-ink-100/50">No containers</div>}
          </div>
          {sorted.length > MAX_ROWS && (
            <button onClick={() => setAll((a) => !a)} className="mt-2 w-full text-center text-xs font-medium text-lime-300 hover:text-lime-200">
              {all ? 'Show fewer' : `Show all ${sorted.length}`}
            </button>
          )}
        </>
      ) : (
        <div className="mt-4 flex h-24 items-center justify-center gap-2 rounded-[3px] bg-black/25 text-xs text-ink-100/50 ring-1 ring-white/[0.07]">
          <WifiOff className="size-4" /> no signal from the agent
        </div>
      )}
    </div>
  )
}

const barTone = {
  good: { line: 'bg-emerald-400', fill: 'bg-emerald-400/15', ring: 'ring-emerald-400/20', bg: 'bg-emerald-400/[0.05]' },
  bad: { line: 'bg-rose-400', fill: 'bg-rose-400/20', ring: 'ring-rose-400/40', bg: 'bg-rose-400/[0.08]' },
  warn: { line: 'bg-amber-400', fill: 'bg-amber-400/15', ring: 'ring-amber-400/30', bg: 'bg-amber-400/[0.05]' },
}

function Bar({ c, onClick }: { c: Container; onClick: () => void }) {
  const stopped = c.state === 'exited' || c.state === 'created'
  if (stopped && !isUnhealthy(c)) {
    return (
      <button onClick={onClick} className="flex h-9 w-full items-center gap-2.5 rounded-[3px] border border-dashed border-white/15 px-3 text-left text-[13px] text-ink-100/50 transition hover:border-white/30">
        <span className="h-4 w-[3px] bg-ink-500" />
        <span className="flex-1 truncate">{c.name}</span>
        <span className="text-[11px]">stopped</span>
      </button>
    )
  }
  const t = isUnhealthy(c) ? barTone.bad : c.state === 'paused' ? barTone.warn : barTone.good
  const cpu = c.cpu ?? 0
  return (
    <button
      onClick={onClick}
      title={`${c.name} · ${c.status} · ${c.image}`}
      className={cx('group relative flex h-9 w-full items-center gap-2.5 overflow-hidden rounded-[3px] px-3 text-left ring-1 transition hover:brightness-125', t.ring, t.bg)}
    >
      <span className={cx('absolute inset-y-0 left-0 transition-all duration-700', t.fill)} style={{ width: `${Math.min(cpu, 100)}%` }} />
      <span className={cx('relative h-4 w-[3px] shrink-0', t.line)} />
      <span className="relative flex-1 truncate text-[13px] font-semibold text-white">{c.name}</span>
      <span className={cx('mono relative w-14 text-right text-[11px] tabular-nums', cpu >= 50 ? 'text-rose-300' : 'text-ink-200')}>{c.cpu === undefined ? '–' : `${cpu.toFixed(1)}%`}</span>
      <span className="mono relative w-16 text-right text-[11px] tabular-nums text-ink-100/70">{mem(c.mem)}</span>
    </button>
  )
}

function Attention({ issues, offline, canAct }: { issues: Container[]; offline: Host[]; canAct: boolean }) {
  const openLogs = useOpenLogs()
  const { run, pendingId } = useContainerAction()
  if (issues.length === 0 && offline.length === 0) {
    return (
      <div className="rounded-md border-l-4 border-emerald-400 bg-emerald-400/[0.08] p-6 ring-1 ring-inset ring-emerald-400/20">
        <div className="mono text-[11px] tracking-[0.25em] text-emerald-300">ALL CLEAR</div>
        <div className="mt-2 flex items-center gap-2 font-display text-2xl font-bold text-white">
          Everything's healthy <PartyPopper className="size-6" />
        </div>
        <p className="mt-1 text-sm text-ink-100/75">No unhealthy containers and every host is connected.</p>
      </div>
    )
  }
  const first = issues[0]
  return (
    <div className="rounded-md border-l-4 border-rose-400 bg-rose-500/[0.09] p-6 ring-1 ring-inset ring-rose-400/25">
      <div className="mono text-[11px] tracking-[0.25em] text-rose-300">NEEDS ATTENTION</div>
      {first ? (
        <>
          <div className="mt-2 font-display text-2xl font-bold text-white">{first.name}</div>
          <div className="mt-0.5 text-sm text-ink-100/75">
            {first.hostName} · {first.status}
            {first.cpu !== undefined && ` · ${first.cpu.toFixed(0)}% CPU`}
          </div>
          <div className="mt-4 flex flex-wrap gap-2">
            {canAct && (
              <Button variant="danger" icon={RotateCw} loading={pendingId === first.id} onClick={() => run(first, 'restart')}>
                Restart
              </Button>
            )}
            <Button icon={ScrollText} onClick={() => openLogs(first)}>
              View logs
            </Button>
          </div>
        </>
      ) : null}
      {(issues.length > 1 || offline.length > 0) && (
        <ul className={cx('space-y-1.5 text-sm text-ink-100/80', first && 'mt-5 border-t border-rose-400/20 pt-4')}>
          {issues.slice(1, 5).map((c) => (
            <li key={c.hostId + c.id}>
              <button onClick={() => openLogs(c)} className="flex w-full items-center gap-2 text-left hover:underline">
                <span className="size-1.5 rounded-full bg-white" />
                <b>{c.name}</b> <span className="text-white/75">on {c.hostName}</span>
              </button>
            </li>
          ))}
          {offline.map((h) => (
            <li key={h.id}>
              <Link to="/hosts" className="flex items-center gap-2 hover:underline">
                <WifiOff className="size-3.5" />
                <b>{h.name}</b> <span className="text-white/75">is offline</span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function TopConsumers({ containers }: { containers: Container[] }) {
  const [by, setBy] = useState<'cpu' | 'mem'>('cpu')
  const openLogs = useOpenLogs()
  const top = [...containers]
    .filter((c) => c.state === 'running' && (by === 'cpu' ? c.cpu !== undefined : c.mem))
    .sort(by === 'cpu' ? sortFns.cpu : sortFns.memory)
    .slice(0, 5)
  const max = Math.max(...top.map((c) => (by === 'cpu' ? c.cpu ?? 0 : c.mem ?? 0)), by === 'cpu' ? 1 : 1)
  return (
    <Card className="p-5">
      <div className="mb-3 flex items-center">
        <h2 className="mr-auto font-display text-lg font-bold text-white">Top consumers</h2>
        <div className="flex rounded-[3px] bg-ink-950/60 p-0.5 ring-1 ring-line">
          {(
            [
              ['cpu', Cpu],
              ['mem', MemoryStick],
            ] as const
          ).map(([k, Icon]) => (
            <button
              key={k}
              onClick={() => setBy(k)}
              aria-label={k === 'cpu' ? 'By CPU' : 'By memory'}
              className={cx('rounded-[2px] p-1.5 transition', by === k ? 'bg-lime-400 text-ink-950' : 'text-ink-100/50 hover:text-white')}
            >
              <Icon className="size-3.5" />
            </button>
          ))}
        </div>
      </div>
      {top.length === 0 && <p className="py-3 text-sm text-ink-100/50">Waiting for the first samples from your agents…</p>}
      <ul className="space-y-3">
        {top.map((c) => {
          const v = by === 'cpu' ? c.cpu ?? 0 : c.mem ?? 0
          return (
            <li key={c.hostId + c.id}>
              <button onClick={() => openLogs(c)} className="w-full text-left">
                <div className="flex items-baseline justify-between gap-2 text-sm">
                  <span className="truncate font-medium text-white">
                    {c.name} <span className="text-xs font-normal text-ink-100/50">{c.hostName}</span>
                  </span>
                  <span className="mono shrink-0 text-xs tabular-nums text-ink-200">{by === 'cpu' ? `${v.toFixed(1)}%` : mem(v)}</span>
                </div>
                <div className="mt-1.5 h-1 overflow-hidden bg-white/[0.07]">
                  <div className="h-full bg-lime-400 transition-all duration-700" style={{ width: `${Math.max((v / max) * 100, 3)}%` }} />
                </div>
              </button>
            </li>
          )
        })}
      </ul>
    </Card>
  )
}

const userVerbs: Record<string, string> = {
  start: 'started',
  stop: 'stopped',
  restart: 'restarted',
  pause: 'paused',
  unpause: 'resumed',
  kill: 'killed',
  remove: 'removed',
  'stack.deploy': 'deployed stack',
  'stack.pull': 'pulled and redeployed stack',
  'stack.start': 'started stack',
  'stack.stop': 'stopped stack',
  'stack.restart': 'restarted stack',
  'stack.down': 'took down stack',
  'stack.remove': 'deleted stack',
}

function describe(a: Activity): { text: React.ReactNode; color: string } {
  const who = a.container ?? a.host
  switch (a.kind) {
    case 'host':
      return a.action === 'connected'
        ? { text: <><b>{a.host}</b> connected</>, color: 'bg-emerald-400' }
        : { text: <><b>{a.host}</b> went offline</>, color: 'bg-ink-400' }
    case 'user':
      return { text: <><b>{a.actor}</b> {userVerbs[a.action] ?? a.action} <b>{who}</b></>, color: 'bg-lime-400' }
    default: {
      const map: Record<string, [string, string]> = {
        start: ['started', 'bg-emerald-400'],
        die: ['exited', 'bg-ink-400'],
        stop: ['stopped', 'bg-ink-400'],
        restart: ['restarted', 'bg-lime-400'],
        kill: ['was killed', 'bg-amber-400'],
        oom: ['ran out of memory', 'bg-rose-400'],
        pause: ['paused', 'bg-amber-400'],
        unpause: ['resumed', 'bg-emerald-400'],
        destroy: ['was removed', 'bg-cyan-400'],
        'health_status: unhealthy': ['became unhealthy', 'bg-rose-400'],
        'health_status: healthy': ['is healthy again', 'bg-emerald-400'],
      }
      const [verb, color] = map[a.action] ?? [a.action, 'bg-ink-400']
      return { text: <><b>{who}</b> {verb} <span className="text-ink-100/50">on {a.host}</span></>, color }
    }
  }
}

function ActivityFeed({ activity }: { activity: Activity[] }) {
  return (
    <Card className="p-5">
      <div className="mb-3 flex items-center">
        <h2 className="mr-auto font-display text-lg font-bold text-white">Live activity</h2>
        <Link to="/audit" className="flex items-center gap-1 text-xs font-medium text-lime-300 hover:text-lime-200">
          Audit log <ArrowRight className="size-3" />
        </Link>
      </div>
      {activity.length === 0 && <p className="py-3 text-sm text-ink-100/50">Quiet so far. Container starts, stops and health changes appear here as they happen.</p>}
      <ul className="space-y-2.5">
        {activity.slice(0, 8).map((a, i) => {
          const d = describe(a)
          return (
            <li key={i} className="flex items-center gap-2.5 text-[13px] text-ink-50">
              <span className={cx('size-2 shrink-0 rounded-full', d.color)} />
              <span className="min-w-0 flex-1 truncate">{d.text}</span>
              <span className="mono shrink-0 text-[11px] text-ink-100/50">{timeAgo(a.at).replace(' ago', '')}</span>
            </li>
          )
        })}
      </ul>
    </Card>
  )
}
