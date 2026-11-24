import { useMemo, useState } from 'react'
import { useMutation, useQueries, useQueryClient } from '@tanstack/react-query'
import { Boxes, Download, Eraser, HardDrive, Layers, Network, RefreshCw, Search, Trash2, WifiOff, type LucideIcon } from 'lucide-react'
import { api, can, errorText, size, timeAgo, type Host, type ImageRes, type NetworkRes, type PruneResult, type VolumeRes } from '../api'
import { loadPref, savePref, useFleet } from '../fleet'
import { Button, Card, EmptyState, ErrorNote, IconButton, Input, Menu, Modal, cx, useConfirm, useToast } from '../ui'
import { useMe } from './AppLayout'

type Tab = 'images' | 'volumes' | 'networks'
type Row<T> = T & { host: Host }

const TABS: { key: Tab; label: string; icon: LucideIcon; color: string }[] = [
  { key: 'images', label: 'Images', icon: Layers, color: 'bg-lime-400 text-ink-950' },
  { key: 'volumes', label: 'Volumes', icon: HardDrive, color: 'bg-lime-400 text-ink-950' },
  { key: 'networks', label: 'Networks', icon: Network, color: 'bg-lime-400 text-ink-950' },
]

export function ResourcesPage() {
  const me = useMe()
  const { hosts } = useFleet()
  const online = hosts.filter((h) => h.online)
  const [tab, setTab] = useState<Tab>(() => loadPref('th.res.tab', ['images', 'volumes', 'networks'] as const, 'images'))
  const [hostFilter, setHostFilter] = useState<string>('all')
  const [q, setQ] = useState('')
  const [pulling, setPulling] = useState(false)

  const results = useQueries({
    queries: online.map((h) => ({
      queryKey: ['res', tab, h.id],
      queryFn: () => api<unknown[]>(`/api/hosts/${h.id}/${tab}`),
      refetchInterval: 30000,
    })),
  })
  const loading = results.some((r) => r.isPending)
  const errors = results.map((r, i) => (r.isError ? `${online[i].name}: ${errorText(r.error)}` : null)).filter(Boolean)
  const rows = useMemo(
    () =>
      results.flatMap((r, i) => ((r.data as object[] | undefined) ?? []).map((x) => ({ ...x, host: online[i] }))).filter((x) => hostFilter === 'all' || x.host.id === hostFilter),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [results.map((r) => r.dataUpdatedAt).join(), hostFilter, tab],
  )

  return (
    <div className="animate-[rise_300ms_ease-out]">
      <div className="mb-6 flex flex-wrap items-center gap-3">
        <h1 className="mr-2 font-display text-3xl font-bold tracking-tight text-white">Resources</h1>
        <div className="flex gap-1 rounded-[3px] bg-ink-950/60 p-0.5 ring-1 ring-line">
          {TABS.map((t) => (
            <button
              key={t.key}
              onClick={() => {
                setTab(t.key)
                savePref('th.res.tab', t.key)
              }}
              className={cx(
                'flex items-center gap-1.5 rounded-[2px] px-3.5 py-1.5 font-display text-[12px] font-semibold uppercase tracking-[0.08em] transition',
                tab === t.key ? t.color : 'text-ink-100/70 hover:text-white',
              )}
            >
              <t.icon className="size-4" />
              {t.label}
            </button>
          ))}
        </div>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <div className="relative">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-ink-100/50" />
            <input
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Filter"
              className="w-44 rounded-[2px] bg-ink-950/70 py-1.5 pl-9 pr-3 text-sm text-white ring-1 ring-inset ring-white/12 outline-none placeholder:text-ink-100/35 focus:ring-lime-400/60"
            />
          </div>
          {tab === 'images' && can(me.user, 'operator') && (
            <Button variant="primary" icon={Download} onClick={() => setPulling(true)}>
              Pull image
            </Button>
          )}
          <PruneMenu tab={tab} hosts={online} />
        </div>
      </div>

      <div className="mb-5 flex flex-wrap gap-2">
        {[{ id: 'all', name: 'All hosts' }, ...online].map((h) => (
          <button
            key={h.id}
            onClick={() => setHostFilter(h.id)}
            className={cx(
              'rounded-[2px] px-3.5 py-1 text-xs font-semibold transition',
              hostFilter === h.id ? 'bg-lime-400 text-ink-950' : 'text-ink-100/75 ring-1 ring-inset ring-line hover:text-white hover:ring-lime-400/40',
            )}
          >
            {h.name}
          </button>
        ))}
        {hosts.length > online.length && (
          <span className="flex items-center gap-1 px-2 text-xs text-ink-100/50">
            <WifiOff className="size-3.5" /> {hosts.length - online.length} offline host(s) not shown
          </span>
        )}
      </div>

      {errors.length > 0 && (
        <div className="mb-4">
          <ErrorNote error={errors.join(' · ')} />
        </div>
      )}
      {online.length === 0 ? (
        <EmptyState icon={WifiOff} title="No hosts online">
          Resources are read live from each host's agent.
        </EmptyState>
      ) : tab === 'images' ? (
        <ImagesTable rows={rows as Row<ImageRes>[]} q={q} loading={loading} />
      ) : tab === 'volumes' ? (
        <VolumesTable rows={rows as Row<VolumeRes>[]} q={q} loading={loading} />
      ) : (
        <NetworksTable rows={rows as Row<NetworkRes>[]} q={q} loading={loading} />
      )}

      <PullModal open={pulling} onClose={() => setPulling(false)} hosts={online} defaultHost={hostFilter === 'all' ? online[0]?.id : hostFilter} />
    </div>
  )
}

