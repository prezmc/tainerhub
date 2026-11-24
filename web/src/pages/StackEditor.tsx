import { useEffect, useMemo, useState } from 'react'
import { getRouteApi, Link, useBlocker, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowDownToLine, ArrowLeft, Download, FileText, Lock, Play, RefreshCw, RotateCw, Save, ScrollText, Square, Trash2, Undo2 } from 'lucide-react'
import { api, ApiError, can, errorText, stackProblem, timeAgo, type StackAction, type StackDetail, type StackSaveResult, type StackSummary } from '../api'
import { Badge, Button, Card, ErrorNote, IconButton, Input, Modal, PageHeader, StateBadge, cx, useConfirm, useToast } from '../ui'
import { useFleet, useOpenDetails, useOpenLogs } from '../fleet'
import { CodeEditor, StackStatusBadge, useOpenJob, useStackAction, useStacks, withLive } from '../stacks'
import { useMe } from './AppLayout'
import { HostSelect, useDefaultHost } from '../placement'
import { MoveSection } from './MoveStack'
import { SourceNote, TakeoverPanel, takeoverReady, useTakeoverCheck, useTakeoverSource } from '../takeover'

const STARTER = `services:
  app:
    image: nginx:alpine
    restart: unless-stopped
    ports:
      - "8090:80"
    volumes:
      - ./html:/usr/share/nginx/html:ro
`

const newStackApi = getRouteApi('/app/stacks/new')

const NAME_HINT = "Lowercase letters, digits, '-' and '_'. This is the compose project name and the stack's folder on the host."

// Set right before navigating away on purpose (after saving or deleting), so the
// unsaved-changes prompt doesn't ask about edits that no longer matter.
let leavingOnPurpose = false
const leaveWithoutPrompt = () => {
  leavingOnPurpose = true
  setTimeout(() => (leavingOnPurpose = false), 0)
}

/** Asks before leaving the page (in-app or closing the tab) with unsaved edits. */
function useUnsavedWarning(dirty: boolean) {
  const confirm = useConfirm()
  useBlocker({
    disabled: !dirty,
    enableBeforeUnload: () => dirty && !leavingOnPurpose,
    shouldBlockFn: async ({ current, next }) => {
      if (leavingOnPurpose || current.pathname === next.pathname) return false
      return !(await confirm({ title: 'Discard unsaved changes?', body: "Your edits to this stack haven't been saved.", confirm: 'Discard', danger: true }))
    },
  })
}

function FilesHelp({ name }: { name: string }) {
  return (
    <p className="text-xs leading-relaxed text-ink-100/50">
      Relative paths such as <code className="mono text-lime-300">./data</code> live in{' '}
      <code className="mono text-lime-300">/opt/tainerhub/stacks/{name || '<name>'}/</code> on the host. Variables like{' '}
      <code className="mono text-lime-300">{'${DB_PASSWORD}'}</code> are filled in from .env, which is stored on the hub and written next to the compose file.
    </p>
  )
}

// ---------------- new stack ----------------

