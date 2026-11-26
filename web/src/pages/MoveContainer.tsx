import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowRightLeft } from 'lucide-react'
import { api, ApiError, bytes, errorText, type Container, type MoveReport, type StackMove } from '../api'
import { Button, ErrorNote, Modal } from '../ui'
import { useFleet } from '../fleet'
import { ownContainer } from '../own'
import { usePlacement } from '../placement'
import { useOpenJob } from '../stacks'
import { COPY_BYTES_PER_SEC, HostChoice, MoveNote, Report, duration } from './MoveStack'

/** A standalone container (not part of a compose project) that TainerHub can move. */
export const movable = (c: Container) => !c.project && !ownContainer(c)

/** Moves a standalone container to another host: pick the host, see the check, start the job. */
export function MoveContainerDialog({ c, onClose }: { c: Container; onClose: () => void }) {
  const placement = usePlacement(undefined, { hostId: c.hostId, id: c.id })
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

  const base = `/api/hosts/${c.hostId}/containers/${encodeURIComponent(c.id)}/move`
  const check = useQuery({
    queryKey: ['container-move-check', c.hostId, c.id, to],
    queryFn: () => api<MoveReport>(`${base}?host=${to}`),
    enabled: !!to,
    staleTime: 0,
    retry: false,
  })
  const rep = check.data
  const move = useMutation({
    mutationFn: () => api<{ jobId: string }>(base, { method: 'POST', body: { hostId: to, outside: rep?.confirm ?? [] } }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['container-moves'] })
      openJob(res.jobId)
      onClose()
    },
    onError: (e) => {
      if (e instanceof ApiError && e.status === 409) {
        if (typeof e.data.jobId === 'string') openJob(e.data.jobId)
        check.refetch()
      }
      setError(errorText(e))
    },
  })
  const ready = rep && rep.blockers.length === 0 && (rep.confirm.length === 0 || ack)
  const est = rep && rep.running ? duration(rep.totalBytes / COPY_BYTES_PER_SEC + 20) : null

  return (
    <Modal open onClose={onClose} title={`Move ${c.name}`} wide>
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
          {placement.data?.footprint && c.state === 'running' && (
            <p className="mt-2 text-xs text-ink-100/50">
              {c.name} uses about {Math.round(placement.data.footprint.cpu)}% of a core and {bytes(placement.data.footprint.mem)} of memory now; “after” adds that to each host.
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

/** Recent container moves (admins only). */
export function useContainerMoves(enabled: boolean) {
  return useQuery({
    queryKey: ['container-moves'],
    queryFn: () => api<StackMove[]>('/api/container-moves'),
    enabled,
    refetchInterval: 30000,
  })
}

/**
 * Earlier moves of this container that still matter, for its details: the
 * old copy left on the other host, or a failed move's partial copy.
 */
export function ContainerMoveNotes({ c }: { c: Container }) {
  const { hosts } = useFleet()
  const moves = useContainerMoves(true)
  const mine = (moves.data ?? []).filter((m) => m.stackName === c.name && (m.toHost === c.hostId || m.fromHost === c.hostId) && m.state !== 'running')
  const latest = mine[0]
  const shown = mine.filter((m) => (m.leftovers && !m.leftoversRemovedAt) || (m === latest && m.state !== 'done'))
  if (shown.length === 0) return null
  return (
    <div className="mt-4 space-y-2">
      {shown.map((m) => (
        <MoveNote key={m.id} m={m} hosts={hosts} refresh={['container-moves']} />
      ))}
    </div>
  )
}
