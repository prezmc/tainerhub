import { useEffect, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ArrowRightLeft, Clock, Cpu, Eye, EyeOff, HardDrive, HeartPulse, Network, Play, RotateCw, ScrollText, Settings2, Square, SquareTerminal, Tag, Trash2, Variable, X, type LucideIcon } from 'lucide-react'
import { api, can, errorText, mem, timeAgo, type Container, type DetailResponse } from '../api'
import { useContainerAction, useOpenLogs, useOpenTerminal } from '../fleet'
import { StackLink } from '../stacks'
import { PortLink } from './Containers'
import { useFleet } from '../fleet'
import { Badge, Button, ErrorNote, IconButton, StatusDot, cx, toneOf } from '../ui'
import { useMe } from './AppLayout'
import { ContainerMoveNotes, MoveContainerDialog, movable } from './MoveContainer'

/** Slide-over with everything Docker knows about one container. */
export function DetailsPanel({ container, onClose }: { container: Container; onClose: () => void }) {
  const me = useMe()
  const openLogs = useOpenLogs()
  const { run, pendingId } = useContainerAction()
  const host = useFleet().hosts.find((h) => h.id === container.hostId)
  const q = useQuery({
    queryKey: ['inspect', container.hostId, container.id],
    queryFn: () => api<DetailResponse>(`/api/hosts/${container.hostId}/containers/${encodeURIComponent(container.id)}`),
    refetchInterval: 15000,
  })

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const d = q.data?.detail
  const running = container.state === 'running'
  const operator = can(me.user, 'operator')
  const admin = can(me.user, 'admin')
  const openTerminal = useOpenTerminal()
  const [moving, setMoving] = useState(false)

  return (
    <div className="fixed inset-0 z-40">
      <div className="absolute inset-0 bg-black/50" onClick={onClose} />
      <aside
        role="dialog"
        aria-label={`${container.name} details`}
        className="absolute inset-y-0 right-0 flex w-full max-w-xl flex-col bg-panel shadow-2xl shadow-black/70 ring-1 ring-white/15 animate-[slide_180ms_ease-out]"
      >
        <header className="border-b border-white/10 p-6">
          <div className="flex items-start gap-3">
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2.5">
                <StatusDot tone={toneOf(container.state, container.status)} pulse={running} />
                <h2 className="truncate font-display text-2xl font-bold text-white">{container.name}</h2>
              </div>
              <p className="mt-1 truncate text-sm text-ink-100/60">
                {container.hostName} · {container.status}
              </p>
              {container.project && (
                <div className="mt-1 text-sm text-ink-100/60">
                  <StackLink c={container} onNavigate={onClose} />
                </div>
              )}
            </div>
            <IconButton icon={X} label="Close (Esc)" onClick={onClose} />
          </div>
          <div className="mt-4 flex flex-wrap gap-2">
            <Button icon={ScrollText} onClick={() => openLogs(container)}>
              Logs
            </Button>
            {admin && running && (
              <Button icon={SquareTerminal} onClick={() => openTerminal(container)}>
                Terminal
              </Button>
            )}
            {operator &&
              (running ? (
                <Button icon={Square} loading={pendingId === container.id} onClick={() => run(container, 'stop')}>
                  Stop
                </Button>
              ) : (
                <Button icon={Play} loading={pendingId === container.id} onClick={() => run(container, 'start')}>
                  Start
                </Button>
              ))}
            {operator && (
              <Button icon={RotateCw} variant="primary" disabled={pendingId === container.id} onClick={() => run(container, 'restart')}>
                Restart
              </Button>
            )}
            {operator && (
              <Button
                icon={Trash2}
                variant="dangerGhost"
                disabled={pendingId === container.id}
                onClick={async () => {
                  if (await run(container, 'remove')) onClose()
                }}
              >
                Remove
              </Button>
            )}
            {admin && movable(container) && (
              <Button icon={ArrowRightLeft} disabled={!host?.online} onClick={() => setMoving(true)}>
                Move…
              </Button>
            )}
          </div>
          {admin && container.project && !container.name.startsWith('tainerhub') && (
            <p className="mt-3 text-xs text-ink-100/50">It belongs to the compose project {container.project}: to move it to another host, move its stack.</p>
          )}
          {admin && <ContainerMoveNotes c={container} />}
          {moving && <MoveContainerDialog c={container} onClose={() => setMoving(false)} />}
          {running && (
            <div className="mt-4 grid grid-cols-2 gap-3">
              <Gauge icon={Cpu} label="CPU" value={container.cpu === undefined ? '–' : `${container.cpu.toFixed(1)}%`} pct={Math.min(container.cpu ?? 0, 100)} />
              <Gauge
                icon={HardDrive}
                label="Memory"
                value={container.mem_limit ? `${mem(container.mem)} / ${mem(container.mem_limit)}` : mem(container.mem)}
                pct={container.mem && container.mem_limit ? (container.mem / container.mem_limit) * 100 : 0}
              />
            </div>
          )}
        </header>

        <div className="flex-1 space-y-4 overflow-y-auto p-6">
          {q.isPending && <p className="text-sm text-ink-100/60">Loading details…</p>}
          <ErrorNote error={q.isError ? errorText(q.error) : null} />
          {d && (
            <>
              <Section icon={Settings2} title="Overview">
                <KV k="Image" v={<span className="mono break-all">{d.image}</span>} />
                <KV k="Image ID" v={<span className="mono">{d.imageId.replace('sha256:', '').slice(0, 12)}</span>} />
                <KV k="Created" v={`${new Date(d.created).toLocaleString()} (${timeAgo(d.created)})`} />
                {d.state.startedAt && !d.state.startedAt.startsWith('0001') && <KV k="Started" v={timeAgo(d.state.startedAt)} />}
                {!running && d.state.finishedAt && !d.state.finishedAt.startsWith('0001') && (
                  <KV k="Exited" v={`${timeAgo(d.state.finishedAt)} with code ${d.state.exitCode}${d.state.oomKilled ? ' (out of memory)' : ''}`} />
                )}
                <KV k="Restart policy" v={<Badge tone={d.restartPolicy === 'no' || !d.restartPolicy ? 'warn' : 'good'}>{d.restartPolicy || 'no'}{d.maxRetries ? ` · ${d.maxRetries} retries` : ''}</Badge>} />
                {d.privileged && <KV k="Privileged" v={<Badge tone="bad">yes</Badge>} />}
                {(d.memoryLimit || d.nanoCpus) && (
                  <KV k="Limits" v={[d.nanoCpus ? `${d.nanoCpus / 1e9} CPUs` : '', d.memoryLimit ? mem(d.memoryLimit) : ''].filter(Boolean).join(' · ')} />
                )}
                {(d.entrypoint?.length || d.cmd?.length) && (
                  <KV
                    k="Command"
                    v={
                      <span className="mono break-all text-xs">
                        {[...(d.entrypoint ?? []), ...(d.cmd ?? [])].join(' ')}
                        {!q.data?.envRevealed && (d.entrypoint?.length ?? 0) + (d.cmd?.length ?? 0) > 1 && (
                          <span className="ml-2 font-sans text-ink-100/50">(arguments hidden for your role)</span>
                        )}
                      </span>
                    }
                  />
                )}
                {d.state.error && <KV k="Error" v={<span className="text-rose-300">{d.state.error}</span>} />}
              </Section>

              {d.state.health && (
                <Section icon={HeartPulse} title="Health check" right={<Badge tone={d.state.health.status === 'healthy' ? 'good' : d.state.health.status === 'starting' ? 'warn' : 'bad'}>{d.state.health.status}</Badge>}>
                  {d.state.health.failingStreak > 0 && <p className="mb-2 text-sm text-rose-300">Failed {d.state.health.failingStreak} times in a row.</p>}
                  <ul className="space-y-2">
                    {[...d.state.health.log].reverse().map((p, i) => (
                      <li key={i} className="rounded-[3px] bg-black/30 p-3 ring-1 ring-white/[0.07]">
                        <div className="flex items-center justify-between text-xs">
                          <span className={p.exitCode === 0 ? 'text-emerald-300' : 'text-rose-300'}>{p.exitCode === 0 ? 'passed' : `failed (exit ${p.exitCode})`}</span>
                          <span className="text-ink-100/50">{timeAgo(p.start)}</span>
                        </div>
                        {p.output.trim() && <pre className="mono mt-1.5 max-h-24 overflow-auto whitespace-pre-wrap break-all text-[11px] text-ink-100/70">{p.output.trim()}</pre>}
                      </li>
                    ))}
                  </ul>
                </Section>
              )}

              <Section icon={Network} title="Networking" right={<span className="text-xs text-ink-100/50">{d.networkMode}</span>}>
                {d.ports.length > 0 && (
                  <div className="mb-3 flex flex-wrap gap-1.5">
                    {d.ports.map((p, i) => (
                      <span key={i} className="mono rounded-[3px] bg-lime-400/10 px-2 py-1 text-xs text-lime-200 ring-1 ring-lime-400/25">
                        {p.host ? (
                          <>
                            <PortLink port={p.host} host={host} /> →{' '}
                          </>
                        ) : (
                          ''
                        )}
                        {p.container}
                      </span>
                    ))}
                  </div>
                )}
                {d.networks.map((n) => (
                  <KV key={n.name} k={n.name} v={<span className="mono text-xs">{n.ip || '–'}</span>} />
                ))}
                {d.networks.length === 0 && d.ports.length === 0 && <p className="text-sm text-ink-100/50">No networks or published ports.</p>}
              </Section>

              <Section icon={HardDrive} title="Mounts">
                {d.mounts.length === 0 && <p className="text-sm text-ink-100/50">No volumes or bind mounts.</p>}
                <ul className="space-y-2">
                  {d.mounts.map((m, i) => (
                    <li key={i} className="rounded-[3px] bg-black/30 p-3 text-xs ring-1 ring-white/[0.07]">
                      <div className="flex items-center gap-2">
                        <Badge tone={m.type === 'volume' ? 'accent' : 'info'}>{m.type}</Badge>
                        {!m.rw && <Badge tone="warn">read-only</Badge>}
                        <span className="mono ml-auto truncate text-white">{m.destination}</span>
                      </div>
                      <div className="mono mt-1.5 truncate text-ink-100/60" title={m.source}>
                        {m.name || m.source}
                      </div>
                    </li>
                  ))}
                </ul>
              </Section>

              <EnvSection env={d.env ?? []} revealed={!!q.data?.envRevealed} />

              {d.labels && Object.keys(d.labels).length > 0 && (
                <Section icon={Tag} title="Labels" collapsible>
                  <div className="space-y-1">
                    {Object.entries(d.labels)
                      .sort(([a], [b]) => a.localeCompare(b))
                      .map(([k, v]) => (
                        <div key={k} className="mono text-[11px] leading-relaxed">
                          <span className="text-cyan-300">{k}</span>
                          <span className="text-ink-100/40">=</span>
                          <span className="break-all text-ink-100/80">{v}</span>
                        </div>
                      ))}
                  </div>
                  {!q.data?.envRevealed && <p className="mt-2 text-xs text-ink-100/40">Labels can hold credentials (e.g. reverse-proxy auth), so most values are hidden for your role; admins see them.</p>}
                </Section>
              )}

              <p className="mono flex items-center gap-1.5 text-[11px] text-ink-100/40">
                <Clock className="size-3" /> ID {d.id.slice(0, 12)} · refreshed every 15 s
              </p>
            </>
          )}
        </div>
      </aside>
    </div>
  )
}