export function NewStackPage() {
  const search = newStackApi.useSearch()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const toast = useToast()
  const openJob = useOpenJob()
  const { hosts } = useFleet()
  const stacks = useStacks()
  const adopting = !!search.name
  const [hostId, setHostId] = useState(search.host ?? '')
  const [name, setName] = useState(search.name ?? '')
  const [compose, setCompose] = useState(adopting ? '' : STARTER)
  const [env, setEnv] = useState('')
  const [error, setError] = useState<string | null>(null)
  useUnsavedWarning(compose !== STARTER && compose !== '')

  useDefaultHost(hostId, setHostId)

  const host = hosts.find((h) => h.id === hostId)
  const problem = stackProblem(host)
  const existing = stacks.data?.find((s) => s.hostId === hostId && s.name === name)
  const validName = /^[a-z0-9][a-z0-9_-]{0,62}$/.test(name)
  const takingOver = adopting && !!existing && !existing.managed
  const source = useTakeoverSource(hostId, name, takingOver)
  // Fill the editor from the project once, unless something was typed already.
  useEffect(() => {
    if (source.data && compose === '') {
      setCompose(source.data.compose)
      setEnv(source.data.env)
    }
  }, [source.data]) // eslint-disable-line react-hooks/exhaustive-deps
  const check = useTakeoverCheck({ hostId, name, compose, env }, takingOver)
  const [acked, setAcked] = useState('')
  const deployReady = !takingOver || takeoverReady(check.data, acked)

  const save = useMutation({
    mutationFn: (deploy: boolean) => api<StackSaveResult>('/api/stacks', { method: 'POST', body: { hostId, name, compose, env, deploy, adopt: adopting, ack: acked } }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['stacks'] })
      if (res.deployError) toast('bad', `Saved, but the deploy didn't start: ${res.deployError}`)
      else toast('good', `${res.stack.name} saved`)
      if (res.jobId) openJob(res.jobId)
      leaveWithoutPrompt()
      navigate({ to: '/stacks/$stackId', params: { stackId: res.stack.id } })
    },
    onError: (e) => setError(errorText(e)),
  })

  return (
    <div>
      <BackLink />
      <PageHeader title={adopting ? `Manage ${search.name}` : 'New stack'} subtitle="A compose file that TainerHub deploys to one host." />
      <form
        className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_340px]"
        onSubmit={(e) => {
          e.preventDefault()
          setError(null)
          const deploy = (e.nativeEvent as SubmitEvent).submitter?.getAttribute('value') === 'deploy'
          save.mutate(deploy)
        }}
      >
        <div className="space-y-4">
          <Card className="space-y-3 p-5">
            <div className="flex items-center justify-between">
              <h2 className="font-display text-lg font-bold text-white">compose.yaml</h2>
              <span className="text-xs text-ink-100/40">{compose.length.toLocaleString()} bytes</span>
            </div>
            {takingOver && (
              <SourceNote
                source={source}
                onReload={async () => {
                  const r = await source.refetch()
                  if (r.data) {
                    setCompose(r.data.compose)
                    setEnv(r.data.env)
                  }
                }}
              />
            )}
            <CodeEditor
              label="compose.yaml"
              value={compose}
              onChange={setCompose}
              placeholder={adopting ? (source.isFetching ? `Reading ${search.name}'s compose files from the host…` : `Paste the compose file that ${search.name} was started with.`) : undefined}
            />
          </Card>
          <Card className="space-y-3 p-5">
            <h2 className="font-display text-lg font-bold text-white">.env</h2>
            <CodeEditor label=".env" value={env} onChange={setEnv} rows={6} placeholder="DB_PASSWORD=change-me" />
            <FilesHelp name={name} />
          </Card>
        </div>

        <div className="space-y-4">
          <Card className="space-y-4 p-5">
            <label className="block">
              <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Host</span>
              <HostSelect value={hostId} onChange={setHostId} className="w-full rounded-[3px] bg-white/[0.05] px-3 py-2.5 text-sm text-white ring-1 ring-inset ring-white/10 outline-none focus:ring-lime-500/60" />
            </label>
            {problem && host?.online && <p className="rounded-[3px] bg-amber-500/10 px-3 py-2 text-xs leading-relaxed text-amber-100 ring-1 ring-inset ring-amber-500/20">{problem}</p>}
            <Input
              label="Name"
              value={name}
              onChange={(e) => setName(e.target.value.toLowerCase())}
              placeholder="my-app"
              required
              maxLength={63}
              readOnly={adopting}
              hint={name && !validName ? 'Use lowercase letters, digits, - and _, starting with a letter or digit.' : NAME_HINT}
            />
            {existing && !existing.managed && !adopting && (
              <p className="text-xs text-rose-300">
                A compose project called {name} is already running on {existing.hostName}. Choose another name, or use Manage in TainerHub next to it on the Stacks page to take it over.
              </p>
            )}
            {existing && !existing.managed && adopting && (
              <>
                <p className="rounded-[3px] bg-lime-500/10 px-3 py-2 text-xs leading-relaxed text-lime-100 ring-1 ring-inset ring-lime-500/20">
                  <strong>{name}</strong> is already running on {existing.hostName}, started outside TainerHub. Deploying takes it over: compose recreates its containers from
                  this file. The check below compares it with what runs now.
                </p>
                <TakeoverPanel check={check} acked={acked} setAcked={setAcked} name={name} />
              </>
            )}
            {existing?.managed && <p className="text-xs text-rose-300">This host already has a stack called {name}.</p>}
            <ErrorNote error={error} />
            <div className="flex flex-col gap-2">
              <Button
                variant="primary"
                icon={RefreshCw}
                name="action"
                value="deploy"
                loading={save.isPending && save.variables}
                disabled={!validName || !!problem || !!existing?.managed || (!!existing && !adopting) || save.isPending || !deployReady || check.isFetching}
                title={!deployReady ? 'Deal with the takeover check first' : undefined}
              >
                Save & deploy
              </Button>
              <Button icon={Save} name="action" value="save" loading={save.isPending && !save.variables} disabled={!validName || !hostId || !!existing?.managed || (!!existing && !adopting) || save.isPending}>
                Save without deploying
              </Button>
            </div>
          </Card>
          <p className="px-1 text-xs leading-relaxed text-ink-100/50">
            <Lock className="mr-1 inline size-3" />
            Only admins can create or edit stacks: a compose file can mount the host's disks or run privileged containers. Operators can deploy, start and stop saved stacks.
          </p>
        </div>
      </form>
    </div>
  )
}

