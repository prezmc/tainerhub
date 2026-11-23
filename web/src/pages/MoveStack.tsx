import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, ArrowRightLeft, Ban, Check, HardDrive, Trash2 } from 'lucide-react'
import { api, ApiError, bytes, errorText, size, timeAgo, type Host, type MoveReport, type PlacementHost, type StackDetail, type StackMove, type StackSummary } from '../api'
import { Badge, Button, Card, ErrorNote, Modal, cx, useConfirm, useToast } from '../ui'
import { useFleet } from '../fleet'
import { usePlacement } from '../placement'
import { useOpenJob } from '../stacks'

/** Rough speed of a copy through the hub on a gigabit LAN, for the downtime estimate. */
export const COPY_BYTES_PER_SEC = 80 * 1024 * 1024

const pct = (n?: number) => (n === undefined ? '–' : `${Math.round(n)}%`)

export function duration(sec: number) {
  if (sec < 60) return 'under a minute'
  const m = Math.round(sec / 60)
  return m < 60 ? `about ${m} min` : `about ${(m / 60).toFixed(1)} h`
}

/** Admin card on a stack's page: move it to another host, and deal with what earlier moves left behind. */
export function MoveSection({ d, s }: { d: StackDetail; s: StackSummary }) {
  const [open, setOpen] = useState(false)
  const { hosts } = useFleet()
  const notes = d.moves.filter((m) => m.state !== 'running' && (m.state !== 'done' || (m.leftovers && !m.leftoversRemovedAt)))
  const latest = d.moves[0]
  // Show only what still matters: leftovers to clean up, and the last move if it failed.
  const shown = notes.filter((m) => (m.leftovers && !m.leftoversRemovedAt) || m === latest)
  const why = !s.online ? `${s.hostName} is offline.` : s.job ? 'Wait for the running operation to finish.' : s.pending ? 'Deploy the saved changes first, so what moves is what runs.' : !d.stack.deployedAt ? 'Deploy it first.' : null
  return (
    <Card className="p-5">
      <div className="flex flex-wrap items-center gap-4">
        <div className="min-w-0 flex-1">
          <h2 className="font-display text-lg font-bold text-white">Move to another host</h2>
          <p className="text-sm text-ink-100/60">Copies its volumes and folder to another host and runs it there. The old copy stays on {s.hostName} until you delete it.</p>
        </div>
        <Button icon={ArrowRightLeft} disabled={!!why} title={why ?? undefined} onClick={() => setOpen(true)}>
          Move…
        </Button>
      </div>
      {why && s.online && <p className="mt-3 text-xs text-ink-100/50">{why}</p>}
      {shown.length > 0 && (
        <div className="mt-4 space-y-2">
          {shown.map((m) => (
            <MoveNote key={m.id} m={m} hosts={hosts} refresh={['stack', d.stack.id]} />
          ))}
        </div>
      )}
      {open && <MoveDialog s={s} onClose={() => setOpen(false)} />}
    </Card>
  )
}

/** One earlier move: what it left behind, with a button to delete it. Shared with container moves. */
export function MoveNote({ m, hosts, refresh }: { m: StackMove; hosts: Host[]; refresh: unknown[] }) {
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const name = (id: string) => hosts.find((h) => h.id === id)?.name ?? 'a removed host'
  const left = m.leftovers && !m.leftoversRemovedAt ? m.leftovers : undefined
  const what = left
    ? [...(left.container ? [`container ${left.container}`] : []), ...(left.volumes ?? []).map((v) => `volume ${v}`), ...(left.dir ? ['the stack folder'] : [])].join(', ')
    : ''
  const it = m.kind === 'container' ? 'The container' : 'The stack'
  const remove = useMutation({
    mutationFn: () => api(`/api/moves/${m.id}/leftovers`, { method: 'DELETE' }),
    onSuccess: () => {
      toast('good', `Deleted from ${name(left!.host_id)}`)
      qc.invalidateQueries({ queryKey: refresh })
    },
    onError: (e) => toast('bad', errorText(e)),
  })
  const tone = m.state === 'done' ? 'bg-white/[0.03] ring-white/[0.06]' : 'bg-amber-500/10 ring-amber-500/20 text-amber-100'
  return (
    <div className={cx('flex flex-wrap items-start gap-3 rounded-[3px] px-3 py-2.5 text-xs leading-relaxed ring-1 ring-inset', tone)}>
      {m.state === 'done' ? <HardDrive className="mt-0.5 size-3.5 shrink-0 text-ink-100/50" /> : <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />}
      <div className="min-w-0 flex-1">
        {m.state === 'done' && (
          <p>
            Moved from {name(m.fromHost)} to {name(m.toHost)} {timeAgo(m.finishedAt)} by {m.startedBy} ({size(m.bytes)} copied).
            {left && ` The old copy is still on ${name(left.host_id)}: ${what}.`}
          </p>
        )}
        {m.state === 'failed' && (
          <p>
            The move to {name(m.toHost)} {timeAgo(m.finishedAt)} failed and was undone: {m.error}
            {left && ` Part of the copy couldn't be removed from ${name(left.host_id)}: ${what}.`}
          </p>
        )}
        {m.state === 'interrupted' && (
          <p>
            The move to {name(m.toHost)} was interrupted when the hub restarted. {it} may be stopped on {name(m.fromHost)}: start it {m.kind === 'container' ? 'from the Containers page' : 'from the buttons above'}.
            {left && ` Part of the copy may be on ${name(left.host_id)}: ${what}.`}
          </p>
        )}
      </div>
      {left && (
        <Button
          variant="dangerGhost"
          icon={Trash2}
          loading={remove.isPending}
          onClick={async () => {
            const ok = await confirm({
              title: `Delete the copy on ${name(left.host_id)}?`,
              body: `Deletes ${what} from ${name(left.host_id)}. This data is gone for good.`,
              confirm: 'Delete',
              danger: true,
            })
            if (ok) remove.mutate()
          }}
        >
          Delete {m.state === 'done' ? 'old copy' : 'partial copy'}
        </Button>
      )}
    </div>
  )
}

