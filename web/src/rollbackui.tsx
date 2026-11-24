import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowRight, History, Undo2 } from 'lucide-react'
import { api, ApiError, errorText, timeAgo, type RollbackPreview, type StackDetail, type StackSummary } from './api'
import { changedServices, imageText, triggerText } from './rollback'
import { Button, ErrorNote, Modal, cx, useToast } from './ui'
import { useOpenJob, useStackAction } from './stacks'

const when = (iso: string) => new Date(iso).toLocaleString()

/** "Roll back": opens the preview; enabled once a rollback point exists. */
export function RollbackButton({ d, s, disabled }: { d: StackDetail; s: StackSummary; disabled?: boolean }) {
  const [open, setOpen] = useState(false)
  const point = d.rollbackPoint
  const title = !point
    ? 'Nothing to roll back to yet: TainerHub records what the stack runs right before each deploy, Pull & redeploy or automatic update.'
    : !s.online
      ? `${s.hostName} is offline`
      : `Go back to the images it ran ${triggerText(point.trigger)} (${timeAgo(point.createdAt)})`
  return (
    <>
      <Button variant="ghost" icon={Undo2} disabled={disabled || !point || !s.online} title={title} onClick={() => setOpen(true)}>
        Roll back
      </Button>
      {open && <RollbackModal s={s} onClose={() => setOpen(false)} />}
    </>
  )
}

function RollbackModal({ s, onClose }: { s: StackSummary; onClose: () => void }) {
  const qc = useQueryClient()
  const toast = useToast()
  const openJob = useOpenJob()
  const preview = useQuery({ queryKey: ['rollback', s.id], queryFn: () => api<RollbackPreview>(`/api/stacks/${s.id}/rollback`), staleTime: 0, retry: false })
  const roll = useMutation({
    mutationFn: () => api<{ jobId: string }>(`/api/stacks/${s.id}/rollback`, { method: 'POST' }),
    onSuccess: (res) => {
      onClose()
      openJob(res.jobId)
      qc.invalidateQueries({ queryKey: ['stacks'] })
      qc.invalidateQueries({ queryKey: ['stack', s.id] })
    },
    onError: (e) => {
      if (e instanceof ApiError && e.status === 409 && typeof e.data.jobId === 'string') openJob(e.data.jobId)
      toast('bad', `${s.name}: ${errorText(e)}`)
    },
  })
  const p = preview.data
  const changes = p ? changedServices(p) : 0
  return (
    <Modal open onClose={onClose} title={`Roll back ${s.name}?`} wide>
      {preview.isPending && <p className="text-sm text-ink-100/60">Asking {s.hostName} what runs now…</p>}
      <ErrorNote error={preview.error ? errorText(preview.error) : null} />
      {p && (
        <div className="space-y-4 text-sm">
          <p className="text-ink-100/70">
            Puts each service back on the image it ran {triggerText(p.point.trigger)}, recorded {when(p.point.createdAt)} ({timeAgo(p.point.createdAt)}
            {p.point.createdBy ? `, by ${p.point.createdBy}` : ''}). Nothing is pulled or built, and the stack's files stay as they are. It then stays on these images,
            through restarts, until you return it to the latest.
          </p>
          <div className="overflow-x-auto rounded-[3px] ring-1 ring-line">
            <table className="w-full text-left text-[13px]">
              <thead className="border-b border-line text-[11px] uppercase tracking-[0.12em] text-ink-100/50">
                <tr>
                  <th className="px-3 py-2">Service</th>
                  <th className="px-3 py-2">Now</th>
                  <th className="px-1 py-2" />
                  <th className="px-3 py-2">After the rollback</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {p.services.map((r) => (
                  <tr key={r.service} className={cx(r.same && 'text-ink-100/45')}>
                    <td className="px-3 py-2 font-mono text-white">{r.service}</td>
                    <td className="mono px-3 py-2 break-all">{imageText(r.current)}</td>
                    <td className="px-1 py-2">
                      <ArrowRight className="size-3.5 text-ink-100/40" />
                    </td>
                    <td className={cx('mono px-3 py-2 break-all', !r.same && 'text-lime-200')}>
                      {imageText(r.previous)}
                      {r.same && <span className="ml-2 font-sans text-xs">(unchanged)</span>}
                      {r.previous.created && <div className="font-sans text-[11px] text-ink-100/40">built {timeAgo(r.previous.created)}</div>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {p.currentError && <p className="text-xs text-amber-200">Couldn't ask the host what runs now: {p.currentError}</p>}
          {p.composeChanged && (
            <p className="text-xs text-amber-200">The compose file was deployed again since this point was recorded. Only the images go back; the rest of the current compose file stays.</p>
          )}
          {changes === 0 && <p className="text-xs text-ink-100/60">Every service already runs these images; rolling back pins the stack to them.</p>}
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button variant="primary" icon={Undo2} loading={roll.isPending} onClick={() => roll.mutate()}>
              Roll back {changes > 0 ? `${changes} ${changes === 1 ? 'service' : 'services'}` : ''}
            </Button>
          </div>
        </div>
      )}
    </Modal>
  )
}

/** Shown while the stack runs a rollback: "Return to latest" is a Pull & redeploy without the pin. */
export function RolledBackBanner({ d, s, operator }: { d: StackDetail; s: StackSummary; operator: boolean }) {
  const { run, pending } = useStackAction()
  if (!d.stack.rolledBackTo) return null
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-[3px] bg-amber-400/[0.07] px-4 py-3 text-sm ring-1 ring-amber-400/30">
      <History className="size-5 shrink-0 text-amber-300" />
      <div className="min-w-0 flex-1">
        <div className="font-semibold text-amber-100">Running the images from {when(d.stack.rolledBackTo)} (rolled back)</div>
        <div className="text-ink-100/60">
          Restarts, stops and starts keep these images, and automatic updates leave the stack alone. Return to latest runs Pull & redeploy, which drops the pin.
        </div>
      </div>
      {operator && (
        <Button icon={ArrowRight} loading={pending?.action === 'pull'} disabled={!s.online || !!s.job || !!pending} onClick={() => run(s, 'pull')}>
          Return to latest
        </Button>
      )}
    </div>
  )
}