function BackLink() {
  return (
    <Link to="/stacks" className="mb-4 inline-flex items-center gap-1.5 text-sm text-ink-100/60 hover:text-white">
      <ArrowLeft className="size-4" />
      Stacks
    </Link>
  )
}

// ---------------- existing stack ----------------

export function StackPage() {
  const { stackId } = getRouteApi('/app/stacks/$stackId').useParams()
  const me = useMe()
  const admin = can(me.user, 'admin')
  const operator = can(me.user, 'operator')
  const detail = useQuery({ queryKey: ['stack', stackId], queryFn: () => api<StackDetail>(`/api/stacks/${stackId}`), retry: (n, e) => !(e instanceof ApiError && e.status === 404) && n < 2 })
  const stacks = useStacks()
  const { containers, hosts } = useFleet()
  const live = stacks.data?.find((s) => s.id === stackId) ?? (detail.data && withLive(detail.data.summary, containers, hosts))

  if (detail.isPending) return <p className="text-sm text-ink-100/60">Loading…</p>
  if (detail.isError)
    return (
      <div>
        <BackLink />
        <ErrorNote error={errorText(detail.error)} />
      </div>
    )
  const d = detail.data
  const s = live ?? d.summary
  // Saved while taking over a running project, not deployed by TainerHub yet:
  // the first deploy goes through the takeover check.
  const takeover = !d.stack.deployedAt && s.containers > 0 && s.online
  return (
    <div>
      <BackLink />
      <PageHeader
        title={d.stack.name}
        subtitle={
          <span className="flex flex-wrap items-center gap-2">
            <span>{s.hostName}</span>
            <StackStatusBadge s={s} />
            {s.pending && d.stack.deployedAt && <Badge tone="warn">changes not deployed</Badge>}
            <span className="text-ink-100/50">{d.stack.deployedAt ? `Deployed ${timeAgo(d.stack.deployedAt)}` : 'Never deployed'}</span>
          </span>
        }
      >
        {operator && <LifecycleButtons s={s} takeover={takeover} />}
      </PageHeader>

      <div className="space-y-6">
        {takeover && admin && <TakeoverDeploy d={d} s={s} />}
        <StackContainers s={s} />
        {admin ? <FilesEditor d={d} /> : <FilesLocked />}
        {admin && s.managed && <MoveSection d={d} s={s} />}
        {admin && <DangerZone s={s} />}
      </div>
    </div>
  )
}