function Gauge({ icon: Icon, label, value, pct }: { icon: LucideIcon; label: string; value: string; pct: number }) {
  return (
    <div className="rounded-md bg-white/[0.05] p-3 ring-1 ring-white/10">
      <div className="flex items-center justify-between text-xs text-ink-100/60">
        <span className="flex items-center gap-1.5">
          <Icon className="size-3.5" /> {label}
        </span>
        <span className="mono text-white">{value}</span>
      </div>
      <div className="mt-2 h-1.5 overflow-hidden rounded-[2px] bg-white/10">
        <div className="h-full bg-lime-400 transition-all duration-700" style={{ width: `${Math.max(pct, 2)}%` }} />
      </div>
    </div>
  )
}

function Section({ icon: Icon, title, right, children, collapsible }: { icon: LucideIcon; title: string; right?: ReactNode; children: ReactNode; collapsible?: boolean }) {
  const [open, setOpen] = useState(!collapsible)
  return (
    <section className="rounded-md bg-white/[0.04] p-4 ring-1 ring-white/10">
      {/* The header's own controls (e.g. "Show values") sit beside the toggle,
          not inside it: clicks inside a disabled button never fire. */}
      <div className="flex w-full items-center gap-2">
        <button disabled={!collapsible} onClick={() => setOpen((o) => !o)} className="flex min-w-0 flex-1 items-center gap-2 text-left">
          <Icon className="size-4 shrink-0 text-lime-300" />
          <h3 className="font-display font-bold text-white">{title}</h3>
          {!right && collapsible && <span className="ml-auto text-xs text-ink-100/50">{open ? 'hide' : 'show'}</span>}
        </button>
        {right && <span className="shrink-0">{right}</span>}
      </div>
      {open && <div className="mt-3">{children}</div>}
    </section>
  )
}

