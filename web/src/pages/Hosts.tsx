import { lazy, Suspense, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Clock, Cpu, Ellipsis, KeyRound, Layers, MemoryStick, Pencil, Plus, Server, ShieldCheck, Trash2 } from 'lucide-react'
import { api, bytes, can, errorText, hostAddress, stackProblem, timeAgo, type Host, type JoinTokenResponse } from '../api'
import { Badge, Button, Card, CopyButton, EmptyState, ErrorNote, IconButton, Input, Menu, Modal, PageHeader, StatusDot, cx, useConfirm, useToast } from '../ui'
import { useMe } from './AppLayout'
import { useHostsQuery } from '../fleet'

// Admins only: load the updates card when it's shown.
const UpdatesCard = lazy(() => import('./Updates').then((m) => ({ default: m.UpdatesCard })))
import { LoadBars } from '../placement'
import { directAlternative, joinCommand } from '../join'

export function HostsPage() {
  const me = useMe()
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const admin = can(me.user, 'admin')
  const hosts = useHostsQuery()
  const [adding, setAdding] = useState(false)
  const [name, setName] = useState('')
  // Agents dial the hub directly (not through a reverse proxy) on port 7443.
  // The hub suggests the address: TAINERHUB_AGENT_ADDR, the last one used, or
  // the address this page was opened on unless that's the reverse proxy.
  const [agentAddr, setAgentAddr] = useState('')
  const [reissuing, setReissuing] = useState<Host | null>(null)
  const openAdd = async (forHost?: Host) => {
    if (forHost) setReissuing(forHost)
    else setAdding(true)
    setError(null)
    try {
      const d = await api<{ agentAddr: string }>('/api/hosts/agent-addr')
      setAgentAddr((cur) => cur || d.agentAddr)
    } catch {
      // leave the field for the admin to fill in
    }
  }
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
    onSuccess: (res) => {
      setReissuing(null)
      setJoin(res)
    },
    onError: (e) => setError(errorText(e)),
  })
  const [renaming, setRenaming] = useState<Host | null>(null)
  const [newName, setNewName] = useState('')
  const [renameError, setRenameError] = useState<string | null>(null)
  const rename = useMutation({
    mutationFn: () => api(`/api/hosts/${renaming!.id}`, { method: 'PUT', body: { name: newName.trim() } }),
    onSuccess: () => {
      toast('good', `Renamed to ${newName.trim()}`)
      setRenaming(null)
      qc.invalidateQueries({ queryKey: ['hosts'] })
    },
    onError: (e) => setRenameError(errorText(e)),
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
  const addrInput = (
    <Input
      label="Hub address for agents"
      value={agentAddr}
      onChange={(e) => setAgentAddr(e.target.value)}
      placeholder="192.168.1.10:7443"
      required
      className="mono"
      hint="The hub server's own LAN IP or name, port 7443. The agent connects here directly, never through a reverse proxy, so not your HTTPS address. TainerHub remembers it for the next host."
    />
  )

  return (
    <div>
      <PageHeader title="Hosts" subtitle={hosts.data ? `${online} of ${hosts.data.length} connected` : undefined}>
        {admin && (
          <Button
            variant="primary"
            icon={Plus}
            onClick={() => openAdd()}
          >
            Add host
          </Button>
        )}
      </PageHeader>

      {admin && (
        <Suspense fallback={null}>
          <UpdatesCard />
        </Suspense>
      )}
      {hosts.isError && !hosts.data ? (
        <ErrorNote error={`Couldn't load hosts: ${errorText(hosts.error)}`} />
      ) : hosts.data?.length === 0 ? (
        <EmptyState icon={Server} title="No hosts yet">
          Add a host to get a one-time join token and a ready-made compose file for its agent.
        </EmptyState>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {hosts.data?.map((h) => (
            <HostCard
              key={h.id}
              h={h}
              admin={admin}
              onToken={() => openAdd(h)}
              onRemove={() => askRemove(h)}
              onRename={() => {
                setRenaming(h)
                setNewName(h.name)
                setRenameError(null)
              }}
            />
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
          {addrInput}
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

      <Modal open={!!reissuing} onClose={() => setReissuing(null)} title={reissuing ? `New join token for ${reissuing.name}` : ''}>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            setError(null)
            if (reissuing) reissue.mutate(reissuing)
          }}
        >
          <p className="text-sm text-ink-100/70">The old token stops working. Run the new command on {reissuing?.name}.</p>
          {addrInput}
          <ErrorNote error={error} />
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={() => setReissuing(null)}>
              Cancel
            </Button>
            <Button variant="primary" icon={KeyRound} loading={reissue.isPending}>
              Create join token
            </Button>
          </div>
        </form>
      </Modal>

      <Modal open={!!renaming} onClose={() => setRenaming(null)} title={renaming ? `Rename ${renaming.name}` : ''}>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            setRenameError(null)
            rename.mutate()
          }}
        >
          <Input label="Name" value={newName} onChange={(e) => setNewName(e.target.value)} autoFocus required maxLength={64} hint="Only changes how it's shown in TainerHub; nothing changes on the server." />
          <ErrorNote error={renameError} />
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={() => setRenaming(null)}>
              Cancel
            </Button>
            <Button variant="primary" icon={Pencil} loading={rename.isPending} disabled={!newName.trim() || newName.trim() === renaming?.name}>
              Rename
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

function HostCard({ h, admin, onToken, onRemove, onRename }: { h: Host; admin: boolean; onToken: () => void; onRemove: () => void; onRename: () => void }) {
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
            <Link to="/containers" search={{ host: h.id }} className="min-w-0 hover:text-lime-200 hover:underline" title={`Show ${h.name}'s containers`}>
              <h2 className="truncate text-base font-semibold text-white hover:text-lime-200">{h.name}</h2>
            </Link>
            <StatusDot tone={h.online ? 'good' : 'idle'} pulse={h.online} />
          </div>
          <p className="truncate text-xs text-ink-100/50">
            {h.hello ? [hostAddress(h), h.hello.os, h.hello.arch].filter(Boolean).join(' · ') : h.enrolledAt ? `Last seen ${timeAgo(h.lastSeenAt)}` : 'Waiting for its agent to connect'}
          </p>
        </div>
        {admin && (
          <Menu
            trigger={(toggle, menu) => <IconButton {...menu} icon={Ellipsis} label="Host actions" onClick={toggle} />}
            items={[
              { label: 'Rename', icon: Pencil, onClick: onRename },
              { label: 'New join token', icon: KeyRound, onClick: onToken },
              { label: 'Remove host', icon: Trash2, onClick: onRemove, danger: true },
            ]}
          />
        )}
      </div>

      {h.online && h.hello ? (
        <>
          <div className="mt-5 flex items-center gap-4">
            <Link to="/containers" search={{ host: h.id }} title={`Show ${h.name}'s containers`} className="rounded-full transition hover:brightness-125">
              <Ring pct={pct} />
            </Link>
            <div className="space-y-1 text-sm">
              <div>
                <Link to="/containers" search={{ host: h.id, view: 'running' }} className="hover:underline">
                  <span className="font-semibold tabular-nums text-white">{h.running}</span> <span className="text-ink-100/60">running</span>
                </Link>
              </div>
              <div className="text-ink-100/50">
                <Link to="/containers" search={{ host: h.id, view: 'stopped' }} className="hover:text-white hover:underline">
                  <span className="tabular-nums">{stopped}</span> stopped
                </Link>
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
          {h.load && <LoadBars load={h.load} memTotal={h.hello.mem_total} />}
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
  // One line to paste on the new server: the hub serves a script that writes
  // the agent's compose file, starts it and waits until it's connected.
  // Behind a reverse proxy, the proxy's access list may refuse a server on
  // another subnet (curl: 403), so offer the hub's own address too.
  const page = window.location.origin
  const direct = directAlternative(page, join.directURL)
  // Default to the hub directly: the new server must reach the hub's address
  // anyway (for port 7443), while a proxy's access list may refuse it.
  const [useDirect, setUseDirect] = useState(true)
  const base = useDirect && direct ? direct : page
  const command = joinCommand(base, join.joinToken, join.agentAddr)
  return (
    <div className="space-y-5 text-sm text-ink-100/80">
      <p>
        On <strong className="text-white">{join.host.name}</strong>, paste this into a terminal (as root, or a user in the <code className="mono text-lime-300">docker</code> group):
      </p>
      {direct && (
        <div role="radiogroup" aria-label="Download the installer from" className="flex flex-wrap items-center gap-2 text-xs">
          <span className="text-ink-100/50">Download the installer from</span>
          {[
            { on: false, label: 'this page’s address', url: page },
            { on: true, label: 'the hub directly', url: direct },
          ].map((o) => (
            <button
              key={o.label}
              type="button"
              role="radio"
              aria-checked={useDirect === o.on}
              title={o.url}
              onClick={() => setUseDirect(o.on)}
              className={cx('rounded-[3px] px-2.5 py-1 ring-1 ring-inset transition', useDirect === o.on ? 'bg-lime-400/10 text-lime-200 ring-lime-400/40' : 'text-ink-100/70 ring-line hover:text-white')}
            >
              {o.label}
            </button>
          ))}
        </div>
      )}
      <pre className="mono overflow-x-auto whitespace-pre-wrap break-all rounded-[3px] bg-black/50 p-4 text-xs leading-relaxed text-lime-200 ring-1 ring-line">{command}</pre>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="text-xs text-ink-100/50">
          Works once, until {new Date(join.expiresAt).toLocaleTimeString()}. It installs the agent in <code className="mono">~/tainerhub-agent</code>, and the host turns green here when it's connected.
        </span>
        <CopyButton text={command} label="Copy command" variant="primary" />
      </div>
      <p className="text-xs text-ink-100/50">
        The server must reach <code className="mono text-ink-100/80">{join.agentAddr}</code> (agents connect there) and <code className="mono text-ink-100/80">{base}</code> (to download the installer).
        {direct && !useDirect && (
          <>
            {' '}
            If curl answers <code className="mono text-ink-100/80">403</code>, your reverse proxy's access list doesn't admit this server's network: choose <em>the hub directly</em> above, add the server's subnet to the access list, or set it up by hand below.
          </>
        )}
        {direct && useDirect && <> The installer link carries the one-time token over plain HTTP on your network; choose <em>this page's address</em> to download it over HTTPS through your proxy instead.</>}
      </p>
      <details className="rounded-[3px] ring-1 ring-line">
        <summary className="cursor-pointer select-none px-4 py-2.5 text-xs text-ink-100/70 hover:text-white">Set it up by hand instead</summary>
        <div className="space-y-3 border-t border-line p-4">
          <p className="text-xs">
            Save this as <code className="mono text-lime-300">~/tainerhub-agent/compose.yml</code> on {join.host.name}, then run <code className="mono text-lime-300">docker compose up -d</code> in that folder.
          </p>
          <pre className="mono max-h-64 overflow-auto rounded-[3px] bg-black/50 p-3 text-xs leading-relaxed text-ink-100/80 ring-1 ring-line">{join.compose}</pre>
          <div className="flex justify-end">
            <CopyButton text={join.compose} label="Copy compose file" />
          </div>
        </div>
      </details>
    </div>
  )
}
