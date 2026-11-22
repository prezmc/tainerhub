import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Clock, Cpu, Ellipsis, KeyRound, Layers, MemoryStick, Plus, Server, ShieldCheck, Trash2 } from 'lucide-react'
import { api, bytes, can, errorText, stackProblem, timeAgo, type Host, type JoinTokenResponse } from '../api'
import { Badge, Button, Card, CopyButton, EmptyState, ErrorNote, IconButton, Input, Menu, Modal, PageHeader, StatusDot, cx, useConfirm, useToast } from '../ui'
import { useMe } from './AppLayout'

export function HostsPage() {
  const me = useMe()
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const admin = can(me.user, 'admin')
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: () => api<Host[]>('/api/hosts'), refetchInterval: 30000 })
  const [adding, setAdding] = useState(false)
  const [name, setName] = useState('')
  // Agents dial the hub directly (not through a reverse proxy) on port 7443.
  // The address this page was opened on is the right guess for a plain install.
  const [agentAddr, setAgentAddr] = useState(() => `${window.location.hostname}:7443`)
  const [join, setJoin] = useState<JoinTokenResponse | null>(null)
  const [error, setError] = useState<string | null>(null)

  const create = useMutation({
    mutationFn: () => api<JoinTokenResponse>('/api/hosts', { method: 'POST', body: { name: name.trim(), agentAddr: agentAddr.trim() } }),
    onSuccess: (res) => {
      setAdding(false)
      setName('')
      setJoin(res)
      qc.invalidateQueries({ queryKey: ['hosts'] })
    },
    onError: (e) => setError(errorText(e)),
  })
  const reissue = useMutation({
    mutationFn: (h: Host) => api<JoinTokenResponse>(`/api/hosts/${h.id}/token`, { method: 'POST', body: { agentAddr: agentAddr.trim() } }),
    onSuccess: setJoin,
    onError: (e) => toast('bad', errorText(e)),
  })
  const remove = useMutation({
    mutationFn: (h: Host) => api(`/api/hosts/${h.id}`, { method: 'DELETE' }),
    onSuccess: (_, h) => {
      toast('good', `${h.name} removed`)
      qc.invalidateQueries({ queryKey: ['hosts'] })
    },
    onError: (e) => toast('bad', errorText(e)),
  })

  const askRemove = async (h: Host) => {
    const ok = await confirm({
      title: `Remove ${h.name}?`,
      body: 'Its agent is disconnected immediately and its certificate stops working. Containers on the host keep running, but the stacks TainerHub saved for it are deleted from TainerHub. To add it back, create the host again and re-run the agent.',
      confirm: 'Remove host',
      danger: true,
    })
    if (ok) remove.mutate(h)
  }

  const online = hosts.data?.filter((h) => h.online).length ?? 0

  return (
    <div>
      <PageHeader title="Hosts" subtitle={hosts.data ? `${online} of ${hosts.data.length} connected` : undefined}>
        {admin && (
          <Button
            variant="primary"
            icon={Plus}
            onClick={() => {
              setAdding(true)
              setError(null)
            }}
          >
            Add host
          </Button>
        )}
      </PageHeader>

      {hosts.data?.length === 0 ? (
        <EmptyState icon={Server} title="No hosts yet">
          Add a host to get a one-time join token and a ready-made compose file for its agent.
        </EmptyState>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {hosts.data?.map((h) => (
            <HostCard key={h.id} h={h} admin={admin} onToken={() => reissue.mutate(h)} onRemove={() => askRemove(h)} />
          ))}
        </div>
      )}

      <Modal open={adding} onClose={() => setAdding(false)} title="Add a host">
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            setError(null)
            create.mutate()
          }}
        >
          <Input label="Name" placeholder="server-2" value={name} onChange={(e) => setName(e.target.value)} autoFocus required maxLength={64} hint="How this server appears in TainerHub." />
          <Input
            label="Hub address for agents"
            value={agentAddr}
            onChange={(e) => setAgentAddr(e.target.value)}
            required
            className="mono"
            hint="The hub server's IP or name, port 7443. The agent connects here directly, never through a reverse proxy."
          />
          <ErrorNote error={error} />
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={() => setAdding(false)}>
              Cancel
            </Button>
            <Button variant="primary" icon={KeyRound} loading={create.isPending}>
              Create join token
            </Button>
          </div>
        </form>
      </Modal>

      <Modal open={!!join} onClose={() => setJoin(null)} title={join ? `Connect ${join.host.name}` : ''} wide>
        {join && <JoinInstructions join={join} />}
      </Modal>
    </div>
  )
}

