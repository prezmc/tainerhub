import { Link } from '@tanstack/react-router'
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowDownToLine, Boxes, CircleCheck, CircleX, Download, LoaderCircle, X } from 'lucide-react'
import { useFleet, useLive } from './fleet'
import { api, ApiError, errorText, isRegistryAuthError, registryHint, type Container, type Host, type StackAction, type StackStatus, type StackSummary } from './api'
import { Badge, IconButton, cx, download, useConfirm, useLayer, useToast, type Tone } from './ui'
import { RegistryAuthHint } from './registries'

// ---- live stack list (managed stacks + compose projects found on hosts) ----

export function useStacks() {
  // The list itself (and running jobs) comes from a short poll; container
  // counts and status are recomputed from the live fleet stream so they
  // change the moment a container does.
  // Poll fast only while a job runs or the live stream is down.
  const live = useLive()
  const q = useQuery({
    queryKey: ['stacks'],
    queryFn: () => api<StackSummary[]>('/api/stacks'),
    refetchInterval: (query) => (!live || query.state.data?.some((s) => s.job) ? 5000 : 30000),
  })
  const { containers, hosts } = useFleet()
  const data = useMemo(() => q.data?.map((s) => withLive(s, containers, hosts)), [q.data, containers, hosts])
  return { ...q, data }
}

/** Recomputes a stack's counts and status from live containers (mirrors summarize() on the hub). */
export function withLive(s: StackSummary, containers: Container[], hosts: Host[]): StackSummary {
  const host = hosts.find((h) => h.id === s.hostId)
  const online = host ? host.online : s.online
  if (!online) return { ...s, online, status: 'unknown' }
  const mine = containers.filter((c) => c.hostId === s.hostId && c.project === s.name)
  const running = mine.filter((c) => c.state === 'running').length
  const unhealthy = mine.filter((c) => c.status.includes('(unhealthy)') || c.state === 'restarting').length
  let status: StackStatus
  if (mine.length === 0) status = s.managed && !s.deployedAt ? 'not deployed' : 'down'
  else if (running === mine.length) status = 'running'
  else if (running === 0) status = 'stopped'
  else status = 'partial'
  return { ...s, online, containers: mine.length, running, unhealthy, status }
}

const statusTone: Record<StackStatus, Tone> = {
  running: 'good',
  partial: 'warn',
  stopped: 'idle',
  down: 'idle',
  'not deployed': 'info',
  unknown: 'idle',
}

export function stackTone(s: StackSummary): Tone {
  if (s.unhealthy > 0) return 'bad'
  return statusTone[s.status]
}

export function StackStatusBadge({ s }: { s: StackSummary }) {
  if (s.job) {
    return (
      <Badge tone="accent">
        <LoaderCircle className="size-3 animate-spin" />
        {jobVerb[s.job.action] ?? s.job.action}…
      </Badge>
    )
  }
  return <Badge tone={stackTone(s)}>{s.unhealthy > 0 ? `${s.unhealthy} unhealthy` : s.status}</Badge>
}

const jobVerb: Record<string, string> = {
  deploy: 'deploying',
  pull: 'pulling',
  start: 'starting',
  stop: 'stopping',
  restart: 'restarting',
  down: 'taking down',
  remove: 'removing',
  rollback: 'rolling back',
}

const jobTitle: Record<string, string> = {
  move: 'Move',
  deploy: 'Deploy',
  pull: 'Pull & redeploy',
  start: 'Start',
  stop: 'Stop',
  restart: 'Restart',
  down: 'Down',
  remove: 'Delete',
  rollback: 'Roll back',
}

export const ACTION_LABELS: Record<StackAction, string> = {
  deploy: 'Deploy',
  pull: 'Pull & redeploy',
  start: 'Start',
  stop: 'Stop',
  restart: 'Restart',
  down: 'Down',
}

// ---- job drawer, opened from anywhere ----

export const JobContext = createContext<(jobId: string) => void>(() => {})
export const useOpenJob = () => useContext(JobContext)

/** Runs a lifecycle action, confirming the disruptive ones, and opens its output. */
export function useStackAction() {
  const toast = useToast()
  const confirm = useConfirm()
  const openJob = useOpenJob()
  const qc = useQueryClient()
  const m = useMutation({
    mutationFn: ({ s, action }: { s: StackSummary; action: StackAction }) => api<{ jobId: string }>(`/api/stacks/${s.id}/${action}`, { method: 'POST' }),
    onSuccess: (res) => {
      openJob(res.jobId)
      qc.invalidateQueries({ queryKey: ['stacks'] })
    },
    onError: (e, { s }) => {
      // Already running: show the operation that is in progress.
      if (e instanceof ApiError && e.status === 409 && typeof e.data.jobId === 'string') openJob(e.data.jobId)
      toast('bad', `${s.name}: ${errorText(e)}`)
    },
  })
  const run = useCallback(
    async (s: StackSummary, action: StackAction) => {
      const ask: Partial<Record<StackAction, { title: string; body: string; confirm: string }>> = {
        stop: { title: `Stop ${s.name}?`, body: `Every container in the stack stops on ${s.hostName}. They keep their data and can be started again.`, confirm: 'Stop stack' },
        down: {
          title: `Take ${s.name} down?`,
          body: `Its containers and networks are removed from ${s.hostName}. Volumes and the stack definition are kept, so Deploy brings it back.`,
          confirm: 'Take down',
        },
        pull: { title: `Pull and redeploy ${s.name}?`, body: 'Pulls newer versions of its images, then recreates any container whose image changed.', confirm: 'Pull & redeploy' },
      }
      const d = ask[action]
      if (d && !(await confirm({ ...d, danger: action !== 'pull' }))) return
      m.mutate({ s, action })
    },
    [confirm, m],
  )
  return { run, pending: m.isPending ? m.variables : undefined }
}