function MoveDialog({ s, onClose }: { s: StackSummary; onClose: () => void }) {
  const placement = usePlacement(s.id)
  const [to, setTo] = useState('')
  const [ack, setAck] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const openJob = useOpenJob()
  const qc = useQueryClient()
  const others = (placement.data?.hosts ?? []).filter((p) => !p.current)
  useEffect(() => {
    if (!to && placement.data) {
      const best = others.find((p) => p.suggested) ?? others.find((p) => p.eligible)
      if (best) setTo(best.hostId)
    }
  }, [placement.data]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => setAck(false), [to])

  const check = useQuery({
    queryKey: ['move-check', s.id, to],
    queryFn: () => api<MoveReport>(`/api/stacks/${s.id}/move?host=${to}`),
    enabled: !!to,
    staleTime: 0,
    retry: false,
  })
  const rep = check.data
  const move = useMutation({
    mutationFn: () => api<{ jobId: string }>(`/api/stacks/${s.id}/move`, { method: 'POST', body: { hostId: to, outside: rep?.confirm ?? [] } }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['stacks'] })
      qc.invalidateQueries({ queryKey: ['stack', s.id] })
      openJob(res.jobId)
      onClose()
    },
    onError: (e) => {
      if (e instanceof ApiError && e.status === 409) check.refetch()
      setError(errorText(e))
    },
  })
  const ready = rep && rep.blockers.length === 0 && (rep.confirm.length === 0 || ack)
  const est = rep && rep.totalBytes > 0 ? duration(rep.totalBytes / COPY_BYTES_PER_SEC + 30) : null

  return (
    <Modal open onClose={onClose} title={`Move ${s.name}`} wide>
      <div className="space-y-5 text-sm text-ink-100/80">
        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-ink-100/50">To</h3>
          {placement.isPending && <p className="text-xs text-ink-100/50">Loading hosts…</p>}
          {placement.data && others.length === 0 && <p className="text-xs">There's no other host to move it to.</p>}
          <div className="space-y-1.5" role="radiogroup" aria-label="Host to move to">
            {others.map((p) => (
              <HostChoice key={p.hostId} p={p} selected={p.hostId === to} onSelect={() => setTo(p.hostId)} />
            ))}
          </div>
          {placement.data?.footprint && (
            <p className="mt-2 text-xs text-ink-100/50">
              {s.name} uses about {Math.round(placement.data.footprint.cpu)}% of a core and {bytes(placement.data.footprint.mem)} of memory now; “after” adds that to each host.
            </p>
          )}
        </div>

        {to && (
          <div className="space-y-3">
            <h3 className="text-xs font-semibold uppercase tracking-wide text-ink-100/50">What happens</h3>
            {check.isFetching && !rep && <p className="text-xs text-ink-100/50">Checking both hosts…</p>}
            {check.isError && <ErrorNote error={errorText(check.error)} />}
            {rep && <Report rep={rep} ack={ack} setAck={setAck} est={est} />}
          </div>
        )}

        <ErrorNote error={error} />
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" icon={ArrowRightLeft} disabled={!ready || check.isFetching} loading={move.isPending} onClick={() => move.mutate()}>
            {rep ? `Move to ${rep.toName}` : 'Move'}
          </Button>
        </div>
      </div>
    </Modal>
  )
}

