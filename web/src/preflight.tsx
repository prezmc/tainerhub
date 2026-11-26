// The check before a new stack's first deploy (POST /api/stacks/preflight):
// the compose file resolved on the chosen host and compared with it. Shown on
// New container, where a finding's suggested fix can be applied with a click.
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, Ban, CircleCheck, LoaderCircle, RefreshCw, Wand2 } from 'lucide-react'
import { api, errorText } from './api'
import { Button, cx } from './ui'
import { useDebounced } from './takeover'

export type PreflightFix = {
  label: string
  kind: 'port' | 'name' | 'cpus' | 'memory' | 'gpu-off' | 'network-create' | 'network-drop' | 'tag'
  target?: string
  value?: string
}
export type PreflightItem = { key: string; level: 'block' | 'warn' | 'ok'; title: string; detail?: string; fixes?: PreflightFix[] }
export type PreflightReport = { items: PreflightItem[]; blocked: boolean; partial?: boolean; host: string; arch?: string }
export type PreflightInput = { hostId: string; name: string; compose: string; env: string }

const key = (d: PreflightInput) => ['preflight', d.hostId, d.name, d.compose, d.env]
const fetchReport = (d: PreflightInput) => api<PreflightReport>('/api/stacks/preflight', { method: 'POST', body: d })

/** Checks the files as they change (debounced). */
export function usePreflight(input: PreflightInput, enabled: boolean) {
  const d = useDebounced(input, 900)
  const settled = d.hostId === input.hostId && d.name === input.name && d.compose === input.compose && d.env === input.env
  const q = useQuery({
    queryKey: key(d),
    queryFn: () => fetchReport(d),
    enabled: enabled && !!d.hostId && !!d.name && d.compose.trim() !== '',
    staleTime: 30_000,
    retry: false,
  })
  return { ...q, settled }
}

/** The report for exactly these files, from the cache when it's fresh (for Deploy). */
export function useFreshPreflight() {
  const qc = useQueryClient()
  return (d: PreflightInput) => qc.fetchQuery({ queryKey: key(d), queryFn: () => fetchReport(d), staleTime: 30_000 })
}

const ICON = { block: Ban, warn: AlertTriangle, ok: CircleCheck }
const TONE = {
  block: 'bg-rose-500/10 text-rose-100 ring-rose-500/20',
  warn: 'bg-amber-500/10 text-amber-100 ring-amber-500/20',
  ok: 'text-ink-100/60 ring-transparent',
}

export function PreflightPanel({
  check,
  hostName,
  onFix,
  canFix = () => true,
  waiting,
}: {
  check: ReturnType<typeof usePreflight>
  hostName: string
  /** Applies a suggested fix to the form; without it fixes are shown as text. */
  onFix?: (f: PreflightFix) => void
  canFix?: (f: PreflightFix) => boolean
  /** Why it isn't checking yet (e.g. the form has errors), if so. */
  waiting?: string | null
}) {
  const rep = check.data
  const busy = check.isFetching || (!check.settled && !waiting)
  const problems = rep?.items.filter((i) => i.level !== 'ok') ?? []
  const fine = rep?.items.filter((i) => i.level === 'ok') ?? []
  const blocks = problems.filter((i) => i.level === 'block').length
  return (
    <section aria-label={`Checks on ${hostName}`} className="space-y-2.5 rounded-[3px] bg-white/[0.02] p-3 text-xs leading-relaxed ring-1 ring-line">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-sm font-medium text-ink-100/80">Checks on {hostName || 'the host'}</h3>
        {busy && <LoaderCircle className="size-3.5 animate-spin text-ink-100/50" aria-label="Checking" />}
        {rep && !busy && (
          <span className={cx('font-medium', blocks ? 'text-rose-200' : problems.length ? 'text-amber-200' : 'text-emerald-200')}>
            {blocks ? `${blocks} to fix before deploying` : problems.length ? `Ready, with ${problems.length} thing${problems.length === 1 ? '' : 's'} to check` : 'Ready to deploy'}
          </span>
        )}
        <Button type="button" variant="ghost" icon={RefreshCw} className="ml-auto !px-2 !py-1 text-[11px]" onClick={() => check.refetch()} disabled={!!waiting || check.isFetching}>
          Check again
        </Button>
      </div>
      {waiting && <p className="text-ink-100/50">{waiting}</p>}
      {!waiting && !rep && !check.isError && <p className="text-ink-100/50">Checking the image, ports, networks and limits against {hostName || 'the host'}…</p>}
      {!waiting && check.isError && <p className="text-amber-200">Couldn't check: {errorText(check.error)}. You can still deploy.</p>}
      {!waiting && rep && (
        <>
          {problems.length > 0 && (
            <ul className="space-y-1.5">
              {problems.map((i) => (
                <Item key={i.key + i.title} i={i} onFix={onFix} canFix={canFix} />
              ))}
            </ul>
          )}
          {fine.length > 0 && (
            <ul className="space-y-0.5">
              {fine.map((i) => (
                <Item key={i.key + i.title} i={i} />
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  )
}

function Item({ i, onFix, canFix }: { i: PreflightItem; onFix?: (f: PreflightFix) => void; canFix?: (f: PreflightFix) => boolean }) {
  const Icon = ICON[i.level]
  const fixes = (i.fixes ?? []).filter((f) => !canFix || canFix(f))
  return (
    <li className={cx('rounded-[3px] ring-1 ring-inset', i.level === 'ok' ? 'px-1' : 'px-3 py-2', TONE[i.level])}>
      <div className="flex gap-2">
        <Icon className={cx('mt-0.5 size-3.5 shrink-0', i.level === 'ok' && 'text-emerald-300/80')} />
        <div className="min-w-0 flex-1">
          <p className={cx('break-words', i.level !== 'ok' && 'font-medium')}>{i.title}</p>
          {i.detail && <p className={cx('mt-0.5 whitespace-pre-wrap break-words', i.level === 'ok' ? 'text-ink-100/45' : 'opacity-80')}>{i.detail}</p>}
          {fixes.length > 0 && (
            <div className="mt-1.5 flex flex-wrap gap-1.5">
              {fixes.map((f) =>
                onFix ? (
                  <Button key={f.label} type="button" variant="secondary" icon={Wand2} className="!px-2 !py-1 text-[11px]" onClick={() => onFix(f)}>
                    {f.label}
                  </Button>
                ) : (
                  <span key={f.label} className="rounded-[2px] bg-black/30 px-2 py-0.5">
                    Suggestion: {f.label}
                  </span>
                ),
              )}
            </div>
          )}
        </div>
      </div>
    </li>
  )
}