type JobMeta = { id: string; stackId: string; stack: string; host: string; action: string; actor: string; startedAt: string }

const MAX_CHARS = 1_000_000

export function JobPanel({ jobId, onClose }: { jobId: string; onClose: () => void }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [meta, setMeta] = useState<JobMeta | null>(null)
  const [text, setText] = useState('')
  const [state, setState] = useState<'connecting' | 'running' | 'ok' | 'failed' | 'gone'>('connecting')
  const [error, setError] = useState('')
  const panel = useRef<HTMLDivElement>(null)
  // Escape closes the drawer, unless focus is in a field (or a dialog sits above it).
  useLayer(true, panel, () => (document.activeElement?.tagName === 'INPUT' ? false : onClose()))
  const [follow, setFollow] = useState(true)
  const pre = useRef<HTMLPreElement>(null)

  useEffect(() => {
    setText('')
    setError('')
    setState('connecting')
    const es = new EventSource(`/api/jobs/${jobId}`)
    let m: JobMeta | null = null
    es.addEventListener('job', (e) => {
      m = JSON.parse((e as MessageEvent).data) as JobMeta
      setMeta(m)
      setState('running')
    })
    es.addEventListener('output', (e) => {
      const chunk = JSON.parse((e as MessageEvent).data) as string
      setText((t) => {
        const next = t + chunk
        return next.length > MAX_CHARS ? next.slice(next.length - MAX_CHARS) : next
      })
    })
    es.addEventListener('done', (e) => {
      const d = JSON.parse((e as MessageEvent).data) as { ok: boolean; error: string }
      es.close()
      setState(d.ok ? 'ok' : 'failed')
      setError(d.error)
      qc.invalidateQueries({ queryKey: ['stacks'] })
      qc.invalidateQueries({ queryKey: ['container-moves'] })
      if (m) {
        qc.invalidateQueries({ queryKey: ['stack', m.stackId] })
        const what = `${jobTitle[m.action] ?? m.action} ${m.stack}`
        toast(d.ok ? 'good' : 'bad', d.ok ? `${what}: done` : `${what} failed: ${d.error}${isRegistryAuthError(d.error) ? `. ${registryHint}` : ''}`)
      }
    })
    es.onerror = () => {
      // 404: the job finished more than an hour ago and was dropped.
      if (es.readyState === EventSource.CLOSED) setState((s) => (s === 'connecting' ? 'gone' : s))
    }
    return () => es.close()
  }, [jobId, qc, toast])

  useEffect(() => {
    if (follow && pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [text, follow])

  const title = meta ? `${jobTitle[meta.action] ?? meta.action} ${meta.stack}` : 'Stack operation'
  const icon =
    state === 'ok' ? (
      <CircleCheck className="size-5 text-emerald-400" />
    ) : state === 'failed' ? (
      <CircleX className="size-5 text-rose-400" />
    ) : (
      <LoaderCircle className="size-5 animate-spin text-lime-300" />
    )

  return (
    <div ref={panel} className="fixed inset-x-0 bottom-0 z-[45] mx-auto flex h-[46vh] max-w-[1500px] flex-col rounded-t-md border-t-2 border-lime-400 bg-panel shadow-[0_-20px_60px_-10px_rgba(0,0,0,0.8)] ring-1 ring-white/15 animate-[drawer_200ms_ease-out]">
      <div className="flex flex-wrap items-center gap-3 border-b border-white/[0.06] px-5 py-3">
        {state === 'gone' ? <CircleX className="size-5 text-ink-100/40" /> : icon}
        <div className="min-w-0">
          <div className="truncate font-display text-base font-bold text-white">{title}</div>
          <div className="text-xs text-ink-100/50">
            {meta ? `${meta.host} · by ${meta.actor} · ` : ''}
            {{ connecting: 'connecting', running: 'running', ok: 'finished', failed: 'failed', gone: 'output expired' }[state]}
          </div>
        </div>
        <div className="ml-auto flex items-center gap-0.5">
          <IconButton icon={ArrowDownToLine} label={follow ? 'Auto-scroll on' : 'Auto-scroll off'} className={cx(follow && 'bg-lime-500/15 text-lime-300')} onClick={() => setFollow((f) => !f)} />
          <IconButton icon={Download} label="Download output" onClick={() => download(`${meta?.stack ?? 'stack'}-${meta?.action ?? 'job'}.log`, text)} />
          <IconButton icon={X} label="Close (keeps running)" onClick={onClose} />
        </div>
      </div>
      <pre
        ref={pre}
        onScroll={(e) => {
          const el = e.currentTarget
          if (el.scrollHeight - el.scrollTop - el.clientHeight >= 24 && follow) setFollow(false)
        }}
        className="mono flex-1 overflow-auto whitespace-pre-wrap break-all px-5 py-3 text-[12px] leading-relaxed text-ink-100/80"
      >
        {text || (state === 'gone' ? 'This operation finished a while ago and its output is no longer kept.' : 'Waiting for output…')}
        {state === 'failed' && error && <span className="mt-2 block font-semibold text-rose-300">✗ {error}</span>}
        {state === 'ok' && <span className="mt-2 block font-semibold text-emerald-300">✓ Done</span>}
      </pre>
      {state === 'failed' && (
        <div className="px-5 pb-3">
          {/* compose's own output says why a pull failed; the final error is just its exit code */}
          <RegistryAuthHint error={`${error}\n${text.slice(-4000)}`} />
        </div>
      )}
    </div>
  )
}

// ---- a plain-text editor for compose files and .env ----

export function CodeEditor({
  value,
  onChange,
  readOnly,
  label,
  rows = 22,
  placeholder,
}: {
  value: string
  onChange?: (v: string) => void
  readOnly?: boolean
  label: string
  rows?: number
  placeholder?: string
}) {
  const gutter = useRef<HTMLDivElement>(null)
  const lines = Math.max(value.split('\n').length, 1)
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key !== 'Tab' || readOnly || e.metaKey || e.ctrlKey || e.altKey) return
    // YAML is indented with spaces: Tab inserts two. (Esc then Tab leaves the field.)
    e.preventDefault()
    const el = e.currentTarget
    const { selectionStart: a, selectionEnd: b } = el
    const next = value.slice(0, a) + '  ' + value.slice(b)
    onChange?.(next)
    requestAnimationFrame(() => el.setSelectionRange(a + 2, a + 2))
  }
  return (
    <div className="flex overflow-hidden rounded-[3px] bg-black/40 ring-1 ring-inset ring-white/10 focus-within:ring-lime-500/60">
      <div ref={gutter} aria-hidden className="mono select-none overflow-hidden border-r border-white/[0.06] px-2 py-3 text-right text-[12px] leading-[1.6] text-ink-100/25">
        {Array.from({ length: lines }, (_, i) => (
          <div key={i}>{i + 1}</div>
        ))}
      </div>
      <textarea
        aria-label={label}
        value={value}
        readOnly={readOnly}
        placeholder={placeholder}
        onChange={(e) => onChange?.(e.target.value)}
        onKeyDown={onKeyDown}
        onScroll={(e) => {
          if (gutter.current) gutter.current.scrollTop = e.currentTarget.scrollTop
        }}
        rows={rows}
        spellCheck={false}
        autoCapitalize="off"
        autoComplete="off"
        autoCorrect="off"
        wrap="off"
        className="mono min-w-0 flex-1 resize-y bg-transparent px-3 py-3 text-[12px] leading-[1.6] text-ink-50 outline-none placeholder:text-ink-100/25"
      />
    </div>
  )
}

