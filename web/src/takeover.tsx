import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { AlertTriangle, Ban, CircleCheck, FolderOpen, Info, LoaderCircle, RefreshCw } from 'lucide-react'
import { api, errorText } from './api'
import { Button, CopyButton, cx } from './ui'

export interface TakeoverIssue {
  key: string
  level: 'block' | 'confirm' | 'info'
  title: string
  detail?: string
  /** A command to run on the host, if one helps. */
  fix?: string
}

export interface TakeoverReport {
  issues: TakeoverIssue[]
  /** Sent back to confirm the issues that need it; empty when there are none. */
  ack: string
  blocked: boolean
  workingDir?: string
  dir: string
  running: number
}

function useDebounced<T>(value: T, ms: number) {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return v
}

/** Checks a takeover as the files are edited (debounced). */
export function useTakeoverCheck(input: { hostId: string; name: string; compose: string; env: string }, enabled: boolean) {
  const d = useDebounced(input, 700)
  return useQuery({
    queryKey: ['takeover-check', d.hostId, d.name, d.compose, d.env],
    queryFn: () => api<TakeoverReport>('/api/stacks/takeover-check', { method: 'POST', body: d }),
    enabled: enabled && !!d.hostId && !!d.name && d.compose.trim() !== '',
    staleTime: 10_000,
    retry: false,
  })
}

/** Whether the deploy may go ahead, and the ack to send with it. */
export function takeoverReady(rep: TakeoverReport | undefined, acked: string) {
  if (!rep || rep.blocked) return false
  return rep.ack === '' || rep.ack === acked
}

/**
 * The takeover check's findings. Issues that need confirming get one
 * checkbox; ticking it records the report's ack, so editing the files (which
 * changes the findings) asks again.
 */
export function TakeoverPanel({
  check,
  acked,
  setAcked,
  name,
}: {
  check: ReturnType<typeof useTakeoverCheck>
  acked: string
  setAcked: (ack: string) => void
  name: string
}) {
  const rep = check.data
  const icon = { block: Ban, confirm: AlertTriangle, info: Info }
  const tone = {
    block: 'bg-rose-500/10 text-rose-100 ring-rose-500/20',
    confirm: 'bg-amber-500/10 text-amber-100 ring-amber-500/20',
    info: 'bg-white/[0.03] text-ink-100/70 ring-white/[0.08]',
  }
  return (
    <div className="space-y-2.5 text-xs leading-relaxed">
      <div className="flex items-center gap-2">
        <h3 className="font-semibold text-white">Takeover check</h3>
        {check.isFetching && <LoaderCircle className="size-3.5 animate-spin text-ink-100/50" />}
      </div>
      {!rep && !check.isError && <p className="text-ink-100/50">{check.isFetching ? 'Comparing with what runs now…' : 'Paste the compose file to check it against what runs now.'}</p>}
      {check.isError && <p className="text-rose-200">{errorText(check.error)}</p>}
      {rep && (
        <>
          <p className="text-ink-100/60">
            {rep.workingDir ? (
              <>
                Started from <code className="mono text-ink-50">{rep.workingDir}</code>; TainerHub runs it from <code className="mono text-ink-50">{rep.dir}</code>.
              </>
            ) : (
              <>
                TainerHub runs it from <code className="mono text-ink-50">{rep.dir}</code>.
              </>
            )}{' '}
            On the first deploy, compose recreates any of its {rep.running || ''} container{rep.running === 1 ? '' : 's'} whose settings differ from this file, and leaves the rest running.
          </p>
          {rep.issues.length === 0 && (
            <p className="flex items-center gap-1.5 text-emerald-200">
              <CircleCheck className="size-3.5" />
              Nothing changes for {name}'s data: same folders, same volumes, same services.
            </p>
          )}
          <ul className="space-y-1.5">
            {rep.issues.map((i) => {
              const Icon = icon[i.level]
              return (
                <li key={i.key} className={cx('rounded-[3px] px-3 py-2 ring-1 ring-inset', tone[i.level])}>
                  <div className="flex gap-2">
                    <Icon className="mt-0.5 size-3.5 shrink-0" />
                    <div className="min-w-0 flex-1">
                      <p className="font-medium">{i.title}</p>
                      {i.detail && <p className="mt-0.5 whitespace-pre-wrap opacity-80">{i.detail}</p>}
                      {i.fix && (
                        <div className="mt-1.5 flex items-start gap-2">
                          <code className="mono min-w-0 flex-1 break-all rounded-[2px] bg-black/40 px-2 py-1 text-lime-200">{i.fix}</code>
                          <CopyButton text={i.fix} label="Copy" />
                        </div>
                      )}
                    </div>
                  </div>
                </li>
              )
            })}
          </ul>
          {!rep.blocked && rep.ack !== '' && (
            <label className="flex items-start gap-2.5 rounded-[3px] bg-white/[0.03] p-3 ring-1 ring-inset ring-white/[0.08]">
              <input type="checkbox" checked={acked === rep.ack} onChange={(e) => setAcked(e.target.checked ? rep.ack : '')} className="mt-0.5 accent-lime-400" />
              <span className="text-ink-50">I've dealt with these (copied the data, or they're intended).</span>
            </label>
          )}
          {rep.blocked && <p className="text-rose-200">Fix the problems above to deploy. You can still save the stack without deploying.</p>}
        </>
      )}
    </div>
  )
}

