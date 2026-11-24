// The collapsible "Advanced" sections of the New container form, plus the
// small form pieces it shares with NewContainer.tsx.
import { useId, useMemo, type ReactNode } from 'react'
import { ChevronRight, Plus, X } from 'lucide-react'
import { Badge, Button, CopyButton, IconButton, Input, cx } from '../ui'
import { CodeEditor } from '../stacks'
import { CAPS, DROP_CAPS, splitArgs, type Advanced, type KV, type Problem, type Section } from '../compose'

export const field =
  'mono rounded-[2px] bg-ink-950/70 px-2.5 py-1.5 text-sm text-white ring-1 ring-inset ring-white/12 outline-none placeholder:text-ink-100/30 focus:ring-lime-400/60 disabled:opacity-50'
const select = 'w-full rounded-[2px] bg-ink-950/70 px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60'

export function Rows<T>({
  title,
  hint,
  items,
  setItems,
  blank,
  render,
  disabled,
  error,
}: {
  title: string
  hint?: ReactNode
  items: T[]
  setItems: (f: (old: T[]) => T[]) => void
  blank: T
  render: (item: T, set: (v: T) => void) => ReactNode
  disabled?: boolean
  error?: string[]
}) {
  return (
    <fieldset disabled={disabled}>
      <div className="mb-2 flex items-center justify-between">
        <legend className="text-sm font-medium text-ink-100/80">{title}</legend>
        <Button type="button" variant="ghost" icon={Plus} className="!px-2 !py-1 text-[11px]" onClick={() => setItems((old) => [...old, blank])}>
          Add
        </Button>
      </div>
      {hint && <p className="-mt-1 mb-2 text-xs text-ink-100/45">{hint}</p>}
      <div className="space-y-2">
        {items.map((it, i) => (
          <div key={i} className="flex items-center gap-2">
            {render(it, (v) => setItems((old) => old.map((o, j) => (j === i ? v : o))))}
            <IconButton icon={X} label="Remove row" onClick={() => setItems((old) => old.filter((_, j) => j !== i))} />
          </div>
        ))}
      </div>
      <FieldError msgs={error} />
    </fieldset>
  )
}

export function FieldError({ msgs, id }: { msgs?: string[]; id?: string }) {
  if (!msgs?.length) return null
  return (
    <ul id={id} className="mt-1.5 space-y-0.5 text-xs text-red-300">
      {[...new Set(msgs)].map((m) => (
        <li key={m}>{m}</li>
      ))}
    </ul>
  )
}

/** A text input with a label, an optional hint and its validation messages. */
function Text({ label, value, onChange, placeholder, hint, error, list, mono = true, className }: { label: string; value: string; onChange: (v: string) => void; placeholder?: string; hint?: ReactNode; error?: string[]; list?: string; mono?: boolean; className?: string }) {
  const id = useId()
  return (
    <div className={className}>
      <Input
        label={label}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        list={list}
        className={mono ? 'mono' : undefined}
        aria-invalid={error?.length ? true : undefined}
        aria-describedby={error?.length ? id : undefined}
        spellCheck={false}
        autoCapitalize="off"
        autoComplete="off"
      />
      {hint && <span className="mt-1.5 block text-xs text-ink-100/50">{hint}</span>}
      <FieldError msgs={error} id={id} />
    </div>
  )
}

function Check({ label, checked, onChange, hint, tone }: { label: string; checked: boolean; onChange: (v: boolean) => void; hint?: ReactNode; tone?: 'bad' }) {
  return (
    <label className="flex items-start gap-2.5 text-sm">
      <input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} className={cx('mt-0.5 size-4 shrink-0', tone === 'bad' ? 'accent-red-500' : 'accent-lime-400')} />
      <span>
        <span className={cx('font-medium', tone === 'bad' ? 'text-red-200' : 'text-ink-100/85')}>{label}</span>
        {hint && <span className="mt-0.5 block text-xs text-ink-100/50">{hint}</span>}
      </span>
    </label>
  )
}