// ---------------- shared bits ----------------

function useResourceMutation() {
  const qc = useQueryClient()
  const toast = useToast()
  return useMutation({
    mutationFn: (v: { path: string; method: 'DELETE' | 'POST'; body?: unknown; done: (r: unknown) => string }) => api<unknown>(v.path, { method: v.method, body: v.body }).then((r) => v.done(r)),
    onSuccess: (msg) => {
      toast('good', msg)
      qc.invalidateQueries({ queryKey: ['res'] })
    },
    onError: (e) => toast('bad', errorText(e)),
  })
}

function UsedBy({ names }: { names: string[] }) {
  if (names.length === 0) return <span className="rounded-[2px] bg-amber-400/10 px-1.5 py-[3px] font-mono text-[10.5px] font-medium uppercase leading-none tracking-wider text-amber-300 ring-1 ring-amber-400/25">unused</span>
  return (
    <span className="flex flex-wrap gap-1">
      {names.slice(0, 3).map((n) => (
        <span key={n} className="rounded-[2px] bg-emerald-400/10 px-1.5 py-[3px] font-mono text-[10.5px] font-medium uppercase leading-none tracking-wider text-emerald-200 ring-1 ring-emerald-400/20">
          {n}
        </span>
      ))}
      {names.length > 3 && <span className="text-[11px] text-ink-100/50">+{names.length - 3}</span>}
    </span>
  )
}

