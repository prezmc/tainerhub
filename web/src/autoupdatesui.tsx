import { useEffect, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CalendarClock, Play, Save, Timer } from 'lucide-react'
import { api, errorText, needsReauth, timeAgo } from './api'
import {
  containerAutoProblem,
  daysText,
  findTarget,
  resultLabel,
  resultText,
  resultTone,
  WEEK,
  windowText,
  type AutoOutcome,
  type AutoSettings,
  type AutoUpdates,
  type AutoUpdateTarget,
  type AutoWindow,
} from './autoupdates'
import { Badge, Button, Card, ErrorNote, Input, cx, useToast } from './ui'
import { useOpenJob } from './stacks'
import { hubAdmin } from './hostscope'
import { useMe } from './pages/AppLayout'

/** The automatic update window lives in Settings, which only admins with every host can open. */
function SettingsLink({ children, className }: { children: string; className: string }) {
  const me = useMe()
  if (!hubAdmin(me?.user)) return null
  return (
    <Link to="/settings" hash="auto-updates" className={className}>
      {children}
    </Link>
  )
}

/** Opted-in targets, the window and the last run (admins only). Polls quickly while a run is going. */
export function useAutoUpdates(enabled = true) {
  return useQuery({
    queryKey: ['auto-updates'],
    queryFn: () => api<AutoUpdates>('/api/auto-updates'),
    enabled,
    refetchInterval: (q) => (q.state.data?.run?.running ? 3000 : 60000),
  })
}

function useToggle() {
  const qc = useQueryClient()
  const toast = useToast()
  return useMutation({
    mutationFn: ({ path, enabled }: { path: string; enabled: boolean; label: string }) => api<{ enabled: boolean }>(path, { method: 'PUT', body: { enabled } }),
    onSuccess: (r, v) => {
      toast('good', r.enabled ? `Automatic updates on for ${v.label}` : `Automatic updates off for ${v.label}`)
      qc.invalidateQueries({ queryKey: ['auto-updates'] })
      qc.invalidateQueries({ queryKey: ['stacks'] })
    },
    // A stale sign-in already opened the "Sign in again" dialog.
    onError: (e) => !needsReauth(e) && toast('bad', errorText(e)),
  })
}

function Switch({ on, busy, disabled, onChange, label }: { on: boolean; busy?: boolean; disabled?: boolean; onChange: (on: boolean) => void; label: string }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      aria-label={label}
      disabled={disabled || busy}
      onClick={() => onChange(!on)}
      className={cx(
        'relative inline-flex h-5 w-9 shrink-0 items-center rounded-full ring-1 ring-inset transition disabled:opacity-45 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-lime-400',
        on ? 'bg-lime-400/80 ring-lime-300' : 'bg-white/[0.06] ring-white/15',
        busy && 'animate-pulse',
      )}
    >
      <span className={cx('inline-block size-3.5 rounded-full bg-white transition', on ? 'translate-x-[18px]' : 'translate-x-[3px]')} />
    </button>
  )
}

function LastResult({ t }: { t: AutoUpdateTarget }) {
  if (!t.lastResult) return <span className="text-ink-100/50">Not run yet.</span>
  return (
    <span className="flex flex-wrap items-center gap-2">
      <Badge tone={resultTone(t.lastResult)}>{resultLabel(t.lastResult)}</Badge>
      <span className={t.lastResult === 'failed' ? 'text-rose-200' : t.lastResult === 'rolled_back' ? 'text-amber-200' : 'text-ink-100/60'}>
        {t.lastError || (t.lastResult === 'ok' ? 'to the newest image' : '')}
        {t.lastRunAt && <span className="text-ink-100/40"> · {timeAgo(t.lastRunAt)}</span>}
      </span>
    </span>
  )
}

function WindowNote({ w }: { w: AutoWindow }) {
  const me = useMe()
  if (!w.settings.enabled)
    return (
      <span className="text-amber-200">
        Paused for everything{hubAdmin(me?.user) ? ' under ' : ' by an admin.'}
        <SettingsLink className="underline underline-offset-2">Settings</SettingsLink>
        {hubAdmin(me?.user) && '.'}
      </span>
    )
  return (
    <span>
      Next window {w.nextText ?? 'not scheduled'} ({daysText(w.settings.days)}).{' '}
      <SettingsLink className="text-lime-300 underline-offset-2 hover:underline">Change</SettingsLink>
    </span>
  )
}

/**
 * The opt-in for one stack or standalone container: on the stack page and
 * in the container details panel (admins).
 */