/** A number with a unit select, e.g. 512 MB. */
function Amount({ label, value, unit, onValue, onUnit, error, placeholder }: { label: string; value: string; unit: 'm' | 'g'; onValue: (v: string) => void; onUnit: (u: 'm' | 'g') => void; error?: string[]; placeholder?: string }) {
  const id = useId()
  return (
    <div>
      <label htmlFor={id} className="mb-1.5 block text-sm font-medium text-ink-100/80">
        {label}
      </label>
      <div className="flex gap-2">
        <input id={id} inputMode="decimal" value={value} onChange={(e) => onValue(e.target.value.trim())} placeholder={placeholder} aria-invalid={error?.length ? true : undefined} className={field + ' min-w-0 flex-1 !py-2'} />
        <select aria-label={`${label} unit`} value={unit} onChange={(e) => onUnit(e.target.value as 'm' | 'g')} className={field + ' w-20 !py-2'}>
          <option value="m" className="bg-panel">MB</option>
          <option value="g" className="bg-panel">GB</option>
        </select>
      </div>
      <FieldError msgs={error} />
    </div>
  )
}

function Chips({ label, options, value, onChange, tone }: { label: string; options: readonly string[]; value: string[]; onChange: (v: string[]) => void; tone?: 'bad' }) {
  return (
    <fieldset>
      <legend className="mb-1.5 text-sm font-medium text-ink-100/80">{label}</legend>
      <div className="flex flex-wrap gap-1.5">
        {options.map((o) => {
          const on = value.includes(o)
          return (
            <button
              key={o}
              type="button"
              aria-pressed={on}
              onClick={() => onChange(on ? value.filter((x) => x !== o) : [...value, o])}
              className={cx(
                'mono rounded-[2px] px-2 py-1 text-[11px] ring-1 ring-inset transition focus-visible:outline-2 focus-visible:outline-lime-400',
                on ? (tone === 'bad' ? 'bg-red-500/15 text-red-200 ring-red-400/50' : 'bg-lime-400/15 text-lime-200 ring-lime-400/50') : 'text-ink-100/60 ring-white/12 hover:text-white',
              )}
            >
              {o}
            </button>
          )
        })}
      </div>
    </fieldset>
  )
}

function ArgsPreview({ line }: { line: string }) {
  if (!line.trim()) return null
  const r = splitArgs(line)
  if (r.error) return null
  return (
    <span className="mono mt-1.5 block break-all text-[11px] text-ink-100/50">
      → [{r.args.map((a) => JSON.stringify(a)).join(', ')}]
    </span>
  )
}

/** A collapsible section header: a button with aria-expanded, and a count of set options while collapsed. */
function Fold({ id, title, count, issues, open, onToggle, children }: { id: Section; title: string; count: number; issues: number; open: boolean; onToggle: (id: Section) => void; children: ReactNode }) {
  const panel = `adv-${id}`
  return (
    <div className="rounded-[3px] ring-1 ring-line">
      <h3>
        <button
          type="button"
          aria-expanded={open}
          aria-controls={panel}
          onClick={() => onToggle(id)}
          className="flex w-full items-center gap-2 px-3 py-2.5 text-left text-sm font-medium text-ink-100/85 transition hover:bg-white/[0.03] focus-visible:outline-2 focus-visible:outline-lime-400"
        >
          <ChevronRight className={cx('size-4 shrink-0 text-ink-100/50 transition-transform', open && 'rotate-90')} />
          <span className="flex-1">{title}</span>
          {issues > 0 && <Badge tone="bad">{issues === 1 ? '1 problem' : `${issues} problems`}</Badge>}
          {!open && count > 0 && <Badge tone="accent">{count} set</Badge>}
        </button>
      </h3>
      {open && (
        <div id={panel} className="space-y-4 border-t border-line px-3 py-4">
          {children}
        </div>
      )}
    </div>
  )
}

