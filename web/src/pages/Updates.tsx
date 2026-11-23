import { useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowUpCircle, CircleCheck, Feather, LoaderCircle, RefreshCw, TriangleAlert } from 'lucide-react'
import { api, errorText, timeAgo, type Host, type HubState } from '../api'
import { Badge, Button, Card, CopyButton, ErrorNote, Modal, cx, useConfirm, useToast } from '../ui'

type HostUpdate = {
  id: string
  name: string
  online: boolean
  version: string
  docker: string
  dockerBehind: boolean
  current: boolean
  hubHost: boolean
  canUpdate: boolean
  cgroupStats?: boolean
  statsFromDocker: boolean
  note?: string
}
type Updates = {
  hubVersion: string
  hubCurrent: boolean
  hubManaged: boolean
  latest: { version: string; commit: string; title: string; at: string } | null
  error?: string
  dockerLatest?: string
  hosts: HostUpdate[]
}

type DockerPlan = {
  manager: string
  installed?: string
  candidate?: string
  systemd: boolean
  supported: boolean
  available: boolean
  reason?: string
  command: string
}

/** "5:29.10.0-1~debian.12~bookworm" -> "29.10.0" */
const pkgVersion = (v?: string) => v?.match(/\d+\.\d+\.\d+/)?.[0] ?? v ?? ''

/** Polls until the hub answers with a different version, then reloads the page. */
async function waitForNewHub(from: string) {
  const deadline = Date.now() + 5 * 60_000
  await new Promise((r) => setTimeout(r, 5000))
  while (Date.now() < deadline) {
    try {
      const s = await api<HubState>('/api/state')
      if (s.version !== from) return true
    } catch {
      /* restarting */
    }
    await new Promise((r) => setTimeout(r, 3000))
  }
  return false
}