function Table({ head, children, empty, loading }: { head: string[]; children: React.ReactNode; empty: boolean; loading: boolean }) {
  return (
    <Card className="overflow-hidden">
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead className="border-b border-white/[0.08] text-left text-[11px] font-semibold uppercase tracking-[0.14em] text-ink-100/50">
            <tr>
              {head.map((h, i) => (
                <th key={i} className={cx('px-5 py-3', i === head.length - 1 && 'text-right')}>
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-white/[0.05]">{children}</tbody>
        </table>
      </div>
      {empty && <div className="px-5 py-12 text-center text-sm text-ink-100/50">{loading ? 'Loading from your agents…' : 'Nothing here.'}</div>}
    </Card>
  )
}

const match = (q: string, ...xs: (string | undefined)[]) => !q || xs.some((x) => x?.toLowerCase().includes(q.toLowerCase()))

// ---------------- images ----------------

function ImagesTable({ rows, q, loading }: { rows: Row<ImageRes>[]; q: string; loading: boolean }) {
  const me = useMe()
  const confirm = useConfirm()
  const m = useResourceMutation()
  const list = rows.filter((r) => match(q, r.host.name, r.id, ...r.tags))
  const total = list.reduce((a, r) => a + r.size, 0)
  const unused = list.filter((r) => r.usedBy.length === 0).reduce((a, r) => a + r.size, 0)
  const operator = can(me.user, 'operator')
  return (
    <>
      <Summary items={[`${list.length} images`, `${size(total)} on disk`, `${size(unused)} unused`]} />
      <Table head={['Image', 'Host', 'Size', 'Created', 'Used by', '']} empty={list.length === 0} loading={loading}>
        {list.map((r) => {
          const tag = r.tags[0]
          return (
            <tr key={r.host.id + r.id} className="transition hover:bg-white/[0.03]">
              <td className="px-5 py-3">
                <div className="mono text-[13px] text-white">{tag ?? <span className="text-ink-100/50">&lt;untagged&gt;</span>}</div>
                <div className="mono text-[11px] text-ink-100/40">
                  {r.id.replace('sha256:', '').slice(0, 12)}
                  {r.tags.length > 1 && ` · +${r.tags.length - 1} tags`}
                </div>
              </td>
              <td className="px-5 py-3 text-ink-100/80">{r.host.name}</td>
              <td className="mono px-5 py-3 text-ink-100/80">{size(r.size)}</td>
              <td className="px-5 py-3 text-ink-100/60">{timeAgo(new Date(r.created * 1000).toISOString())}</td>
              <td className="px-5 py-3">
                <UsedBy names={r.usedBy} />
              </td>
              <td className="px-5 py-3">
                {operator && (
                  <div className="flex justify-end gap-0.5">
                    {tag && (
                      <IconButton
                        icon={RefreshCw}
                        label={`Pull ${tag} again (update)`}
                        tone="good"
                        onClick={() =>
                          m.mutate({
                            path: `/api/hosts/${r.host.id}/images/pull`,
                            method: 'POST',
                            body: { ref: tag },
                            done: (res) => `${tag} on ${r.host.name}: ${(res as { status?: string })?.status || 'pulled'}`,
                          })
                        }
                      />
                    )}
                    <IconButton
                      icon={Trash2}
                      label={r.usedBy.length ? 'In use: remove its containers first' : 'Remove image'}
                      tone="bad"
                      disabled={r.usedBy.length > 0}
                      onClick={async () => {
                        if (await confirm({ title: `Remove ${tag ?? 'this image'}?`, body: `Deletes the image from ${r.host.name}. It can be pulled again later.`, confirm: 'Remove', danger: true }))
                          m.mutate({ path: `/api/hosts/${r.host.id}/images/${encodeURIComponent(r.id)}`, method: 'DELETE', done: () => `Image removed from ${r.host.name}` })
                      }}
                    />
                  </div>
                )}
              </td>
            </tr>
          )
        })}
      </Table>
    </>
  )
}

// ---------------- volumes ----------------

function VolumesTable({ rows, q, loading }: { rows: Row<VolumeRes>[]; q: string; loading: boolean }) {
  const me = useMe()
  const confirm = useConfirm()
  const m = useResourceMutation()
  const list = rows.filter((r) => match(q, r.host.name, r.name, r.project))
  const admin = can(me.user, 'admin')
  return (
    <>
      <Summary items={[`${list.length} volumes`, `${list.filter((v) => v.usedBy.length === 0).length} unused`]} />
      <Table head={['Volume', 'Host', 'Stack', 'Driver', 'Used by', '']} empty={list.length === 0} loading={loading}>
        {list.map((r) => (
          <tr key={r.host.id + r.name} className="transition hover:bg-white/[0.03]">
            <td className="px-5 py-3">
              <div className="mono max-w-xs truncate text-[13px] text-white" title={r.name}>
                {r.name}
              </div>
              <div className="mono max-w-xs truncate text-[11px] text-ink-100/40" title={r.mountpoint}>
                {r.mountpoint}
              </div>
            </td>
            <td className="px-5 py-3 text-ink-100/80">{r.host.name}</td>
            <td className="px-5 py-3">
              {r.project ? (
                <span className="inline-flex items-center gap-1 text-cyan-300">
                  <Boxes className="size-3.5" />
                  {r.project}
                </span>
              ) : (
                <span className="text-ink-100/40">–</span>
              )}
            </td>
            <td className="px-5 py-3 text-ink-100/60">{r.driver}</td>
            <td className="px-5 py-3">
              <UsedBy names={r.usedBy} />
            </td>
            <td className="px-5 py-3 text-right">
              {admin && (
                <IconButton
                  icon={Trash2}
                  label={r.usedBy.length ? 'In use' : 'Delete volume and its data'}
                  tone="bad"
                  disabled={r.usedBy.length > 0}
                  onClick={async () => {
                    if (
                      await confirm({
                        title: `Delete volume ${r.name}?`,
                        body: <>All data in this volume on {r.host.name} is <b>permanently deleted</b>. This can't be undone.</>,
                        confirm: 'Delete volume',
                        danger: true,
                      })
                    )
                      m.mutate({ path: `/api/hosts/${r.host.id}/volumes/${encodeURIComponent(r.name)}`, method: 'DELETE', done: () => `Volume ${r.name} deleted` })
                  }}
                />
              )}
            </td>
          </tr>
        ))}
      </Table>
    </>
  )
}

// ---------------- networks ----------------

function NetworksTable({ rows, q, loading }: { rows: Row<NetworkRes>[]; q: string; loading: boolean }) {
  const me = useMe()
  const confirm = useConfirm()
  const m = useResourceMutation()
  const list = rows.filter((r) => match(q, r.host.name, r.name, r.project, r.driver))
  const operator = can(me.user, 'operator')
  return (
    <>
      <Summary items={[`${list.length} networks`, `${list.filter((n) => !n.builtin && n.usedBy.length === 0).length} unused`]} />
      <Table head={['Network', 'Host', 'Driver', 'Scope', 'Used by', '']} empty={list.length === 0} loading={loading}>
        {list.map((r) => (
          <tr key={r.host.id + r.id} className="transition hover:bg-white/[0.03]">
            <td className="px-5 py-3">
              <div className="flex items-center gap-2">
                <span className="mono text-[13px] text-white">{r.name}</span>
                {r.builtin && <span className="rounded-[2px] bg-white/10 px-1.5 py-[3px] font-mono text-[10.5px] font-medium uppercase leading-none tracking-wider text-ink-100/70">built-in</span>}
                {r.internal && <span className="rounded-[2px] bg-amber-400/10 px-1.5 py-[3px] font-mono text-[10.5px] font-medium uppercase leading-none tracking-wider text-amber-300">internal</span>}
              </div>
              {r.project && <div className="text-[11px] text-cyan-300">stack: {r.project}</div>}
            </td>
            <td className="px-5 py-3 text-ink-100/80">{r.host.name}</td>
            <td className="px-5 py-3 text-ink-100/60">{r.driver}</td>
            <td className="px-5 py-3 text-ink-100/60">{r.scope}</td>
            <td className="px-5 py-3">{r.builtin ? <span className="text-xs text-ink-100/40">–</span> : <UsedBy names={r.usedBy} />}</td>
            <td className="px-5 py-3 text-right">
              {operator && !r.builtin && (
                <IconButton
                  icon={Trash2}
                  label={r.usedBy.length ? 'In use' : 'Remove network'}
                  tone="bad"
                  disabled={r.usedBy.length > 0}
                  onClick={async () => {
                    if (await confirm({ title: `Remove network ${r.name}?`, body: `Removes it from ${r.host.name}.`, confirm: 'Remove', danger: true }))
                      m.mutate({ path: `/api/hosts/${r.host.id}/networks/${encodeURIComponent(r.id)}`, method: 'DELETE', done: () => `Network ${r.name} removed` })
                  }}
                />
              )}
            </td>
          </tr>
        ))}
      </Table>
    </>
  )
}

function Summary({ items }: { items: string[] }) {
  return (
    <div className="mb-3 flex flex-wrap gap-2 text-xs text-ink-100/70">
      {items.map((s) => (
        <span key={s} className="rounded-[2px] bg-white/[0.05] px-3 py-1 ring-1 ring-white/10">
          {s}
        </span>
      ))}
    </div>
  )
}

// ---------------- prune & pull ----------------

function PruneMenu({ tab, hosts }: { tab: Tab; hosts: Host[] }) {
  const me = useMe()
  const confirm = useConfirm()
  const m = useResourceMutation()
  const allowed = tab === 'volumes' ? can(me.user, 'admin') : can(me.user, 'operator')
  if (!allowed || hosts.length === 0) return null
  const copy = {
    images: { light: 'dangling images (untagged layers)', all: 'every image not used by a container' },
    volumes: { light: 'unused anonymous volumes', all: 'every unused volume, including named ones' },
    networks: { light: 'unused networks', all: 'unused networks' },
  }[tab]
  const go = async (h: Host, all: boolean) => {
    const ok = await confirm({
      title: `Prune ${tab} on ${h.name}?`,
      body: (
        <>
          Removes {all ? copy.all : copy.light}.{tab === 'volumes' && <b> Data in those volumes is deleted permanently.</b>}
        </>
      ),
      confirm: 'Prune',
      danger: true,
    })
    if (!ok) return
    m.mutate({
      path: `/api/hosts/${h.id}/${tab}/prune${all ? '?all=1' : ''}`,
      method: 'POST',
      done: (r) => {
        const p = r as PruneResult
        const kept = p.kept?.length ? `. Kept ${p.kept.length}: ${p.kept[0].name} (${p.kept[0].reason})${p.kept.length > 1 ? ` and ${p.kept.length - 1} more` : ''}` : ''
        return `${h.name}: removed ${p.deleted} ${tab}${p.reclaimed ? `, freed ${size(p.reclaimed)}` : ''}${kept}`
      },
    })
  }
  return (
    <Menu
      trigger={(toggle, menu) => (
        <Button {...menu} icon={Eraser} onClick={toggle} loading={m.isPending}>
          Prune
        </Button>
      )}
      items={hosts.flatMap((h) => [
        { label: `${h.name}: ${tab === 'networks' ? 'unused' : tab === 'images' ? 'dangling' : 'anonymous'}`, icon: Eraser, onClick: () => go(h, false) },
        ...(tab !== 'networks' ? [{ label: `${h.name}: all unused`, icon: Trash2, danger: true, onClick: () => go(h, true) }] : []),
      ])}
    />
  )
}

function PullModal({ open, onClose, hosts, defaultHost }: { open: boolean; onClose: () => void; hosts: Host[]; defaultHost?: string }) {
  const [ref, setRef] = useState('')
  const [hostId, setHostId] = useState<string | undefined>(undefined)
  const [error, setError] = useState<string | null>(null)
  const m = useResourceMutation()
  const target = hostId ?? defaultHost
  return (
    <Modal open={open} onClose={onClose} title="Pull an image">
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault()
          setError(null)
          const h = hosts.find((x) => x.id === target)
          if (!h) return setError('Pick a host')
          m.mutate(
            { path: `/api/hosts/${h.id}/images/pull`, method: 'POST', body: { ref: ref.trim() }, done: (r) => `${h.name}: ${(r as { status?: string })?.status || 'pulled ' + ref}` },
            {
              onSuccess: () => {
                setRef('')
                onClose()
              },
              onError: (err) => setError(errorText(err)),
            },
          )
        }}
      >
        <Input label="Image" placeholder="nginx:1.27 or ghcr.io/org/app:tag" value={ref} onChange={(e) => setRef(e.target.value)} autoFocus required className="mono" hint="Public registries for now; private registry credentials come in a later phase." />
        <div>
          <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Host</span>
          <div className="flex flex-wrap gap-2">
            {hosts.map((h) => (
              <button
                type="button"
                key={h.id}
                onClick={() => setHostId(h.id)}
                className={cx('rounded-[2px] px-3.5 py-1.5 text-sm font-semibold transition', target === h.id ? 'bg-lime-400 text-ink-950' : 'text-ink-100/75 ring-1 ring-inset ring-line hover:ring-lime-400/40')}
              >
                {h.name}
              </button>
            ))}
          </div>
        </div>
        <ErrorNote error={error} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" icon={Download} loading={m.isPending}>
            {m.isPending ? 'Pulling…' : 'Pull'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}