function LifecycleButtons({ s, takeover }: { s: StackSummary; takeover: boolean }) {
  const { run, pending } = useStackAction()
  const openJob = useOpenJob()
  if (s.job) {
    const id = s.job.id
    return (
      <Button icon={ScrollText} onClick={() => openJob(id)}>
        View progress
      </Button>
    )
  }
  const disabled = !s.online || !!pending
  const busy = (a: StackAction) => pending?.action === a
  const btn = (a: StackAction, label: string, icon: typeof Play, variant: 'primary' | 'secondary' | 'ghost' = 'secondary') => (
    <Button key={a} variant={variant} icon={icon} loading={busy(a)} disabled={disabled} onClick={() => run(s, a)} title={!s.online ? `${s.hostName} is offline` : undefined}>
      {label}
    </Button>
  )
  const has = s.containers > 0
  return (
    <div className="flex flex-wrap gap-2">
      {s.running > 0 && btn('restart', 'Restart', RotateCw, 'ghost')}
      {s.running > 0 && btn('stop', 'Stop', Square, 'ghost')}
      {has && s.running < s.containers && btn('start', 'Start', Play, 'ghost')}
      {has && btn('down', 'Down', ArrowDownToLine, 'ghost')}
      {!takeover && btn('pull', 'Pull & redeploy', Download)}
      {!takeover && btn('deploy', 'Deploy', RefreshCw, 'primary')}
    </div>
  )
}

/** First deploy of a taken-over project: check, confirm, deploy. */
function TakeoverDeploy({ d, s }: { d: StackDetail; s: StackSummary }) {
  const qc = useQueryClient()
  const toast = useToast()
  const openJob = useOpenJob()
  const check = useTakeoverCheck({ hostId: s.hostId, name: s.name, compose: d.stack.compose, env: d.stack.env }, true)
  const [acked, setAcked] = useState('')
  const deploy = useMutation({
    mutationFn: () => api<{ jobId: string }>(`/api/stacks/${s.id}/deploy`, { method: 'POST', body: { ack: acked } }),
    onSuccess: (res) => {
      openJob(res.jobId)
      qc.invalidateQueries({ queryKey: ['stacks'] })
      qc.invalidateQueries({ queryKey: ['stack', s.id] })
    },
    onError: (e) => {
      check.refetch()
      toast('bad', errorText(e))
    },
  })
  return (
    <Card className="space-y-4 p-5 ring-lime-400/30">
      <div>
        <h2 className="font-display text-lg font-bold text-white">Take over {s.name}</h2>
        <p className="text-sm text-ink-100/60">
          It still runs as it was started outside TainerHub. Deploying recreates its containers from the saved compose file, run from TainerHub's folder.
        </p>
      </div>
      <TakeoverPanel check={check} acked={acked} setAcked={setAcked} name={s.name} />
      <div className="flex justify-end">
        <Button variant="primary" icon={RefreshCw} loading={deploy.isPending} disabled={!takeoverReady(check.data, acked) || check.isFetching || !!s.job} onClick={() => deploy.mutate()}>
          Deploy and take over
        </Button>
      </div>
    </Card>
  )
}

function StackContainers({ s }: { s: StackSummary }) {
  const { containers } = useFleet()
  const openLogs = useOpenLogs()
  const openDetails = useOpenDetails()
  const mine = containers.filter((c) => c.hostId === s.hostId && c.project === s.name)
  return (
    <Card>
      <div className="flex items-center justify-between px-5 pt-4">
        <h2 className="font-display text-lg font-bold text-white">Containers</h2>
        <span className="text-xs text-ink-100/50">{mine.length > 0 ? `${s.running} of ${mine.length} running` : ''}</span>
      </div>
      {mine.length === 0 ? (
        <p className="px-5 pb-5 pt-2 text-sm text-ink-100/60">
          {!s.online ? `${s.hostName} is offline.` : s.deployedAt ? 'No containers. Deploy to start the stack again.' : 'Not deployed yet.'}
        </p>
      ) : (
        <div className="mt-2 divide-y divide-white/[0.06]">
          {mine.map((c) => (
            <div key={c.id} className="flex flex-wrap items-center gap-3 px-5 py-3">
              <button onClick={() => openDetails(c)} className="min-w-0 flex-1 text-left">
                <div className="truncate text-sm font-semibold text-white hover:underline">{c.name}</div>
                <div className="mono truncate text-xs text-ink-100/50">
                  {c.image}
                  {c.ports && c.ports.length > 0 && ` · ${c.ports.join(', ')}`}
                </div>
              </button>
              <StateBadge state={c.state} status={c.status} />
              <IconButton icon={ScrollText} label={`Logs for ${c.name}`} onClick={() => openLogs(c)} />
            </div>
          ))}
        </div>
      )}
    </Card>
  )
}