/** A running project's own files, read from its host (GET /api/stacks/takeover-source). */
export interface TakeoverSource {
  compose: string
  env: string
  working_dir: string
  config_files: string[]
  env_file?: string
  copied?: string[]
  warnings?: string[]
}

export function useTakeoverSource(hostId: string, name: string, enabled: boolean) {
  return useQuery({
    queryKey: ['takeover-source', hostId, name],
    queryFn: () => api<TakeoverSource>(`/api/stacks/takeover-source?host=${encodeURIComponent(hostId)}&name=${encodeURIComponent(name)}`),
    enabled: enabled && !!hostId && !!name,
    staleTime: Infinity,
    retry: false,
  })
}

/** Where the editor's contents came from, above the editor on the takeover page. */
export function SourceNote({ source, onReload }: { source: ReturnType<typeof useTakeoverSource>; onReload: () => void }) {
  if (source.isPending && source.isFetching) {
    return (
      <p className="flex items-center gap-2 text-xs text-ink-100/60">
        <LoaderCircle className="size-3.5 animate-spin" />
        Reading its compose files from the folder it was started in…
      </p>
    )
  }
  if (source.isError) {
    return (
      <p className="rounded-[3px] bg-amber-500/10 px-3 py-2 text-xs leading-relaxed text-amber-100 ring-1 ring-inset ring-amber-500/20">
        Couldn't read it from the host: {errorText(source.error)}. Paste its compose file instead.
      </p>
    )
  }
  const src = source.data
  if (!src) return null
  return (
    <div className="flex flex-wrap items-start gap-3 rounded-[3px] bg-lime-500/10 px-3 py-2.5 text-xs leading-relaxed text-lime-100 ring-1 ring-inset ring-lime-500/20">
      <FolderOpen className="mt-0.5 size-3.5 shrink-0" />
      <div className="min-w-0 flex-1 space-y-1">
        <p>
          Read from <code className="mono">{src.working_dir}</code>
          {src.config_files.length > 1 ? `, merging ${src.config_files.length} compose files` : ''}
          {src.env_file ? ', with its .env' : ''}. Relative paths are now absolute, so its data stays where it is and nothing is copied.
        </p>
        {src.copied && src.copied.length > 0 && <p className="opacity-80">Copied into the stack's folder, since the agent can't read them where they are: {src.copied.join(', ')}.</p>}
        <p className="opacity-70">This is compose's own tidied-up version of the files: comments and formatting aren't kept. You can edit it, or paste your own.</p>
      </div>
      <Button variant="ghost" icon={RefreshCw} loading={source.isFetching} onClick={onReload}>
        Read again
      </Button>
    </div>
  )
}