function HostCard({ h, admin, onToken, onRemove }: { h: Host; admin: boolean; onToken: () => void; onRemove: () => void }) {
  const stopped = h.containers - h.running
  const pct = h.containers ? Math.round((h.running / h.containers) * 100) : 0
  return (
    <Card className={cx('relative p-5 transition hover:ring-lime-400/30', !h.online && 'opacity-80')}>
      <div className={cx('pointer-events-none absolute left-0 top-0 h-[2px] w-16', h.online ? 'bg-lime-400' : 'bg-ink-600')} />
      <div className="flex items-start gap-3">
        <div className={cx('rounded-[3px] p-2.5 ring-1 ring-inset', h.online ? 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/20' : 'bg-white/[0.04] text-ink-100/50 ring-white/10')}>
          <Server className="size-5" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <h2 className="truncate text-base font-semibold text-white">{h.name}</h2>
            <StatusDot tone={h.online ? 'good' : 'idle'} pulse={h.online} />
          </div>
          <p className="truncate text-xs text-ink-100/50">
            {h.hello ? `${h.hello.os} · ${h.hello.arch}` : h.enrolledAt ? `Last seen ${timeAgo(h.lastSeenAt)}` : 'Waiting for its agent to connect'}
          </p>
        </div>
        {admin && (
          <Menu
            trigger={(toggle) => <IconButton icon={Ellipsis} label="Host actions" onClick={toggle} />}
            items={[
              { label: 'New join token', icon: KeyRound, onClick: onToken },
              { label: 'Remove host', icon: Trash2, onClick: onRemove, danger: true },
            ]}
          />
        )}
      </div>

      {h.online && h.hello ? (
        <>
          <div className="mt-5 flex items-center gap-4">
            <Ring pct={pct} />
            <div className="space-y-1 text-sm">
              <div>
                <span className="font-semibold tabular-nums text-white">{h.running}</span> <span className="text-ink-100/60">running</span>
              </div>
              <div className="text-ink-100/50">
                <span className="tabular-nums">{stopped}</span> stopped
                {h.unhealthy > 0 && (
                  <Badge tone="bad" className="ml-2">
                    {h.unhealthy} unhealthy
                  </Badge>
                )}
              </div>
            </div>
          </div>
          <dl className="mt-5 grid grid-cols-3 gap-2 border-t border-white/[0.06] pt-4 text-xs">
            <Fact icon={Cpu} tint="text-lime-400" label="CPU" value={`${h.hello.ncpu} cores`} />
            <Fact icon={MemoryStick} tint="text-cyan-400" label="Memory" value={bytes(h.hello.mem_total)} />
            <Fact icon={ShieldCheck} tint="text-emerald-400" label="Docker" value={h.hello.docker_version} />
          </dl>
          {stackProblem(h) && (
            <p className="mt-4 flex items-start gap-2 rounded-[3px] bg-amber-500/10 px-3 py-2 text-xs leading-relaxed text-amber-100 ring-1 ring-inset ring-amber-500/20">
              <Layers className="mt-0.5 size-3.5 shrink-0" />
              <span>Stacks unavailable: {stackProblem(h)}</span>
            </p>
          )}
        </>
      ) : (
        <div className="mt-5 flex items-center gap-2 rounded-[3px] bg-white/[0.03] px-3 py-2.5 text-xs text-ink-100/60 ring-1 ring-inset ring-white/[0.06]">
          <Clock className="size-3.5 shrink-0" />
          {h.enrolledAt ? 'Agent offline. Check docker logs tainerhub-agent on this host.' : 'Run the agent with its join token to connect.'}
        </div>
      )}
    </Card>
  )
}

function Fact({ icon: Icon, label, value, tint }: { icon: typeof Cpu; label: string; value: string; tint: string }) {
  return (
    <div className="min-w-0">
      <dt className="flex items-center gap-1 text-ink-100/50">
        <Icon className={cx('size-3', tint)} />
        {label}
      </dt>
      <dd className="mt-0.5 truncate font-medium text-ink-50">{value}</dd>
    </div>
  )
}

/** Small donut showing the share of running containers. */
function Ring({ pct }: { pct: number }) {
  const r = 22
  const c = 2 * Math.PI * r
  return (
    <div className="relative size-14 shrink-0" role="img" aria-label={`${pct}% running`}>
      <svg viewBox="0 0 56 56" className="size-14 -rotate-90">
        <defs>
          <linearGradient id="ring-g">
            <stop offset="0" stopColor="#d4f77a" />
            <stop offset=".6" stopColor="#c4f04a" />
            <stop offset="1" stopColor="#7fa81a" />
          </linearGradient>
        </defs>
        <circle cx="28" cy="28" r={r} fill="none" stroke="rgb(255 255 255 / 0.07)" strokeWidth="6" />
        <circle cx="28" cy="28" r={r} fill="none" stroke="url(#ring-g)" strokeWidth="6" strokeLinecap="butt" strokeDasharray={`${(pct / 100) * c} ${c}`} className="transition-all duration-700" />
      </svg>
      <span className="absolute inset-0 flex items-center justify-center text-[12px] font-semibold tabular-nums text-white">{pct}%</span>
    </div>
  )
}

function JoinInstructions({ join }: { join: JoinTokenResponse }) {
  const steps = [
    <>
      On <strong className="text-white">{join.host.name}</strong>, create a folder such as <code className="mono text-lime-300">~/tainerhub-agent</code> and save this as{' '}
      <code className="mono text-lime-300">compose.yml</code>.
    </>,
    <>
      Run <code className="mono text-lime-300">docker compose up -d</code>. The agent connects to <code className="mono text-lime-300">{join.agentAddr}</code>, so that port must be reachable
      from the server. The host turns green here within a few seconds.
    </>,
    <>
      The token works once and expires at <strong className="text-white">{new Date(join.expiresAt).toLocaleTimeString()}</strong>. Afterwards you can delete the{' '}
      <code className="mono text-lime-300">TAINERHUB_JOIN_TOKEN</code> line.
    </>,
  ]
  return (
    <div className="space-y-5">
      <ol className="space-y-3">
        {steps.map((s, i) => (
          <li key={i} className="flex gap-3 text-sm text-ink-100/80">
            <span className="flex size-6 shrink-0 items-center justify-center rounded-full bg-lime-500/15 text-xs font-semibold text-lime-300 ring-1 ring-inset ring-lime-500/30">{i + 1}</span>
            <span className="pt-0.5">{s}</span>
          </li>
        ))}
      </ol>
      <pre className="mono max-h-72 overflow-auto rounded-[3px] bg-black/50 p-4 text-xs leading-relaxed text-ink-100/80 ring-1 ring-white/10">{join.compose}</pre>
      <div className="flex justify-end">
        <CopyButton text={join.compose} label="Copy compose file" />
      </div>
    </div>
  )
}