function FilesLocked() {
  return (
    <Card className="flex items-start gap-3 p-5 text-sm text-ink-100/70">
      <Lock className="mt-0.5 size-4 shrink-0 text-ink-100/50" />
      Only admins can view this stack's compose file and .env, because they often hold passwords and can grant access to the whole host.
    </Card>
  )
}

function FilesEditor({ d }: { d: StackDetail }) {
  const qc = useQueryClient()
  const toast = useToast()
  const openJob = useOpenJob()
  const [tab, setTab] = useState<'compose' | 'env'>('compose')
  const [compose, setCompose] = useState(d.stack.compose)
  const [env, setEnv] = useState(d.stack.env)
  const [base, setBase] = useState(d.stack)
  const [error, setError] = useState<string | null>(null)
  const dirty = compose !== base.compose || env !== base.env
  useUnsavedWarning(dirty)

  // Pick up changes saved elsewhere, unless the user is in the middle of editing.
  useEffect(() => {
    if (d.stack.updatedAt !== base.updatedAt && !dirty) {
      setBase(d.stack)
      setCompose(d.stack.compose)
      setEnv(d.stack.env)
    }
  }, [d.stack, base.updatedAt, dirty])

  const save = useMutation({
    mutationFn: (deploy: boolean) =>
      api<StackSaveResult>(`/api/stacks/${d.stack.id}`, { method: 'PUT', body: { compose, env, updatedAt: base.updatedAt, deploy } }),
    onSuccess: (res) => {
      setBase(res.stack)
      setError(null)
      qc.setQueryData<StackDetail>(['stack', d.stack.id], (old) => (old ? { ...old, stack: res.stack } : old))
      qc.invalidateQueries({ queryKey: ['stacks'] })
      if (res.deployError) toast('bad', `Saved, but the deploy didn't start: ${res.deployError}`)
      else toast('good', 'Saved')
      if (res.jobId) openJob(res.jobId)
    },
    onError: (e) => setError(errorText(e)),
  })

  const lines = useMemo(() => {
    const a = base.compose.split('\n')
    const b = compose.split('\n')
    let changed = Math.abs(a.length - b.length)
    for (let i = 0; i < Math.min(a.length, b.length); i++) if (a[i] !== b[i]) changed++
    return changed
  }, [base.compose, compose])

  return (
    <Card className="p-5">
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <div className="flex gap-1 rounded-[3px] bg-ink-950/60 p-0.5 ring-1 ring-line">
          {(['compose', 'env'] as const).map((t) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={cx('flex items-center gap-1.5 rounded-[2px] px-3 py-1 text-sm font-medium', tab === t ? 'bg-lime-400 text-ink-950' : 'text-ink-100/60 hover:text-white')}
            >
              <FileText className="size-3.5" />
              {t === 'compose' ? 'compose.yaml' : '.env'}
              {(t === 'compose' ? compose !== base.compose : env !== base.env) && <span className="size-1.5 rounded-full bg-amber-400" />}
            </button>
          ))}
        </div>
        <span className="text-xs text-ink-100/50">
          Last edited by {base.updatedBy || 'unknown'} {timeAgo(base.updatedAt)}
          {dirty && lines > 0 && ` · ${lines} line${lines === 1 ? '' : 's'} changed`}
        </span>
        <div className="ml-auto flex flex-wrap gap-2">
          {dirty && (
            <Button
              variant="ghost"
              icon={Undo2}
              onClick={() => {
                setCompose(base.compose)
                setEnv(base.env)
                setError(null)
              }}
            >
              Discard
            </Button>
          )}
          <Button icon={Save} disabled={!dirty} loading={save.isPending && !save.variables} onClick={() => save.mutate(false)}>
            Save
          </Button>
          <Button variant="primary" icon={RefreshCw} disabled={!dirty} loading={save.isPending && save.variables} onClick={() => save.mutate(true)}>
            Save & deploy
          </Button>
        </div>
      </div>
      <ErrorNote error={error} />
      {tab === 'compose' ? (
        <CodeEditor label="compose.yaml" value={compose} onChange={setCompose} />
      ) : (
        <CodeEditor label=".env" value={env} onChange={setEnv} rows={10} placeholder="DB_PASSWORD=change-me" />
      )}
      <div className="mt-3">
        <FilesHelp name={d.stack.name} />
      </div>
    </Card>
  )
}