function KV({ k, v }: { k: string; v: ReactNode }) {
  return (
    <div className="flex gap-4 border-b border-white/[0.05] py-1.5 text-sm last:border-0">
      <span className="w-32 shrink-0 text-ink-100/55">{k}</span>
      <span className="min-w-0 flex-1 text-ink-50">{v}</span>
    </div>
  )
}

function EnvSection({ env, revealed }: { env: string[]; revealed: boolean }) {
  const [show, setShow] = useState(false)
  if (env.length === 0) return null
  return (
    <Section
      icon={Variable}
      title="Environment"
      right={
        revealed ? (
          <button onClick={() => setShow((s) => !s)} className="flex items-center gap-1 text-xs text-lime-300 hover:text-lime-200">
            {show ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />} {show ? 'Hide values' : 'Show values'}
          </button>
        ) : (
          <span className="text-xs text-ink-100/50">values visible to admins</span>
        )
      }
    >
      <div className="space-y-1">
        {env.map((e, i) => {
          const [k, ...rest] = e.split('=')
          const v = rest.join('=')
          return (
            <div key={i} className="mono flex gap-1 text-[11px] leading-relaxed">
              <span className="shrink-0 text-emerald-300">{k}</span>
              <span className="text-ink-100/40">=</span>
              <span className={cx('break-all', show ? 'text-ink-100/80' : 'select-none text-ink-100/40')}>{show ? v : v ? '••••••••' : revealed ? '' : '••••••••'}</span>
            </div>
          )
        })}
      </div>
      {!revealed && <p className="mt-2 text-xs text-ink-100/40">Secrets often live in environment variables, so the hub only sends the values to admins.</p>}
    </Section>
  )
}