/** Admins: compare every server with the newest published version and update in place. */
export function UpdatesCard() {
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const u = useQuery({ queryKey: ['updates'], queryFn: () => api<Updates>('/api/updates'), refetchInterval: 60_000 })
  const [busy, setBusy] = useState<string | null>(null) // host id, or 'all'
  const [restarting, setRestarting] = useState(false)
  const [dockerFor, setDockerFor] = useState<HostUpdate | null>(null)
  const [lightening, setLightening] = useState<string | null>(null) // host id

  if (!u.data) return null
  const d = u.data
  const stale = d.hosts.filter((h) => !h.current)
  const hubHost = d.hosts.find((h) => h.hubHost)

  const updateOne = async (h: HostUpdate) => {
    await api(`/api/hosts/${h.id}/update`, { method: 'POST' })
    if (h.hubHost) {
      setRestarting(true)
      const ok = await waitForNewHub(d.hubVersion)
      if (ok) window.location.reload()
      else {
        setRestarting(false)
        toast('bad', "TainerHub didn't come back with a new version within 5 minutes. Check the server with: docker compose logs")
      }
    } else {
      toast('good', `${h.name} is updating; it reconnects in a few seconds`)
      setTimeout(() => qc.invalidateQueries({ queryKey: ['updates'] }), 15_000)
    }
  }

  const run = async (target: HostUpdate | 'all') => {
    const list = target === 'all' ? [...stale.filter((h) => !h.hubHost && h.canUpdate), ...stale.filter((h) => h.hubHost && h.canUpdate)] : [target]
    const touchesHub = list.some((h) => h.hubHost)
    const ok = await confirm({
      title: target === 'all' ? 'Update everything?' : `Update ${target.name}?`,
      body: (
        <>
          Pulls the newest TainerHub images and recreates only the containers that changed. Your other containers aren't touched.
          {touchesHub && ' The hub restarts too: this page will reconnect on its own in a minute or so.'}
        </>
      ),
      confirm: 'Update',
    })
    if (!ok) return
    setBusy(target === 'all' ? 'all' : target.id)
    try {
      for (const h of list) await updateOne(h)
    } catch (e) {
      toast('bad', errorText(e))
    } finally {
      setBusy(null)
    }
  }

  if (restarting) {
    return (
      <Card className="mb-6 flex items-center gap-3 p-5">
        <LoaderCircle className="size-5 animate-spin text-lime-300" />
        <div>
          <div className="font-semibold text-white">TainerHub is updating</div>
          <div className="text-sm text-ink-100/60">It's pulling the new version and restarting. This page reloads when it's back.</div>
        </div>
      </Card>
    )
  }

  // Hosts whose agent says where its stats come from (agents before this don't).
  const statHosts = d.hosts.filter((h) => h.online && h.cgroupStats !== undefined)
  const setSource = async (h: HostUpdate, source: 'docker' | 'cgroup') => {
    setLightening(h.id)
    try {
      await api(`/api/hosts/${h.id}/stats-source`, { method: 'PUT', body: { source } })
      toast('good', source === 'docker' ? `${h.name} now asks Docker for stats` : `${h.name} now uses lighter stats`)
      await qc.invalidateQueries({ queryKey: ['updates'] })
    } catch (e) {
      toast('bad', errorText(e))
    } finally {
      setLightening(null)
    }
  }
  const lighten = async (h: HostUpdate) => {
    const ok = await confirm({
      title: `Turn on lighter stats on ${h.name}?`,
      body: (
        <>
          The agent will read container CPU and memory from the kernel instead of asking Docker, which costs the server less CPU. It adds one read-only mount (
          <code className="mono text-lime-300">/sys/fs/cgroup</code>) in a new <code className="mono text-lime-300">compose.override.yml</code> next to your compose
          file, then restarts only the agent. Your other containers{h.hubHost && ' and the hub'} aren't touched.
        </>
      ),
      confirm: 'Turn on',
    })
    if (!ok) return
    setLightening(h.id)
    try {
      await api(`/api/hosts/${h.id}/lighter-stats`, { method: 'POST' })
      const deadline = Date.now() + 90_000
      while (Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 3000))
        const now = await api<Updates>('/api/updates').catch(() => null)
        if (now?.hosts.find((x) => x.id === h.id)?.cgroupStats) {
          toast('good', `${h.name} now reads stats from the kernel`)
          qc.setQueryData(['updates'], now)
          return
        }
      }
      toast(
        'bad',
        `${h.name}'s agent didn't come back with the mount. If compose.override.yml was already there, add "- /sys/fs/cgroup:/host/sys/fs/cgroup:ro" under the agent's volumes yourself.`,
      )
    } catch (e) {
      toast('bad', errorText(e))
    } finally {
      setLightening(null)
    }
  }

  const upToDate = stale.length === 0 && d.hubCurrent
  return (
    <Card className={cx('mb-6 p-5', !upToDate && d.latest && 'ring-lime-400/40')}>
      <div className="flex flex-wrap items-center gap-3">
        {upToDate ? <CircleCheck className="size-5 text-emerald-400" /> : <ArrowUpCircle className="size-5 text-lime-300" />}
        <div className="min-w-0 flex-1">
          <div className="font-semibold text-white">
            {!d.latest ? 'Updates' : upToDate ? 'Everything is up to date' : 'An update is available'}
          </div>
          <div className="text-sm text-ink-100/60">
            Running <span className="mono text-ink-50">{d.hubVersion}</span>
            {d.latest && (
              <>
                {' · '}latest <span className="mono text-ink-50">{d.latest.version}</span>
                {' · '}
                <span title={d.latest.title}>“{d.latest.title}”</span>, {timeAgo(d.latest.at)}
              </>
            )}
            {d.error && <span className="text-amber-200"> · {d.error}</span>}
          </div>
        </div>
        {!upToDate && d.latest && stale.some((h) => h.canUpdate) && (
          <Button variant="primary" icon={RefreshCw} loading={busy === 'all'} disabled={!!busy} onClick={() => run('all')}>
            Update everything
          </Button>
        )}
      </div>
      {!upToDate && d.latest && (
        <ul className="mt-4 divide-y divide-line rounded-[3px] ring-1 ring-line">
          {stale.map((h) => (
            <li key={h.id} className="flex flex-wrap items-center gap-3 px-4 py-2.5 text-sm">
              <span className="font-semibold text-white">{h.name}</span>
              {h.hubHost && <Badge tone="accent">hub + agent</Badge>}
              <span className="mono text-xs text-ink-100/50">{h.version || 'unknown version'}</span>
              <span className="ml-auto text-xs text-ink-100/50">{h.note}</span>
              <Button icon={RefreshCw} loading={busy === h.id} disabled={!!busy || !h.canUpdate} onClick={() => run(h)}>
                Update
              </Button>
            </li>
          ))}
        </ul>
      )}
      {!d.hubCurrent && !d.hubManaged && d.latest && (
        <p className="mt-3 text-xs text-ink-100/60">
          This hub was installed without its built-in agent, so it can't update itself. On its server run{' '}
          <code className="mono text-lime-300">cd ~/tainerhub && docker compose pull && docker compose up -d</code>.
        </p>
      )}
      {hubHost && !hubHost.online && !d.hubCurrent && <p className="mt-3 text-xs text-amber-200">The hub's own agent is offline, so the hub can't update itself right now.</p>}

      <div className="mt-5 border-t border-line pt-4">
        <div className="mb-2 flex flex-wrap items-baseline gap-2">
          <h3 className="font-semibold text-white">Docker Engine</h3>
          {d.dockerLatest && <span className="text-xs text-ink-100/50">latest release {d.dockerLatest}</span>}
        </div>
        <ul className="divide-y divide-line rounded-[3px] ring-1 ring-line">
          {d.hosts
            .filter((h) => h.online && h.docker)
            .map((h) => (
              <li key={h.id} className="flex flex-wrap items-center gap-3 px-4 py-2.5 text-sm">
                <span className="font-semibold text-white">{h.name}</span>
                <span className="mono text-xs text-ink-100/60">Docker {h.docker}</span>
                {h.dockerBehind ? <Badge tone="warn">newer available</Badge> : <Badge tone="good">up to date</Badge>}
                <span className="ml-auto" />
                <Button icon={ArrowUpCircle} variant={h.dockerBehind ? 'secondary' : 'ghost'} onClick={() => setDockerFor(h)}>
                  {h.dockerBehind ? 'Update Docker…' : 'Check…'}
                </Button>
              </li>
            ))}
        </ul>
      </div>
      {statHosts.length > 0 && (
        <div className="mt-5 border-t border-line pt-4">
          <div className="mb-2 flex flex-wrap items-baseline gap-2">
            <h3 className="font-semibold text-white">Container stats</h3>
            <span className="text-xs text-ink-100/50">lighter stats read CPU and memory from the kernel; asking Docker costs the server more CPU</span>
          </div>
          <ul className="divide-y divide-line rounded-[3px] ring-1 ring-line">
            {statHosts.map((h) => (
              <li key={h.id} className="flex flex-wrap items-center gap-3 px-4 py-2.5 text-sm">
                <span className="font-semibold text-white">{h.name}</span>
                {!h.cgroupStats ? (
                  <Badge tone="warn">from Docker</Badge>
                ) : h.statsFromDocker ? (
                  <Badge tone="info">from Docker (your choice)</Badge>
                ) : (
                  <Badge tone="good">lighter stats</Badge>
                )}
                <span className="ml-auto" />
                {!h.cgroupStats ? (
                  <Button icon={Feather} loading={lightening === h.id} disabled={!!lightening} onClick={() => lighten(h)}>
                    Turn on lighter stats
                  </Button>
                ) : h.statsFromDocker ? (
                  <Button icon={Feather} loading={lightening === h.id} disabled={!!lightening} onClick={() => setSource(h, 'cgroup')}>
                    Use lighter stats
                  </Button>
                ) : (
                  <Button
                    variant="ghost"
                    loading={lightening === h.id}
                    disabled={!!lightening}
                    title="If the numbers look wrong, compare them with Docker's own. You can switch back any time."
                    onClick={() => setSource(h, 'docker')}
                  >
                    Use Docker instead
                  </Button>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}
      {dockerFor && <DockerUpgradeDialog host={dockerFor} onClose={() => setDockerFor(null)} onDone={() => qc.invalidateQueries({ queryKey: ['updates'] })} />}
    </Card>
  )
}

/**
 * Upgrading Docker restarts every container on the server, so this asks the
 * server first (how Docker is installed, what its repository offers), spells
 * out the consequences, and only proceeds after an explicit OK.
 */
function DockerUpgradeDialog({ host, onClose, onDone }: { host: HostUpdate; onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const [plan, setPlan] = useState<DockerPlan | null>(null)
  const [planError, setPlanError] = useState<string | null>(null)
  const [ok, setOk] = useState(false)
  const [phase, setPhase] = useState<'plan' | 'running' | 'done' | 'failed'>('plan')
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    api<DockerPlan>(`/api/hosts/${host.id}/docker/plan`, { method: 'POST' })
      .then(setPlan)
      .catch((e) => setPlanError(errorText(e)))
  }, [host.id])

  const start = async () => {
    setError(null)
    setPhase('running')
    try {
      await api(`/api/hosts/${host.id}/docker/upgrade`, { method: 'POST', body: { confirm: host.name } })
    } catch (e) {
      setError(errorText(e))
      setPhase('failed')
      return
    }
    // Docker restarts; the agent drops off and comes back on the new version.
    const want = pkgVersion(plan?.candidate)
    const deadline = Date.now() + 10 * 60_000
    while (Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 4000))
      try {
        const hs = await api<Host[]>('/api/hosts')
        const v = hs.find((h) => h.id === host.id)?.hello?.docker_version
        if (v && v !== host.docker && (!want || v === want)) {
          setPhase('done')
          toast('good', `Docker on ${host.name} is now ${v}`)
          onDone()
          return
        }
      } catch {
        /* the hub itself may be restarting if this is its server */
      }
    }
    setError(`Docker on ${host.name} didn't come back on a new version within 10 minutes. On the server, check: journalctl -u tainerhub-docker-upgrade, or /var/log/tainerhub-docker-upgrade.log`)
    setPhase('failed')
  }

  return (
    <Modal open onClose={phase === 'running' ? () => {} : onClose} title={`Update Docker on ${host.name}`} wide>
      <div className="space-y-4 text-sm text-ink-100/80">
        {!plan && !planError && (
          <p className="flex items-center gap-2">
            <LoaderCircle className="size-4 animate-spin text-lime-300" /> Checking how Docker is installed on {host.name} and what its package repository offers (this can take a minute)…
          </p>
        )}
        {planError && <ErrorNote error={planError} />}
        {plan && !plan.supported && (
          <>
            <p>{plan.reason}.</p>
            {plan.command && (
              <>
                <p>To update Docker yourself, run this on {host.name}:</p>
                <pre className="mono overflow-x-auto whitespace-pre-wrap break-all rounded-[3px] bg-black/40 p-3 text-xs text-lime-200 ring-1 ring-line">{plan.command}</pre>
                <div className="flex justify-end">
                  <CopyButton text={plan.command} label="Copy command" />
                </div>
              </>
            )}
          </>
        )}
        {plan && plan.supported && !plan.available && (
          <p>
            Docker {host.docker} is the newest version {host.name}'s package repository offers ({plan.manager}). Docker may have released something newer that the repository doesn't have
            yet; check again later.
          </p>
        )}
        {plan && plan.supported && plan.available && phase === 'plan' && (
          <>
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 rounded-[3px] bg-black/30 p-3 ring-1 ring-line">
              <dt className="text-ink-100/50">Now</dt>
              <dd className="mono text-white">{pkgVersion(plan.installed) || host.docker}</dd>
              <dt className="text-ink-100/50">After</dt>
              <dd className="mono text-lime-200">{pkgVersion(plan.candidate)}</dd>
              <dt className="text-ink-100/50">How</dt>
              <dd>{plan.manager === 'apt' ? 'apt (Docker’s apt repository)' : 'dnf (Docker’s rpm repository)'}, run by the server’s systemd</dd>
            </dl>
            <div className="space-y-2 rounded-[3px] border-l-4 border-rose-400 bg-rose-500/[0.08] p-3 ring-1 ring-inset ring-rose-400/25">
              <div className="flex items-center gap-2 font-semibold text-rose-200">
                <TriangleAlert className="size-4" /> What happens
              </div>
              <ul className="list-disc space-y-1 pl-5 text-ink-100/80">
                <li>
                  Docker restarts on {host.name}: <strong className="text-white">every container there stops and starts again</strong>, usually within a minute.
                </li>
                <li>Containers without a restart policy (restart: no) stay stopped until you start them.</li>
                <li>{host.name} shows as offline in TainerHub until Docker is back.</li>
                {host.hubHost && (
                  <li>
                    <strong className="text-white">This server runs TainerHub itself</strong>, so TainerHub is unavailable for a minute or two.
                  </li>
                )}
              </ul>
            </div>
            <details className="text-xs">
              <summary className="cursor-pointer text-ink-100/60 hover:text-white">Command that runs on the server</summary>
              <pre className="mono mt-2 overflow-x-auto whitespace-pre-wrap break-all rounded-[3px] bg-black/40 p-3 text-ink-100/70 ring-1 ring-line">{plan.command}</pre>
            </details>
            <label className="flex cursor-pointer items-start gap-2.5 rounded-[3px] p-3 ring-1 ring-inset ring-line has-[:checked]:ring-rose-400/50">
              <input type="checkbox" checked={ok} onChange={(e) => setOk(e.target.checked)} className="mt-0.5 accent-rose-500" />
              <span>
                I understand that <strong className="text-white">every container on {host.name} will restart</strong>, and I want to update Docker now.
              </span>
            </label>
          </>
        )}
        {phase === 'running' && (
          <p className="flex items-center gap-2">
            <LoaderCircle className="size-4 animate-spin text-lime-300" /> Updating Docker on {host.name}. It will drop offline and come back on {pkgVersion(plan?.candidate)}; this
            usually takes one to three minutes.
          </p>
        )}
        {phase === 'done' && <p className="text-emerald-300">Done: Docker on {host.name} is up to date. Containers with a restart policy are running again.</p>}
        <ErrorNote error={error} />
        <div className="flex justify-end gap-2">
          {phase !== 'running' && (
            <Button variant="ghost" onClick={onClose}>
              {phase === 'done' ? 'Close' : 'Cancel'}
            </Button>
          )}
          {plan?.supported && plan.available && phase === 'plan' && (
            <Button variant="danger" icon={ArrowUpCircle} disabled={!ok} onClick={start}>
              Update Docker on {host.name}
            </Button>
          )}
        </div>
      </div>
    </Modal>
  )
}
