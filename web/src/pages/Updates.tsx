import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowUpCircle, CircleCheck, LoaderCircle, RefreshCw } from 'lucide-react'
import { api, errorText, timeAgo, type HubState } from '../api'
import { Badge, Button, Card, cx, useConfirm, useToast } from '../ui'

type HostUpdate = { id: string; name: string; online: boolean; version: string; current: boolean; hubHost: boolean; canUpdate: boolean; note?: string }
type Updates = {
  hubVersion: string
  hubCurrent: boolean
  hubManaged: boolean
  latest: { version: string; commit: string; title: string; at: string } | null
  error?: string
  hosts: HostUpdate[]
}

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
    </Card>
  )
}