export function AutoUpdateToggle({ stackId, hostId, name, problem, compact }: { stackId?: string; hostId: string; name: string; problem?: string | null; compact?: boolean }) {
  const q = useAutoUpdates()
  const toggle = useToggle()
  const target = findTarget(q.data?.targets, stackId ? { stackId } : { hostId, name })
  const on = !!target
  const path = stackId ? `/api/stacks/${stackId}/auto-update` : `/api/hosts/${hostId}/containers/${encodeURIComponent(name)}/auto-update`
  const body = (
    <div className="flex items-start gap-3">
      <Switch
        on={on}
        busy={toggle.isPending}
        disabled={!q.data || (!on && !!problem)}
        label={`Automatic updates for ${name}`}
        onChange={(enabled) => toggle.mutate({ path, enabled, label: name })}
      />
      <div className="min-w-0 flex-1 space-y-1 text-sm">
        <div className="font-semibold text-white">Automatic updates</div>
        {problem && !on ? (
          <p className="text-ink-100/60">{problem}</p>
        ) : (
          <p className="text-ink-100/60">
            {on
              ? stackId
                ? 'In the maintenance window, when a newer image is out, TainerHub runs Pull & redeploy and checks the containers come up.'
                : 'In the maintenance window, when a newer image is out, TainerHub recreates the container on it (the old one is put back if the new one fails to start).'
              : 'Off: updates only happen when someone starts them.'}{' '}
            {q.data && <WindowNote w={q.data.window} />}
          </p>
        )}
        {target && stackId && <AutoRollbackSetting path={path} on={target.autoRollback} label={name} />}
        {target && (
          <div className="text-xs">
            <LastResult t={target} />
          </div>
        )}
        {q.error && <ErrorNote error={errorText(q.error)} />}
      </div>
    </div>
  )
  if (compact) return <div className="rounded-[3px] px-3 py-3 ring-1 ring-line">{body}</div>
  return <Card className="p-5">{body}</Card>
}

/** "Roll back automatically if the update fails" for an opted-in stack (on by default). */
function AutoRollbackSetting({ path, on, label }: { path: string; on: boolean; label: string }) {
  const qc = useQueryClient()
  const toast = useToast()
  const m = useMutation({
    mutationFn: (autoRollback: boolean) => api<{ enabled: boolean; autoRollback: boolean }>(path, { method: 'PUT', body: { enabled: true, autoRollback } }),
    onSuccess: (r) => {
      toast('good', r.autoRollback ? `${label}: a failed update is rolled back automatically` : `${label}: a failed update is left as it is`)
      qc.invalidateQueries({ queryKey: ['auto-updates'] })
    },
    onError: (e) => !needsReauth(e) && toast('bad', errorText(e)),
  })
  return (
    <label className="flex items-start gap-2 pt-1 text-sm">
      <input type="checkbox" className="mt-0.5 accent-lime-400" checked={on} disabled={m.isPending} onChange={(e) => m.mutate(e.target.checked)} />
      <span>
        <span className="text-white">Roll back automatically if the update fails</span>
        <span className="block text-xs text-ink-100/55">
          When the redeploy fails or the containers don't come up healthy, TainerHub puts the previous images back right away (see Roll back above).
        </span>
      </span>
    </label>
  )
}

/** Container details: the toggle for standalone containers, a pointer to the stack otherwise. */
export function ContainerAutoUpdate({ c }: { c: { name: string; project?: string; image: string; hostId: string } }) {
  const problem = containerAutoProblem(c)
  if (c.project || c.name.startsWith('tainerhub')) return null // the stack's page has its own switch
  return <AutoUpdateToggle hostId={c.hostId} name={c.name} problem={problem} compact />
}

/** One line for the Hosts page's update card. */
export function AutoUpdatesSummary() {
  const q = useAutoUpdates()
  if (!q.data) return null
  const { targets, window: w, run } = q.data
  return (
    <p className="mt-3 flex flex-wrap items-center gap-x-2 text-xs text-ink-100/60">
      <CalendarClock className="size-3.5 text-lime-300" />
      <span>
        Automatic updates: {targets.length === 0 ? 'nothing opted in' : `${targets.length} ${targets.length === 1 ? 'target' : 'targets'}`}
        {targets.length > 0 && (w.settings.enabled ? `, next run ${w.nextText ?? '–'}` : ', paused')}
        {run?.running && ', running now'}
      </span>
      <SettingsLink className="text-lime-300 underline-offset-2 hover:underline">Settings</SettingsLink>
    </p>
  )
}

const durations = [30, 60, 90, 120, 180, 240, 360, 480]

