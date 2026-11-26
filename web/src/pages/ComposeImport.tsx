// "Paste compose" on the New container page: reads a compose file from a
// project's docs and fills the form's own fields from one of its services.
// The parser (and the yaml package) load only when the dialog first opens.
import { useEffect, useMemo, useState } from 'react'
import { AlertTriangle, ClipboardPaste, Dices, Info, Layers, Wand2, X } from 'lucide-react'
import { Button, ErrorNote, IconButton, Modal, cx } from '../ui'
import { CodeEditor } from '../stacks'
import type { Change, Imported, Note } from '../composeimport'
import type { StackDraft } from '../stackdraft'
import { field } from './ContainerOptions'
import { generateSecret } from '../placeholder'

type Lib = typeof import('../composeimport')

const EXAMPLE = `services:
  uptime-kuma:
    image: louislam/uptime-kuma:2
    volumes:
      - ./data:/app/data
    ports:
      - "3001:3001"
    restart: unless-stopped`

export function ComposeImportDialog({
  open,
  onClose,
  onApply,
  onStack,
  replacing,
}: {
  open: boolean
  onClose: () => void
  onApply: (r: Imported) => void
  onStack: (d: StackDraft) => void
  /** The form already has something in it, which applying replaces. */
  replacing: boolean
}) {
  const [lib, setLib] = useState<Lib | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [text, setText] = useState('')
  const [picked, setPicked] = useState('')
  const [values, setValues] = useState<Record<string, string>>({})
  const [replacements, setReplacements] = useState<Record<string, string>>({})
  const replace = (id: string, v: string) => setReplacements((old) => ({ ...old, [id]: v }))

  useEffect(() => {
    if (!open || lib) return
    import('../composeimport').then(setLib, (e) => setLoadError(`Couldn't load the compose reader: ${e instanceof Error ? e.message : String(e)}`))
  }, [open, lib])

  const parsed = useMemo(() => (lib && text.trim() ? lib.parseCompose(text) : null), [lib, text])
  const ok = parsed?.ok ? parsed : null
  const service = ok ? (ok.services.includes(picked) ? picked : ok.services[0]) : ''
  const vars = useMemo(() => (ok && lib ? lib.variablesOf(ok, service) : []), [ok, lib, service])
  // Variables with a default are listed under "Changed for you" instead.
  const needed = vars.filter((v) => v.def === undefined)
  const result = useMemo(() => (ok && lib ? lib.importService(ok, service, values, replacements) : null), [ok, lib, service, values, replacements])

  const apply = () => {
    if (!result) return
    onApply(result)
    setText('')
    setValues({})
    setReplacements({})
    setPicked('')
  }
  const asStack = () => {
    if (!ok || !lib) return
    onStack(lib.stackDraft(text, ok, values))
  }

  return (
    <Modal open={open} onClose={onClose} title="Paste a compose file" wide>
      <div className="space-y-4">
        <p className="text-sm text-ink-100/70">
          Many projects publish a <code className="mono text-lime-300">docker-compose.yml</code> in their README. Paste it here to fill in the form; you can check and change everything before
          you deploy.
        </p>
        <ErrorNote error={loadError} />
        <CodeEditor label="Compose file to import" value={text} onChange={setText} rows={10} placeholder={EXAMPLE} />
        {parsed && !parsed.ok && <ErrorNote error={parsed.error} />}

        {ok && ok.services.length > 1 && (
          <fieldset className="space-y-2 rounded-[3px] bg-white/[0.03] p-3 ring-1 ring-line">
            <legend className="sr-only">Service</legend>
            <p className="text-sm text-ink-100/80">
              This file has {ok.services.length} services. A container runs one of them: which one?
            </p>
            <div className="flex flex-wrap gap-2">
              {ok.services.map((s) => (
                <label
                  key={s}
                  className={cx(
                    'mono flex cursor-pointer items-center gap-2 rounded-[2px] px-2.5 py-1.5 text-sm ring-1 ring-inset',
                    s === service ? 'bg-lime-400/10 text-white ring-lime-400/60' : 'text-ink-100/70 ring-white/12 hover:text-white',
                  )}
                >
                  <input type="radio" name="compose-service" value={s} checked={s === service} onChange={() => setPicked(s)} className="accent-lime-400" />
                  {s}
                </label>
              ))}
            </div>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-line pt-2">
              <Button type="button" variant="secondary" icon={Layers} onClick={asStack}>
                Deploy all {ok.services.length} as a stack
              </Button>
              <span className="text-xs text-ink-100/50">Opens the stack editor with this file, so the services run together.</span>
            </div>
          </fieldset>
        )}

        {needed.length > 0 && (
          <fieldset className="space-y-2">
            <legend className="mb-1 text-sm font-medium text-ink-100/80">Variables to fill in</legend>
            <p className="-mt-1 text-xs text-ink-100/50">The file uses these and gives no default.</p>
            {needed.map((v) => (
              <label key={v.name} className="flex flex-wrap items-center gap-2">
                <span className="mono w-full text-sm text-amber-200 sm:w-44 sm:shrink-0 sm:truncate">${'{'}{v.name}{'}'}</span>
                <input
                  aria-label={`Value for ${v.name}`}
                  value={values[v.name] ?? ''}
                  onChange={(e) => setValues((old) => ({ ...old, [v.name]: e.target.value }))}
                  placeholder="no default: enter a value"
                  className={cx(field, 'min-w-0 flex-1', !values[v.name] && 'ring-amber-400/40')}
                  spellCheck={false}
                  autoCapitalize="off"
                />
              </label>
            ))}
          </fieldset>
        )}

        {result && result.placeholders.length > 0 && (
          <fieldset className="space-y-2">
            <legend className="mb-1 text-sm font-medium text-ink-100/80">Replace the examples</legend>
            <p className="-mt-1 text-xs text-ink-100/50">
              These look like sample values from the project's docs. Deployed as they are, Docker would create the example folder empty on the host, or the app would run with the
              sample value. What should they be?
            </p>
            {result.placeholders.map((ph) => (
              <div key={ph.id} className="space-y-1.5 rounded-[3px] bg-amber-500/[0.06] p-2.5 ring-1 ring-inset ring-amber-500/20">
                <p className="text-xs text-amber-100">
                  <span className="mono">{ph.label}</span> <span className="text-ink-100/50">is</span> <code className="mono break-all text-amber-200">{ph.value}</code>
                </p>
                <div className="flex flex-wrap items-center gap-2">
                  <input
                    aria-label={`Replacement for ${ph.label}`}
                    value={replacements[ph.id] ?? ''}
                    onChange={(e) => replace(ph.id, e.target.value)}
                    placeholder={ph.kind === 'volume' ? `a folder on the host, or ${ph.suggestion}` : ph.secret ? 'your own value, or generate one' : 'your own value'}
                    className={cx(field, 'min-w-0 flex-1 basis-48')}
                    spellCheck={false}
                    autoCapitalize="off"
                  />
                  {ph.suggestion && (
                    <Button type="button" variant="secondary" icon={Wand2} className="!px-2 !py-1 text-[11px]" onClick={() => replace(ph.id, ph.suggestion!)}>
                      Use {ph.suggestion}
                    </Button>
                  )}
                  {ph.secret && (
                    <Button type="button" variant="secondary" icon={Dices} className="!px-2 !py-1 text-[11px]" onClick={() => replace(ph.id, generateSecret())}>
                      Generate
                    </Button>
                  )}
                </div>
                {ph.kind === 'volume' && (
                  <p className="text-[11px] text-ink-100/45">
                    {ph.suggestion} is a folder inside the stack's own folder on the host: TainerHub creates it, and it moves with the stack.
                  </p>
                )}
              </div>
            ))}
          </fieldset>
        )}

        {result && result.changes.length > 0 && (
          <fieldset className="space-y-2">
            <legend className="mb-1 text-sm font-medium text-ink-100/80">Changed for you</legend>
            <p className="-mt-1 text-xs text-ink-100/50">
              TainerHub filled these in or changed them to fit the form. <strong className="font-medium text-ink-100/70">Fill the form</strong> uses them as shown; change any you'd rather
              set differently.
            </p>
            <ul className="divide-y divide-line rounded-[3px] ring-1 ring-line">
              {result.changes.map((c) => (
                <ChangeRow
                  key={c.id}
                  c={c}
                  value={c.input === 'variable' ? (values[c.id.slice(4)] ?? '') : replacements[`change:${c.id}`]}
                  onChange={(v) => (c.input === 'variable' ? setValues((old) => ({ ...old, [c.id.slice(4)]: v })) : replace(`change:${c.id}`, v))}
                />
              ))}
            </ul>
          </fieldset>
        )}

        {result && <Summary r={result} />}
        {result && <Notes notes={result.notes} />}

        <div className="flex flex-wrap items-center gap-3 pt-1">
          <Button type="button" variant="primary" icon={ClipboardPaste} onClick={apply} disabled={!result || !result.image}>
            Fill the form{ok && ok.services.length > 1 ? ` with ${service}` : ''}
          </Button>
          <Button type="button" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          {replacing && result && <span className="text-xs text-ink-100/50">Replaces what's in the form now.</span>}
        </div>
      </div>
    </Modal>
  )
}

const RESTARTS = [
  ['unless-stopped', 'unless stopped (recommended)'],
  ['always', 'always'],
  ['on-failure', 'on failure'],
  ['no', 'never'],
] as const

/** One value TainerHub chose: what the file had, what it'll be, why, and a way to change it. */
function ChangeRow({ c, value, onChange }: { c: Change; value: string | undefined; onChange: (v: string) => void }) {
  const control =
    c.input === 'restart' ? (
      <select aria-label={c.label} value={value || c.to} onChange={(e) => onChange(e.target.value)} className={cx(field, 'min-w-0 flex-1 basis-40')}>
        {RESTARTS.map(([v, label]) => (
          <option key={v} value={v} className="bg-panel">
            {label}
          </option>
        ))}
      </select>
    ) : c.input === 'variable' ? (
      <input
        aria-label={`Value for ${c.label}`}
        value={value ?? ''}
        onChange={(e) => onChange(e.target.value)}
        placeholder={c.to === '' ? 'default: empty' : `default: ${c.to}`}
        className={cx(field, 'min-w-0 flex-1 basis-40')}
        spellCheck={false}
        autoCapitalize="off"
      />
    ) : c.input === 'text' ? (
      <input aria-label={c.label} value={value ?? c.to} onChange={(e) => onChange(e.target.value)} className={cx(field, 'min-w-0 flex-1 basis-40')} spellCheck={false} autoCapitalize="off" />
    ) : (
      <span className="mono min-w-0 flex-1 basis-40 break-words text-sm text-white">{c.to}</span>
    )
  return (
    <li className="space-y-1 px-3 py-2">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
        <span className="mono w-full shrink-0 text-sm text-lime-200 sm:w-40 sm:truncate" title={c.label}>
          {c.label}
        </span>
        <span className="mono max-w-full shrink-0 truncate text-xs text-ink-100/45" title={c.from || undefined}>
          {c.from ? <s>{c.from}</s> : 'not set'} →
        </span>
        {control}
      </div>
      <p className="text-[11px] leading-snug text-ink-100/50">{c.why}</p>
    </li>
  )
}

/** One line on what goes where. */
function Summary({ r }: { r: Imported }) {
  const n = (k: number, one: string, many = one + 's') => (k ? `${k} ${k === 1 ? one : many}` : null)
  const parts = [
    r.image && `${r.image}${r.image.includes('@') ? '' : `:${r.tag}`}`,
    `named ${r.name}`,
    n(r.ports.length, 'port'),
    n(r.vols.length, 'volume'),
    n(r.env.length, 'variable'),
  ].filter(Boolean)
  return (
    <p className="text-sm text-ink-100/70">
      <span className="text-ink-100/50">Fills in: </span>
      <span className="mono text-white">{parts.join(' · ')}</span>
      {r.open.length > 0 && <span className="text-ink-100/50"> · advanced: {r.open.map((s) => SECTION_NAMES[s]).join(', ')}</span>}
    </p>
  )
}

const SECTION_NAMES: Record<string, string> = {
  network: 'networking',
  labels: 'labels',
  runtime: 'command & runtime',
  resources: 'resources',
  health: 'health check',
  security: 'security',
  logging: 'logging',
  envfile: 'env file',
}

export function Notes({ notes, className }: { notes: Note[]; className?: string }) {
  if (!notes.length) return null
  const sorted = [...notes.filter((x) => x.level === 'warn'), ...notes.filter((x) => x.level === 'info')]
  return (
    <ul className={cx('space-y-1.5 rounded-[3px] bg-white/[0.03] p-3 text-xs ring-1 ring-line', className)} aria-label="Import notes">
      {sorted.map((x, i) => (
        <li key={i} className={cx('flex gap-2', x.level === 'warn' ? 'text-amber-200' : 'text-ink-100/60')}>
          {x.level === 'warn' ? <AlertTriangle className="mt-px size-3.5 shrink-0" aria-label="Warning" /> : <Info className="mt-px size-3.5 shrink-0" aria-label="Note" />}
          <span className="min-w-0 break-words">{x.msg}</span>
        </li>
      ))}
    </ul>
  )
}

/** The notes from the last import, kept above the settings until dismissed. */
export function ImportedNotice({ service, notes, changes, onDismiss }: { service: string; notes: Note[]; changes: Change[]; onDismiss: () => void }) {
  const warnings = notes.filter((n) => n.level === 'warn').length
  return (
    <div className="space-y-2 rounded-[3px] bg-lime-400/[0.05] p-3 ring-1 ring-lime-400/25">
      <div className="flex items-start justify-between gap-2">
        <p className="text-sm text-ink-100/80">
          Filled in from the compose file’s <span className="mono text-white">{service}</span> service.{' '}
          {warnings ? `${warnings} thing${warnings === 1 ? '' : 's'} to check:` : 'Everything in it was mapped. Check the settings below, then deploy.'}
        </p>
        <IconButton icon={X} label="Dismiss import notes" onClick={onDismiss} />
      </div>
      <Notes notes={notes} className="bg-transparent p-0 ring-0" />
      {changes.length > 0 && (
        <div className="space-y-1 border-t border-lime-400/15 pt-2 text-xs">
          <p className="text-ink-100/60">Changed for you (adjust them in the form below):</p>
          <ul className="space-y-0.5">
            {changes.map((c) => (
              <li key={c.id} className="flex flex-wrap gap-x-1.5 text-ink-100/70">
                <span className="mono text-lime-200">{c.label}</span>
                <span className="text-ink-100/45">{c.from ? <s className="mono">{c.from}</s> : 'not set'} →</span>
                <span className="mono break-all text-white">{c.to}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}