export function HostChoice({ p, selected, onSelect }: { p: PlacementHost; selected: boolean; onSelect: () => void }) {
  const figures = p.cpu !== undefined || p.mem !== undefined
  return (
    <button
      role="radio"
      aria-checked={selected}
      onClick={onSelect}
      className={cx(
        'flex w-full flex-wrap items-center gap-x-3 gap-y-1 rounded-[3px] px-3 py-2 text-left ring-1 ring-inset transition',
        selected ? 'bg-lime-400/10 ring-lime-400/50' : 'bg-white/[0.03] ring-white/[0.08] hover:ring-white/20',
      )}
    >
      <span className={cx('size-3 shrink-0 rounded-full ring-2', selected ? 'bg-lime-400 ring-lime-400/40' : 'ring-white/20')} />
      <span className="font-medium text-white">{p.hostName}</span>
      {p.suggested && <Badge tone="good">suggested</Badge>}
      {!p.eligible && p.reason && <span className="text-xs text-amber-200">{p.reason}</span>}
      {figures && (
        <span className="ml-auto text-xs tabular-nums text-ink-100/60">
          CPU {pct(p.cpu)} → {pct(p.cpuAfter)} · memory {pct(p.mem)} → {pct(p.memAfter)}
        </span>
      )}
    </button>
  )
}

/** The check's result: what blocks the move, the steps it takes, and what to confirm. */
export function Report({ rep, ack, setAck, est }: { rep: MoveReport; ack: boolean; setAck: (v: boolean) => void; est: string | null }) {
  const ctr = rep.container !== undefined
  return (
    <div className="space-y-3">
      {rep.blockers.length > 0 && (
        <ul className="space-y-1.5 rounded-[3px] bg-rose-500/10 p-3 text-xs text-rose-100 ring-1 ring-inset ring-rose-500/20">
          {rep.blockers.map((b) => (
            <li key={b} className="flex gap-2">
              <Ban className="mt-0.5 size-3.5 shrink-0" />
              {b}
            </li>
          ))}
        </ul>
      )}
      {rep.blockers.length === 0 && (
        <ol className="list-decimal space-y-1 pl-5 text-xs leading-relaxed">
          {rep.images.length > 0 && (
            <li>
              Pull {rep.images.length === 1 ? 'its image' : `its ${rep.images.length} images`} on {rep.toName}, while it still runs on {rep.fromName}.
            </li>
          )}
          {(!ctr || rep.running) && (
            <li>
              Stop it on {rep.fromName}. <strong className="text-white">It's down from here</strong>
              {est ? ` for ${est}.` : ' until it runs on ' + rep.toName + '.'}
            </li>
          )}
          <li>
            {rep.copy.length === 0 ? (
              ctr ? 'Nothing to copy: it keeps no data in volumes.' : 'Nothing to copy: it keeps no data in volumes or its folder.'
            ) : (
              <>
                Copy {rep.copy.map((c) => (c.kind === 'volume' ? (/^[0-9a-f]{64}$/.test(c.name) ? `its unnamed volume ${c.name.slice(0, 12)}` : `volume ${c.name}`) : 'its folder')).join(', ')}
                {rep.totalBytes > 0 && ` (${rep.sizeUnknown ? 'at least ' : ''}${size(rep.totalBytes)}${rep.diskFree ? `; ${size(rep.diskFree)} free on ${rep.toName}` : ''})`}.
              </>
            )}
          </li>
          {ctr ? (
            <li>
              {rep.networks && rep.networks.length > 0 && `Create network ${rep.networks.join(', ')} on ${rep.toName}. `}
              Create it on {rep.toName} with the same settings{rep.running ? ', start it and wait until it runs and is healthy.' : ' (stopped, as it is now).'}
            </li>
          ) : (
            <li>Deploy on {rep.toName} and wait until everything that ran before runs and is healthy.</li>
          )}
          <li>
            Remove the old {ctr ? 'container' : 'containers'} on {rep.fromName}. {ctr ? 'Its volumes stay' : 'Its data stays'} there until you delete {ctr ? 'them' : 'it'} here.
          </li>
        </ol>
      )}
      {rep.blockers.length === 0 && (
        <p className="flex items-center gap-1.5 text-xs text-ink-100/50">
          <Check className="size-3.5 text-lime-400" />
          If anything fails, the copy is removed and it {ctr && !rep.running ? 'stays as it was' : 'starts again'} on {rep.fromName}.
        </p>
      )}
      {rep.recreated.length > 0 && (
        <p className="text-xs text-ink-100/60">
          Not copied, recreated from the compose file (their data lives elsewhere, such as NFS): {rep.recreated.join(', ')}.
        </p>
      )}
      {rep.warnings.length > 0 && (
        <ul className="space-y-1.5 rounded-[3px] bg-amber-500/10 p-3 text-xs text-amber-100 ring-1 ring-inset ring-amber-500/20">
          {rep.warnings.map((w) => (
            <li key={w} className="flex gap-2">
              <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
              {w}
            </li>
          ))}
        </ul>
      )}
      {rep.confirm.length > 0 && rep.blockers.length === 0 && (
        <label className="flex items-start gap-2.5 rounded-[3px] bg-white/[0.03] p-3 text-xs ring-1 ring-inset ring-white/[0.08]">
          <input type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} className="mt-0.5 accent-lime-400" />
          <span>
            I understand these {ctr ? 'host folders' : 'folders'} aren't copied, and {rep.toName} needs them at the same paths:
            <span className="mono mt-1 block text-ink-100/70">{rep.confirm.join(', ')}</span>
          </span>
        </label>
      )}
    </div>
  )
}