/** Settings → Automatic updates: the window, the global pause switch, Run now and the latest outcomes. */
export function AutoUpdatesCard() {
  const qc = useQueryClient()
  const toast = useToast()
  const q = useAutoUpdates()
  const [draft, setDraft] = useState<AutoSettings | null>(null)
  const [error, setError] = useState<string | null>(null)
  const saved = q.data?.window.settings
  const form = draft ?? saved
  const browserZone = Intl.DateTimeFormat().resolvedOptions().timeZone

  const save = useMutation({
    mutationFn: (s: AutoSettings) => api<AutoWindow>('/api/auto-updates/settings', { method: 'PUT', body: s }),
    onSuccess: (w) => {
      toast('good', w.settings.enabled ? `Saved: ${windowText(w)}` : 'Saved: automatic updates are paused')
      setDraft(null)
      setError(null)
      qc.invalidateQueries({ queryKey: ['auto-updates'] })
    },
    onError: (e) => setError(needsReauth(e) ? 'Sign in again, then save once more.' : errorText(e)),
  })
  const runNow = useMutation({
    mutationFn: () => api<{ runId: string }>('/api/auto-updates/run', { method: 'POST', body: {} }),
    onSuccess: () => {
      toast('good', 'Automatic updates are running now')
      qc.invalidateQueries({ queryKey: ['auto-updates'] })
    },
    onError: (e) => !needsReauth(e) && toast('bad', errorText(e)),
  })

  const set = (patch: Partial<AutoSettings>) => form && setDraft({ ...form, ...patch })
  const run = q.data?.run
  const targets = q.data?.targets ?? []
  const loaded = !!q.data
  useEffect(() => {
    // Links from the stack page and the Hosts page land here.
    if (loaded && window.location.hash === '#auto-updates') document.getElementById('auto-updates')?.scrollIntoView({ block: 'start' })
  }, [loaded])

  return (
    <Card className="px-5 py-4">
      <div id="auto-updates" className="mb-3 flex flex-wrap items-center gap-3 scroll-mt-24">
        <CalendarClock className="size-5 text-lime-300" />
        <div className="min-w-0 flex-1">
          <h2 className="font-display text-lg font-semibold text-white">Automatic updates</h2>
          <p className="text-sm text-ink-100/60">
            Stacks and standalone containers you opt in (on the stack's page or in a container's details) are updated in this window when a newer image is out, one
            at a time per host. TainerHub's own containers never are.
          </p>
        </div>
        <Button icon={Play} loading={runNow.isPending} disabled={!!run?.running || targets.length === 0} onClick={() => runNow.mutate()} title="Check and update every opted-in target now">
          {run?.running ? 'Running…' : 'Run now'}
        </Button>
      </div>
      <ErrorNote error={q.error ? errorText(q.error) : null} />
      {form && q.data && (
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            setError(null)
            save.mutate(form)
          }}
        >
          <label className="flex items-center gap-3 text-sm">
            <Switch on={form.enabled} onChange={(enabled) => set({ enabled })} label="Run automatic updates in the window" />
            <span className="text-white">{form.enabled ? 'On: runs in the window below' : 'Paused: no scheduled runs (Run now still works)'}</span>
          </label>
          <div>
            <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Days</span>
            <div className="flex flex-wrap gap-1.5">
              {WEEK.map((d) => {
                const active = form.days.includes(d)
                return (
                  <button
                    key={d}
                    type="button"
                    aria-pressed={active}
                    onClick={() => set({ days: active ? form.days.filter((x) => x !== d) : [...form.days, d] })}
                    className={cx(
                      'rounded-[2px] px-2.5 py-1 font-mono text-xs uppercase ring-1 ring-inset transition',
                      active ? 'bg-lime-400/15 text-lime-200 ring-lime-400/40' : 'text-ink-100/50 ring-white/12 hover:text-white',
                    )}
                  >
                    {d}
                  </button>
                )
              })}
            </div>
          </div>
          <div className="grid gap-4 sm:grid-cols-3">
            <Input label="Start time" type="time" value={form.time} onChange={(e) => set({ time: e.target.value })} required />
            <Input
              label="Time zone"
              value={form.timeZone}
              placeholder={q.data.window.zone}
              onChange={(e) => set({ timeZone: e.target.value })}
              hint={form.timeZone ? undefined : "Empty: the hub's local time"}
            />
            <label className="block">
              <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Stop starting updates after</span>
              <select
                value={form.maxMinutes}
                onChange={(e) => set({ maxMinutes: Number(e.target.value) })}
                className="w-full rounded-[2px] bg-ink-950/70 px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60"
              >
                {[...new Set([...durations, form.maxMinutes])]
                  .sort((a, b) => a - b)
                  .map((m) => (
                    <option key={m} value={m}>
                      {m % 60 === 0 ? `${m / 60} hour${m === 60 ? '' : 's'}` : `${m} minutes`}
                    </option>
                  ))}
              </select>
            </label>
          </div>
          {browserZone && form.timeZone !== browserZone && (
            <button type="button" className="text-xs text-lime-300 hover:text-lime-200" onClick={() => set({ timeZone: browserZone })}>
              Use my time zone ({browserZone})
            </button>
          )}
          <ErrorNote error={error} />
          <div className="flex flex-wrap items-center gap-3">
            <Button variant="primary" icon={Save} loading={save.isPending} disabled={!draft}>
              Save
            </Button>
            {draft && (
              <Button type="button" variant="ghost" onClick={() => setDraft(null)}>
                Discard
              </Button>
            )}
            <span className="text-sm text-ink-100/55">
              <Timer className="mr-1 inline size-3.5" />
              {saved?.enabled ? <>Next window {q.data.window.nextText ?? '–'}</> : 'Paused'}
            </span>
          </div>
        </form>
      )}

      <div className="mt-5 border-t border-line pt-4">
        <div className="mb-2 flex flex-wrap items-baseline gap-2">
          <h3 className="font-semibold text-white">Opted in</h3>
          <span className="text-xs text-ink-100/50">{targets.length === 0 ? 'nothing yet' : `${targets.length} ${targets.length === 1 ? 'target' : 'targets'}`}</span>
        </div>
        {targets.length > 0 && (
          <ul className="divide-y divide-line rounded-[3px] ring-1 ring-line">
            {targets.map((t) => (
              <li key={t.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm">
                <TargetName kind={t.kind} name={t.kind === 'stack' ? t.stackName ?? '' : t.containerName ?? ''} stackId={t.stackId} />
                <span className="text-xs text-ink-100/50">{t.hostName}</span>
                <span className="ml-auto text-xs">
                  <LastResult t={t} />
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>

      <RecentRun outcomes={run?.outcomes} running={!!run?.running} startedAt={run?.startedAt} trigger={run?.trigger} error={run?.error} />
    </Card>
  )
}

function RollbackOutput({ jobId }: { jobId: string }) {
  const openJob = useOpenJob()
  return (
    <button type="button" className="ml-2 text-lime-300 underline-offset-2 hover:underline" onClick={() => openJob(jobId)}>
      rollback output
    </button>
  )
}

function TargetName({ kind, name, stackId }: { kind: 'stack' | 'container'; name: string; stackId?: string }) {
  return (
    <span className="flex min-w-0 items-center gap-2">
      <Badge tone={kind === 'stack' ? 'accent' : 'idle'}>{kind}</Badge>
      {stackId ? (
        <Link to="/stacks/$stackId" params={{ stackId }} className="truncate font-mono text-[13px] text-white hover:underline">
          {name}
        </Link>
      ) : (
        <span className="truncate font-mono text-[13px] text-white">{name}</span>
      )}
    </span>
  )
}

function RecentRun({ outcomes, running, startedAt, trigger, error }: { outcomes?: AutoOutcome[]; running: boolean; startedAt?: string; trigger?: string; error?: string }) {
  return (
    <div className="mt-5 border-t border-line pt-4">
      <div className="mb-2 flex flex-wrap items-baseline gap-2">
        <h3 className="font-semibold text-white">Recent automatic updates</h3>
        {startedAt && (
          <span className="text-xs text-ink-100/50">
            {running ? 'running since' : 'last run'} {timeAgo(startedAt)} ({trigger === 'manual' ? 'Run now' : 'scheduled'})
          </span>
        )}
      </div>
      <ErrorNote error={error} />
      {!outcomes ? (
        <p className="text-sm text-ink-100/50">No run since the hub started. Each target's last result is listed above; every update is also in the Audit log.</p>
      ) : outcomes.length === 0 ? (
        <p className="text-sm text-ink-100/50">{running ? 'Checking hosts for newer images…' : 'Nothing was opted in.'}</p>
      ) : (
        <ul className="divide-y divide-line rounded-[3px] ring-1 ring-line">
          {outcomes.map((o) => (
            <li key={o.targetId} className="px-3 py-2 text-sm">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <TargetName kind={o.kind} name={o.name} stackId={o.stackId} />
                <span className="text-xs text-ink-100/50">{o.host}</span>
                <Badge tone={resultTone(o.result)} className="ml-auto">
                  {resultLabel(o.result)}
                </Badge>
                <span className="text-xs text-ink-100/40">{timeAgo(o.at)}</span>
              </div>
              {(o.reason || o.images?.length) && (
                <div className={cx('mt-0.5 text-xs', o.result === 'failed' ? 'text-rose-200' : o.result === 'rolled_back' ? 'text-amber-200' : 'text-ink-100/55')}>
                  {o.result === 'skipped' ? resultText(o.result, o.reason) : o.reason}
                  {o.images && o.images.length > 0 && <span className="font-mono text-ink-100/50"> {o.images.join(', ')}</span>}
                  {o.rollbackJobId && <RollbackOutput jobId={o.rollbackJobId} />}
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
