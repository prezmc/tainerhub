import { useMemo, useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowLeft, BadgeCheck, Download, Plus, Rocket, Search, Star, X } from 'lucide-react'
import { api, errorText, stackProblem, type StackSaveResult } from '../api'
import { Badge, Button, Card, ErrorNote, IconButton, Input, PageHeader, cx, useToast } from '../ui'
import { useFleet } from '../fleet'
import { useOpenJob, useStacks } from '../stacks'
import { HostSelect, useDefaultHost } from '../placement'

type Repo = { name: string; description: string; stars: number; pulls: number; official: boolean }
type Tag = { name: string; updated: string; size: number }
type Port = { host: string; container: string; proto: 'tcp' | 'udp' }
type Env = { key: string; value: string }
type Vol = { source: string; target: string }

const RESTART = ['unless-stopped', 'always', 'on-failure', 'no'] as const

const short = (n: number) => (n >= 1e9 ? `${(n / 1e9).toFixed(1)}B` : n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${Math.round(n / 1e3)}k` : String(n))

/** "linuxserver/plex" -> "plex"; made into a valid stack/container name. */
function defaultName(image: string) {
  const last = image.split('/').pop()?.split(':')[0] ?? ''
  return last.toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^[-_]+/, '').slice(0, 63)
}

const q = (s: string) => JSON.stringify(s) // a JSON string is a valid YAML double-quoted scalar
const isNamedVolume = (src: string) => /^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(src)

/** Builds a one-service compose file. Values are quoted, and "$" is escaped so compose doesn't interpolate it. */
export function composeFor(o: { name: string; image: string; restart: string; ports: Port[]; env: Env[]; vols: Vol[] }) {
  const lines = ['services:', `  ${o.name}:`, `    image: ${q(o.image)}`, `    container_name: ${q(o.name)}`, `    restart: ${o.restart}`]
  const ports = o.ports.filter((p) => p.container)
  if (ports.length) {
    lines.push('    ports:')
    for (const p of ports) lines.push(`      - ${q(`${p.host ? p.host + ':' : ''}${p.container}/${p.proto}`)}`)
  }
  const env = o.env.filter((e) => e.key)
  if (env.length) {
    lines.push('    environment:')
    for (const e of env) lines.push(`      ${e.key}: ${q(e.value.replaceAll('$', '$$$$'))}`)
  }
  const vols = o.vols.filter((v) => v.source && v.target)
  if (vols.length) {
    lines.push('    volumes:')
    for (const v of vols) lines.push(`      - ${q(`${v.source}:${v.target}`)}`)
  }
  const named = [...new Set(vols.map((v) => v.source).filter(isNamedVolume))]
  if (named.length) {
    lines.push('volumes:')
    for (const n of named) lines.push(`  ${n}: {}`)
  }
  return lines.join('\n') + '\n'
}

function problems(o: { name: string; image: string; ports: Port[]; env: Env[]; vols: Vol[] }): string | null {
  if (!o.image) return 'Pick an image.'
  if (!/^[a-z0-9][a-z0-9_-]{0,62}$/.test(o.name)) return 'Name: lowercase letters, digits, - and _, starting with a letter or digit.'
  const portOK = (s: string) => /^\d{1,5}$/.test(s) && +s >= 1 && +s <= 65535
  for (const p of o.ports) {
    if (!p.container && !p.host) continue
    if (!portOK(p.container) || (p.host && !portOK(p.host))) return 'Ports must be numbers from 1 to 65535.'
  }
  for (const e of o.env) if (e.key && !/^[A-Za-z_][A-Za-z0-9_]*$/.test(e.key)) return `Environment variable names use letters, digits and _ (not "${e.key}").`
  for (const v of o.vols) {
    if (!v.source && !v.target) continue
    if (!v.source || !v.target.startsWith('/')) return 'Volumes need a source and an absolute path inside the container, like /data.'
    if (!isNamedVolume(v.source) && !/^(\/|\.\/|\.\.\/)/.test(v.source)) return `Volume source "${v.source}" should be a volume name or a path starting with / or ./`
  }
  return null
}

export function NewContainerPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const toast = useToast()
  const openJob = useOpenJob()
  const { hosts, containers } = useFleet()
  const stacks = useStacks()

  const [query, setQuery] = useState('')
  const [submitted, setSubmitted] = useState('')
  const [image, setImage] = useState('') // repository, e.g. "nginx" or "ghcr.io/org/app"
  const [tag, setTag] = useState('latest')
  const [name, setName] = useState('')
  const [hostId, setHostId] = useState('')
  const [restart, setRestart] = useState<string>('unless-stopped')
  const [ports, setPorts] = useState<Port[]>([{ host: '', container: '', proto: 'tcp' }])
  const [env, setEnv] = useState<Env[]>([])
  const [vols, setVols] = useState<Vol[]>([])
  const [error, setError] = useState<string | null>(null)

  useDefaultHost(hostId, setHostId)

  const search = useQuery({
    queryKey: ['hub-search', submitted],
    queryFn: () => api<Repo[]>(`/api/dockerhub/search?q=${encodeURIComponent(submitted)}`),
    enabled: submitted.length > 0,
    staleTime: 5 * 60_000,
  })
  const isHubImage = image !== '' && !image.includes('.') // registry hosts contain a dot: ghcr.io/...
  const tags = useQuery({
    queryKey: ['hub-tags', image],
    queryFn: () => api<Tag[]>(`/api/dockerhub/tags?repo=${encodeURIComponent(image)}`),
    enabled: isHubImage,
    staleTime: 5 * 60_000,
    retry: false,
  })

  const pick = (repo: string) => {
    setImage(repo)
    setTag('latest')
    setName(defaultName(repo))
    setError(null)
  }

  const fullImage = image ? `${image}:${tag || 'latest'}` : ''
  const spec = { name, image: fullImage, restart, ports, env, vols }
  const compose = useMemo(() => (image ? composeFor(spec) : ''), [image, tag, name, restart, ports, env, vols]) // eslint-disable-line react-hooks/exhaustive-deps
  const host = hosts.find((h) => h.id === hostId)
  const hostIssue = stackProblem(host)
  const clash =
    containers.find((c) => c.hostId === hostId && c.name === name) || stacks.data?.find((s) => s.hostId === hostId && s.name === name && s.managed)
      ? `${host?.name ?? 'This host'} already has a container or stack called ${name}.`
      : null

  const deploy = useMutation({
    mutationFn: () => api<StackSaveResult>('/api/stacks', { method: 'POST', body: { hostId, name, compose, env: '', deploy: true } }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['stacks'] })
      if (res.deployError) toast('bad', `Saved, but the deploy didn't start: ${res.deployError}`)
      if (res.jobId) openJob(res.jobId)
      navigate({ to: '/stacks/$stackId', params: { stackId: res.stack.id } })
    },
    onError: (e) => setError(errorText(e)),
  })

  const submit = () => {
    const p = problems(spec) ?? hostIssue ?? clash
    if (p) return setError(p)
    setError(null)
    deploy.mutate()
  }

  return (
    <div>
      <Link to="/containers" className="mb-4 inline-flex items-center gap-1.5 text-sm text-ink-100/60 hover:text-white">
        <ArrowLeft className="size-4" />
        Containers
      </Link>
      <PageHeader title="New container" subtitle="Find an image on Docker Hub, set it up, and run it on one of your hosts." />

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.2fr)]">
        {/* ---- image ---- */}
        <Card className="p-5">
          <h2 className="mb-3 font-display text-lg font-bold text-white">1. Image</h2>
          <form
            className="flex gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              setSubmitted(query.trim())
            }}
          >
            <div className="flex-1">
              <Input icon={Search} value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search Docker Hub, e.g. nginx, plex, uptime-kuma" autoFocus aria-label="Search Docker Hub" />
            </div>
            <Button variant="secondary" icon={Search} loading={search.isFetching}>
              Search
            </Button>
          </form>
          <ErrorNote error={search.error ? errorText(search.error) : null} />
          <ul className="mt-3 max-h-[28rem] divide-y divide-line overflow-y-auto rounded-[3px] ring-1 ring-line empty:hidden">
            {search.data?.map((r) => (
              <li key={r.name}>
                <button
                  onClick={() => pick(r.name)}
                  className={cx('flex w-full items-start gap-3 px-3 py-2.5 text-left transition hover:bg-white/[0.04]', image === r.name && 'bg-lime-400/[0.08] shadow-[inset_3px_0_0] shadow-lime-400')}
                >
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="mono truncate text-sm font-semibold text-white">{r.name}</span>
                      {r.official && (
                        <Badge tone="good">
                          <BadgeCheck className="size-3" />
                          official
                        </Badge>
                      )}
                    </div>
                    {r.description && <p className="mt-0.5 line-clamp-2 text-xs text-ink-100/60">{r.description}</p>}
                  </div>
                  <div className="shrink-0 space-y-0.5 text-right font-mono text-[11px] text-ink-100/50">
                    <div className="flex items-center justify-end gap-1">
                      <Download className="size-3" />
                      {short(r.pulls)}
                    </div>
                    <div className="flex items-center justify-end gap-1">
                      <Star className="size-3" />
                      {short(r.stars)}
                    </div>
                  </div>
                </button>
              </li>
            ))}
          </ul>
          {search.data?.length === 0 && <p className="mt-3 text-sm text-ink-100/50">No images found for “{submitted}”.</p>}
          <p className="mt-4 text-xs text-ink-100/50">
            Not on Docker Hub? Type the full name instead, like <code className="mono text-lime-300">ghcr.io/home-assistant/home-assistant</code>:
          </p>
          <div className="mt-2">
            <Input
              value={image}
              onChange={(e) => {
                setImage(e.target.value.trim())
                if (!name) setName(defaultName(e.target.value))
              }}
              placeholder="image name"
              className="mono"
              aria-label="Image name"
            />
          </div>
        </Card>

        {/* ---- settings ---- */}
        <Card className={cx('space-y-5 p-5', !image && 'opacity-50')}>
          <h2 className="font-display text-lg font-bold text-white">2. Settings</h2>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label className="block">
              <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Version (tag)</span>
              <input
                list="tag-options"
                value={tag}
                onChange={(e) => setTag(e.target.value.trim())}
                disabled={!image}
                className="mono w-full rounded-[2px] bg-ink-950/70 px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60"
              />
              <datalist id="tag-options">
                {tags.data?.map((t) => (
                  <option key={t.name} value={t.name} />
                ))}
              </datalist>
              <span className="mt-1.5 block text-xs text-ink-100/50">
                {tags.isFetching ? 'Loading versions…' : tags.data ? `${tags.data.length} recent versions to pick from` : 'Usually “latest” or a version number.'}
              </span>
            </label>
            <Input label="Name" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} disabled={!image} placeholder="my-app" hint="Container and stack name." />
            <label className="block">
              <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Host</span>
              <HostSelect value={hostId} onChange={setHostId} disabled={!image} className="w-full rounded-[2px] bg-ink-950/70 px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60" />
            </label>
            <label className="block">
              <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Restart</span>
              <select
                value={restart}
                onChange={(e) => setRestart(e.target.value)}
                disabled={!image}
                className="w-full rounded-[2px] bg-ink-950/70 px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60"
              >
                {RESTART.map((r) => (
                  <option key={r} value={r} className="bg-panel">
                    {r === 'unless-stopped' ? 'unless stopped (recommended)' : r === 'no' ? 'never' : r.replace('-', ' ')}
                  </option>
                ))}
              </select>
            </label>
          </div>

          <Rows
            title="Ports"
            hint="Host port: reach it at http://<server>:<host port>. Container port: what the app listens on (see the image's docs)."
            items={ports}
            setItems={setPorts}
            blank={{ host: '', container: '', proto: 'tcp' }}
            disabled={!image}
            render={(p, set) => (
              <>
                <input aria-label="Host port" placeholder="host" value={p.host} onChange={(e) => set({ ...p, host: e.target.value.trim() })} className={field + ' w-24'} />
                <span className="text-ink-100/40">→</span>
                <input aria-label="Container port" placeholder="container" value={p.container} onChange={(e) => set({ ...p, container: e.target.value.trim() })} className={field + ' w-24'} />
                <select aria-label="Protocol" value={p.proto} onChange={(e) => set({ ...p, proto: e.target.value as Port['proto'] })} className={field + ' w-20'}>
                  <option className="bg-panel">tcp</option>
                  <option className="bg-panel">udp</option>
                </select>
              </>
            )}
          />
          <Rows
            title="Environment variables"
            items={env}
            setItems={setEnv}
            blank={{ key: '', value: '' }}
            disabled={!image}
            render={(e, set) => (
              <>
                <input aria-label="Variable name" placeholder="NAME" value={e.key} onChange={(ev) => set({ ...e, key: ev.target.value.trim() })} className={field + ' w-40'} />
                <span className="text-ink-100/40">=</span>
                <input aria-label="Value" placeholder="value" value={e.value} onChange={(ev) => set({ ...e, value: ev.target.value })} className={field + ' min-w-0 flex-1'} />
              </>
            )}
          />
          <Rows
            title="Volumes"
            hint="Keep data across updates. Source: a volume name (e.g. data), or a folder: /srv/app on the host, or ./config inside the stack's folder."
            items={vols}
            setItems={setVols}
            blank={{ source: '', target: '' }}
            disabled={!image}
            render={(v, set) => (
              <>
                <input aria-label="Source" placeholder="data or /srv/app" value={v.source} onChange={(e) => set({ ...v, source: e.target.value.trim() })} className={field + ' min-w-0 flex-1'} />
                <span className="text-ink-100/40">→</span>
                <input aria-label="Path in container" placeholder="/data" value={v.target} onChange={(e) => set({ ...v, target: e.target.value.trim() })} className={field + ' min-w-0 flex-1'} />
              </>
            )}
          />

          {image && (
            <details className="group">
              <summary className="cursor-pointer select-none text-xs text-ink-100/60 hover:text-white">Show the compose file this creates</summary>
              <pre className="mono mt-2 overflow-x-auto rounded-[3px] bg-black/40 p-3 text-xs leading-relaxed text-ink-100/80 ring-1 ring-line">{compose}</pre>
            </details>
          )}
          <ErrorNote error={error} />
          {image && hostIssue && <p className="text-xs text-amber-200">{hostIssue}</p>}
          <div className="flex flex-wrap items-center gap-3">
            <Button variant="primary" icon={Rocket} onClick={submit} loading={deploy.isPending} disabled={!image}>
              Deploy
            </Button>
            <span className="text-xs text-ink-100/50">Runs as a one-service stack, so you can edit, redeploy or update it later from Stacks.</span>
          </div>
        </Card>
      </div>
    </div>
  )
}

const field = 'mono rounded-[2px] bg-ink-950/70 px-2.5 py-1.5 text-sm text-white ring-1 ring-inset ring-white/12 outline-none placeholder:text-ink-100/30 focus:ring-lime-400/60 disabled:opacity-50'

function Rows<T>({
  title,
  hint,
  items,
  setItems,
  blank,
  render,
  disabled,
}: {
  title: string
  hint?: string
  items: T[]
  setItems: (f: (old: T[]) => T[]) => void
  blank: T
  render: (item: T, set: (v: T) => void) => React.ReactNode
  disabled?: boolean
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
    </fieldset>
  )
}