function DangerZone({ s }: { s: StackSummary }) {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const toast = useToast()
  const openJob = useOpenJob()
  const [open, setOpen] = useState(false)
  const [volumes, setVolumes] = useState(false)
  const [confirmName, setConfirmName] = useState('')
  const del = useMutation({
    mutationFn: (mode: 'remove' | 'forget') =>
      api<{ jobId?: string } | undefined>(`/api/stacks/${s.id}?${mode === 'forget' ? 'forget=1' : volumes ? 'volumes=1' : ''}`, { method: 'DELETE' }),
    onSuccess: (res, mode) => {
      setOpen(false)
      qc.invalidateQueries({ queryKey: ['stacks'] })
      if (res?.jobId) openJob(res.jobId)
      else toast('good', mode === 'forget' ? `${s.name} removed from TainerHub` : `${s.name} deleted`)
      leaveWithoutPrompt()
      navigate({ to: '/stacks' })
    },
    onError: (e) => toast('bad', errorText(e)),
  })
  return (
    <Card className="flex flex-wrap items-center gap-4 p-5 ring-rose-500/20">
      <div className="min-w-0 flex-1">
        <h2 className="font-display text-lg font-bold text-white">Delete stack</h2>
        <p className="text-sm text-ink-100/60">Removes its containers and networks from {s.hostName}, then the stack itself.</p>
      </div>
      <Button variant="dangerGhost" icon={Trash2} onClick={() => setOpen(true)}>
        Delete…
      </Button>
      <Modal open={open} onClose={() => setOpen(false)} title={`Delete ${s.name}?`}>
        <div className="space-y-4 text-sm text-ink-100/80">
          <p>
            Runs <code className="mono text-lime-300">docker compose down</code> on {s.hostName}, deletes the compose file and .env there, and removes the stack from TainerHub. Files
            the stack wrote into its folder (bind-mounted data) are left on the host.
          </p>
          <label className="flex items-start gap-2.5 rounded-[3px] bg-rose-500/[0.07] p-3 ring-1 ring-inset ring-rose-500/20">
            <input type="checkbox" checked={volumes} onChange={(e) => setVolumes(e.target.checked)} className="mt-0.5 accent-rose-500" />
            <span>
              Also delete its named volumes. <strong className="text-rose-200">Their data is gone for good.</strong>
            </span>
          </label>
          <Input label={`Type ${s.name} to confirm`} value={confirmName} onChange={(e) => setConfirmName(e.target.value)} autoFocus />
          {!s.online && <p className="text-xs text-amber-200">{s.hostName} is offline, so only “Remove from TainerHub” is possible. Its containers keep running.</p>}
          <div className="flex flex-wrap justify-end gap-2">
            <Button variant="ghost" loading={del.isPending && del.variables === 'forget'} disabled={confirmName !== s.name || del.isPending} onClick={() => del.mutate('forget')}>
              Remove from TainerHub only
            </Button>
            <Button
              variant="danger"
              icon={Trash2}
              loading={del.isPending && del.variables === 'remove'}
              disabled={confirmName !== s.name || !s.online || del.isPending || !!s.job}
              onClick={() => del.mutate('remove')}
            >
              Delete stack
            </Button>
          </div>
        </div>
      </Modal>
    </Card>
  )
}