/**
 * "stack <name>" for a container that belongs to a compose project. Links to
 * the stack's page when TainerHub manages it, else to the Stacks list.
 */
export function StackLink({ c, onNavigate, className }: { c: Container; onNavigate?: () => void; className?: string }) {
  const stacks = useStacks()
  if (!c.project) return null
  const s = stacks.data?.find((x) => x.hostId === c.hostId && x.name === c.project)
  const inner = (
    <>
      <Boxes className="size-3 shrink-0 text-cyan-300" />
      <span className="shrink-0 text-ink-100/40">stack</span>
      <span className="truncate">{c.project}</span>
    </>
  )
  const cls = cx('inline-flex min-w-0 items-center gap-1 hover:text-cyan-200 hover:underline', className)
  return s?.managed && s.id ? (
    <Link to="/stacks/$stackId" params={{ stackId: s.id }} onClick={onNavigate} className={cls} title={`Part of the ${c.project} stack, managed by TainerHub. Open it.`}>
      {inner}
    </Link>
  ) : (
    <Link
      to="/stacks"
      search={{ q: c.project }}
      onClick={onNavigate}
      className={cls}
      title={`Part of the ${c.project} compose project, started outside TainerHub (with docker compose). See it under Stacks.`}
    >
      {inner}
    </Link>
  )
}