const timeZones = (() => {
  try {
    return (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.('timeZone') ?? []
  } catch {
    return []
  }
})()
const myZone = (() => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone ?? ''
  } catch {
    return ''
  }
})()

export function AdvancedOptions({
  adv,
  set,
  open,
  onToggle,
  problems,
  counts,
  networks,
  networksLoading,
  networksError,
  envHasTZ,
}: {
  adv: Advanced
  set: (patch: Partial<Advanced>) => void
  open: Set<Section>
  onToggle: (id: Section) => void
  problems: Problem[]
  counts: Record<Exclude<Section, 'basic'>, number>
  networks: string[]
  networksLoading: boolean
  networksError: string | null
  envHasTZ: boolean
}) {
  const err = (f: string) => problems.filter((p) => p.field === f).map((p) => p.msg)
  const issues = (s: Section) => problems.filter((p) => p.section === s).length
  const fold = (id: Exclude<Section, 'basic'>, title: string, children: ReactNode) => (
    <Fold id={id} title={title} count={counts[id]} issues={issues(id)} open={open.has(id)} onToggle={onToggle}>
      {children}
    </Fold>
  )
  const ownNet = adv.networkMode === ''
  const choices = useMemo(() => [...new Set([...networks, ...adv.networks])].sort(), [networks, adv.networks])

  return (
    <div className="space-y-2">
      <h3 className="text-sm font-medium text-ink-100/80">Advanced</h3>

      {fold(
        'network',
        'Networking',
        <>
          <label className="block">
            <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Network mode</span>
            <select value={adv.networkMode} onChange={(e) => set({ networkMode: e.target.value as Advanced['networkMode'] })} className={select}>
              <option value="" className="bg-panel">default (the stack's own bridge network)</option>
              <option value="host" className="bg-panel">host (share the host's network)</option>
              <option value="none" className="bg-panel">none (no network)</option>
            </select>
            {adv.networkMode === 'host' && <span className="mt-1.5 block text-xs text-amber-200">Ports are turned off: on the host network the app listens on the host's ports directly, so there's nothing to map.</span>}
            {adv.networkMode === 'none' && <span className="mt-1.5 block text-xs text-amber-200">Ports and networks are turned off: the container has no network at all.</span>}
          </label>
          <fieldset disabled={!ownNet} className="disabled:opacity-50">
            <legend className="mb-1.5 text-sm font-medium text-ink-100/80">Join existing networks on this host</legend>
            {networksLoading ? (
              <p className="text-xs text-ink-100/50">Loading networks…</p>
            ) : networksError ? (
              <p className="text-xs text-amber-200">Couldn't list networks: {networksError}</p>
            ) : choices.length === 0 ? (
              <p className="text-xs text-ink-100/50">No user networks on this host yet.</p>
            ) : (
              <div className="grid grid-cols-1 gap-1.5 sm:grid-cols-2">
                {choices.map((n) => (
                  <Check key={n} label={n} checked={adv.networks.includes(n)} onChange={(on) => set({ networks: on ? [...adv.networks, n] : adv.networks.filter((x) => x !== n) })} />
                ))}
              </div>
            )}
            <FieldError msgs={err('networks')} />
            <Text
              className="mt-3"
              label="Create a new network"
              value={adv.newNetwork}
              onChange={(v) => set({ newNetwork: v.trim() })}
              placeholder="e.g. backend"
              hint="Created with this exact name, so other stacks can join it. Joining any network replaces the stack's default one."
              error={err('newNetwork')}
            />
          </fieldset>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Text label="Hostname" value={adv.hostname} onChange={(v) => set({ hostname: v })} placeholder="app" error={err('hostname')} />
            <Text label="DNS servers" value={adv.dns} onChange={(v) => set({ dns: v })} placeholder="1.1.1.1 9.9.9.9" hint="Separate with spaces or commas." error={err('dns')} />
          </div>
          <Rows<KV>
            title="Extra hosts"
            hint={
              <>
                Adds lines to /etc/hosts. Use <code className="mono">host-gateway</code> as the IP to reach the Docker host.
              </>
            }
            items={adv.extraHosts}
            setItems={(f) => set({ extraHosts: f(adv.extraHosts) })}
            blank={{ key: '', value: '' }}
            error={err('extraHosts')}
            render={(h, setH) => (
              <>
                <input aria-label="Extra host name" placeholder="db.lan" value={h.key} onChange={(e) => setH({ ...h, key: e.target.value.trim() })} className={field + ' min-w-0 flex-1'} />
                <span className="text-ink-100/40">→</span>
                <input aria-label="Extra host IP" placeholder="10.0.0.5" value={h.value} onChange={(e) => setH({ ...h, value: e.target.value.trim() })} className={field + ' min-w-0 flex-1'} />
              </>
            )}
          />
        </>,
      )}

      {fold(
        'labels',
        'Labels',
        <Rows<KV>
          title="Labels"
          hint={
            <>
              For Traefik, for example: <code className="mono text-lime-300">traefik.enable</code> = <code className="mono text-lime-300">true</code> and{' '}
              <code className="mono text-lime-300">traefik.http.routers.myapp.rule</code> = <code className="mono text-lime-300">{'Host(`app.example.com`)'}</code>. Values are passed as-is; “$” is kept literally.
            </>
          }
          items={adv.labels}
          setItems={(f) => set({ labels: f(adv.labels) })}
          blank={{ key: '', value: '' }}
          error={err('labels')}
          render={(l, setL) => (
            <>
              <input aria-label="Label key" placeholder="com.example.key" value={l.key} onChange={(e) => setL({ ...l, key: e.target.value.trim() })} className={field + ' min-w-0 flex-1'} />
              <span className="text-ink-100/40">=</span>
              <input aria-label="Label value" placeholder="value" value={l.value} onChange={(e) => setL({ ...l, value: e.target.value })} className={field + ' min-w-0 flex-1'} />
            </>
          )}
        />,
      )}

      {fold(
        'runtime',
        'Command & runtime',
        <>
          <Text label="Command override" value={adv.command} onChange={(v) => set({ command: v })} placeholder={`e.g. --port 8080 --log-level "debug"`} hint={<ArgsPreview line={adv.command} />} error={err('command')} />
          <Text label="Entrypoint override" value={adv.entrypoint} onChange={(v) => set({ entrypoint: v })} placeholder="e.g. /bin/sh -c" hint={<ArgsPreview line={adv.entrypoint} />} error={err('entrypoint')} />
          <p className="-mt-2 text-xs text-ink-100/45">Split like a shell line: quote arguments that contain spaces. Nothing is expanded.</p>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Text label="Working directory" value={adv.workingDir} onChange={(v) => set({ workingDir: v })} placeholder="/app" error={err('workingDir')} />
            <Text label="User" value={adv.user} onChange={(v) => set({ user: v })} placeholder="1000:1000" error={err('user')} />
            <Text label="Stop grace period" value={adv.stopGrace} onChange={(v) => set({ stopGrace: v.trim() })} placeholder="10s" hint="Time to shut down before being killed." error={err('stopGrace')} />
            <div>
              <Text
                label="Time zone"
                value={adv.tz}
                onChange={(v) => set({ tz: v.trim() })}
                placeholder={myZone || 'Europe/London'}
                list={timeZones.length ? 'tz-options' : undefined}
                hint={envHasTZ ? 'TZ is already set under Environment variables, so this is ignored.' : 'Sets the TZ environment variable.'}
                error={err('tz')}
              />
              {myZone && !adv.tz && (
                <button type="button" onClick={() => set({ tz: myZone })} className="mt-1 text-xs text-lime-300 hover:underline">
                  Use {myZone}
                </button>
              )}
            </div>
          </div>
          {timeZones.length > 0 && (
            <datalist id="tz-options">
              {timeZones.map((z) => (
                <option key={z} value={z} />
              ))}
            </datalist>
          )}
          <div className="space-y-2">
            <Check label="Run an init process (init)" checked={adv.init} onChange={(v) => set({ init: v })} hint="Reaps zombie processes and forwards signals; useful for apps that don't." />
            <Check label="Read-only root filesystem" checked={adv.readOnly} onChange={(v) => set({ readOnly: v })} hint="The app can only write to volumes. Many images need a writable /tmp volume for this." />
          </div>
        </>,
      )}

      {fold(
        'resources',
        'Resources',
        <>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <Amount label="Memory limit" value={adv.memory} unit={adv.memoryUnit} onValue={(v) => set({ memory: v })} onUnit={(u) => set({ memoryUnit: u })} placeholder="512" error={err('memory')} />
            <Text label="CPU limit (cores)" value={adv.cpus} onChange={(v) => set({ cpus: v.trim() })} placeholder="1.5" error={err('cpus')} />
            <Amount label="Shared memory (/dev/shm)" value={adv.shm} unit={adv.shmUnit} onValue={(v) => set({ shm: v })} onUnit={(u) => set({ shmUnit: u })} placeholder="64" error={err('shm')} />
          </div>
          <Rows<string>
            title="Devices"
            hint={
              <>
                Host device, optionally with a path in the container and permissions: <code className="mono">/dev/dri</code> or <code className="mono">/dev/ttyUSB0:/dev/ttyUSB0:rwm</code>.
              </>
            }
            items={adv.devices}
            setItems={(f) => set({ devices: f(adv.devices) })}
            blank=""
            error={err('devices')}
            render={(d, setD) => <input aria-label="Device" placeholder="/dev/dri" value={d} onChange={(e) => setD(e.target.value.trim())} className={field + ' min-w-0 flex-1'} />}
          />
          <Check label="Use all NVIDIA GPUs" checked={adv.gpu} onChange={(v) => set({ gpu: v })} hint="Needs the NVIDIA Container Toolkit on the host." />
        </>,
      )}

      {fold(
        'health',
        'Health check',
        <>
          <label className="block">
            <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Health check</span>
            <select value={adv.health} onChange={(e) => set({ health: e.target.value as Advanced['health'] })} className={select}>
              <option value="image" className="bg-panel">use the image's (if it has one)</option>
              <option value="custom" className="bg-panel">custom command</option>
              <option value="disable" className="bg-panel">disable the image's health check</option>
            </select>
          </label>
          {adv.health === 'custom' && (
            <>
              <Text label="Command (run with sh -c)" value={adv.hcTest} onChange={(v) => set({ hcTest: v })} placeholder="curl -f http://localhost/ || exit 1" hint="Exit code 0 means healthy. The command must exist inside the image." error={err('hcTest')} />
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                <Text label="Interval" value={adv.hcInterval} onChange={(v) => set({ hcInterval: v.trim() })} placeholder="30s" error={err('hcInterval')} />
                <Text label="Timeout" value={adv.hcTimeout} onChange={(v) => set({ hcTimeout: v.trim() })} placeholder="30s" error={err('hcTimeout')} />
                <Text label="Retries" value={adv.hcRetries} onChange={(v) => set({ hcRetries: v.trim() })} placeholder="3" error={err('hcRetries')} />
                <Text label="Start period" value={adv.hcStart} onChange={(v) => set({ hcStart: v.trim() })} placeholder="0s" error={err('hcStart')} />
              </div>
            </>
          )}
        </>,
      )}

      {fold(
        'security',
        'Security',
        <>
          <Chips label="Add capabilities (cap_add)" options={CAPS} value={adv.capAdd} onChange={(v) => set({ capAdd: v })} />
          <Chips label="Drop capabilities (cap_drop)" options={DROP_CAPS} value={adv.capDrop} onChange={(v) => set({ capDrop: v })} tone="bad" />
          <FieldError msgs={err('caps')} />
          <Check label="No new privileges" checked={adv.noNewPrivileges} onChange={(v) => set({ noNewPrivileges: v })} hint="Stops processes from gaining privileges through setuid binaries. Safe for most apps." />
          <Check
            label="Privileged"
            tone="bad"
            checked={adv.privileged}
            onChange={(v) => set({ privileged: v })}
            hint="Gives the container every capability and all host devices."
          />
          {adv.privileged && (
            <p role="alert" className="rounded-[3px] bg-red-500/10 px-3 py-2 text-xs text-red-200 ring-1 ring-red-400/40">
              A privileged container has full root access to the host. Only use it for software you trust completely; you'll be asked to confirm on deploy.
            </p>
          )}
        </>,
      )}

      {fold(
        'logging',
        'Logging',
        <>
          <Check label="Limit log size" checked={adv.logLimit} onChange={(v) => set({ logLimit: v })} hint="Rotates the json-file log so it can't fill the disk. Off means Docker's default (unlimited, unless the host sets one)." />
          {adv.logLimit && (
            <div className="grid grid-cols-2 gap-3">
              <Text label="Max size per file" value={adv.logMaxSize} onChange={(v) => set({ logMaxSize: v.trim() })} placeholder="10m" error={err('logMaxSize')} />
              <Text label="Files to keep" value={adv.logMaxFile} onChange={(v) => set({ logMaxFile: v.trim() })} placeholder="3" error={err('logMaxFile')} />
            </div>
          )}
        </>,
      )}

      {fold(
        'envfile',
        'Env file (.env)',
        <>
          <p className="text-xs text-ink-100/50">
            Paste <code className="mono">KEY=value</code> lines, e.g. passwords. They're saved as the stack's .env (only admins can read it, and it's not in the audit log) and passed to the container with{' '}
            <code className="mono">env_file</code>, so they don't appear in the compose file.
          </p>
          <CodeEditor label=".env contents" value={adv.envFile} onChange={(v) => set({ envFile: v })} rows={6} placeholder={'DB_PASSWORD=change-me\n# comments are fine'} />
          <FieldError msgs={err('envFile')} />
        </>,
      )}
    </div>
  )
}

/** The compose file, lightly highlighted: keys, strings and list dashes. */
export function ComposePreview({ compose }: { compose: string }) {
  const lines = compose.replace(/\n$/, '').split('\n')
  return (
    <div>
      <div className="mb-2 flex items-center justify-between gap-2">
        <h3 className="text-sm font-medium text-ink-100/80">Compose file</h3>
        <CopyButton text={compose} variant="ghost" />
      </div>
      <pre aria-label="Compose file preview" className="mono max-h-[28rem] overflow-auto rounded-[3px] bg-black/40 p-3 text-xs leading-relaxed text-ink-100/80 ring-1 ring-line">
        {lines.map((l, i) => (
          <Line key={i} text={l} />
        ))}
      </pre>
      <p className="mt-1.5 text-xs text-ink-100/45">Exactly what will be deployed. You can edit it later under Stacks.</p>
    </div>
  )
}

function Line({ text }: { text: string }) {
  const m = /^(\s*)(- )?("(?:[^"\\]|\\.)*"|[\w.-]+)(:)(.*)$/.exec(text)
  if (!m) {
    const d = /^(\s*)(- )(.*)$/.exec(text)
    if (d) return <span className="block">{d[1]}<span className="text-ink-100/40">{d[2]}</span><span className="text-amber-200">{d[3]}</span></span>
    return <span className="block">{text}</span>
  }
  return (
    <span className="block">
      {m[1]}
      {m[2] && <span className="text-ink-100/40">{m[2]}</span>}
      <span className="text-lime-300">{m[3]}</span>
      <span className="text-ink-100/40">{m[4]}</span>
      <span className="text-amber-200">{m[5]}</span>
    </span>
  )
}
