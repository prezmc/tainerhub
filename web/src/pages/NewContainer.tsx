import { useCallback, useMemo, useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowLeft, BadgeCheck, Download, Rocket, Search, Star } from 'lucide-react'
import { api, errorText, stackProblem, type NetworkRes, type StackSaveResult } from '../api'
import { Badge, Button, Card, ErrorNote, Input, PageHeader, cx, useConfirm, useToast } from '../ui'
import { useFleet } from '../fleet'
import { useOpenJob, useStacks } from '../stacks'
import { HostSelect, useDefaultHost } from '../placement'
import { allProblems, blankAdvanced, composeFor, isUserNetwork, setCounts, type Advanced, type Env, type Port, type Section, type Vol } from '../compose'
import { AdvancedOptions, ComposePreview, FieldError, Rows, field } from './ContainerOptions'

type Repo = { name: string; description: string; stars: number; pulls: number; official: boolean }
type Tag = { name: string; updated: string; size: number }

const RESTART = ['unless-stopped', 'always', 'on-failure', 'no'] as const

const short = (n: number) => (n >= 1e9 ? `${(n / 1e9).toFixed(1)}B` : n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${Math.round(n / 1e3)}k` : String(n))

/** "linuxserver/plex" -> "plex"; made into a valid stack/container name. */
function defaultName(image: string) {
  const last = image.split('/').pop()?.split(':')[0] ?? ''
  return last.toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^[-_]+/, '').slice(0, 63)
}

export function NewContainerPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
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
  const [adv, setAdv] = useState<Advanced>(blankAdvanced)
  const [openSections, setOpenSections] = useState<Set<Section>>(() => new Set())
  const [error, setError] = useState<string | null>(null)
  const [tried, setTried] = useState(false) // basic fields show their messages after the first Deploy

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
    enabled: isHubImage && !/[:@]/.test(image), // no tag lookup once a tag is typed
    staleTime: 5 * 60_000,
    retry: false,
  })

  const pick = (repo: string) => {
    setImage(repo)
    setTag('latest')
    setName(defaultName(repo))
    setError(null)
  }

  const patchAdv = useCallback((patch: Partial<Advanced>) => setAdv((old) => ({ ...old, ...patch })), [])
  const toggleSection = useCallback(
    (id: Section) =>
      setOpenSections((old) => {
        const next = new Set(old)
        if (!next.delete(id)) next.add(id)
        return next
      }),
    [],
  )
  const changeHost = (id: string) => {
    setHostId(id)
    // networks belong to a host: a choice on the old one means nothing on the new one
    if (id !== hostId) patchAdv({ networks: [] })
  }

  // A tag or digest typed into the full name wins over the Version field, so
  // "nginx:1.27-alpine" doesn't become "nginx:1.27-alpine:latest".
  const typedTag = image.includes('@') || (image.split('/').pop() ?? '').includes(':')
  const fullImage = image ? (typedTag ? image : `${image}:${tag || 'latest'}`) : ''
  const spec = { name, image: fullImage, restart, ports, env, vols, adv }
  const compose = useMemo(() => (image ? composeFor(spec) : ''), [image, tag, name, restart, ports, env, vols, adv]) // eslint-disable-line react-hooks/exhaustive-deps
  const host = hosts.find((h) => h.id === hostId)
  const networks = useQuery({
    queryKey: ['host-networks', hostId],
    queryFn: () => api<NetworkRes[]>(`/api/hosts/${hostId}/networks`),
    enabled: !!image && !!hostId && !!host?.online,
    staleTime: 30_000,
    retry: false,
  })
  const allNetworks = useMemo(() => (networks.data ?? []).map((n) => n.name), [networks.data])
  const userNetworks = useMemo(
    () => (networks.data ?? []).filter((n) => !n.builtin && isUserNetwork(n.name) && n.name !== 'ingress' && n.name !== 'docker_gwbridge').map((n) => n.name),
    [networks.data],
  )
  const issues = useMemo(() => allProblems(spec, allNetworks), [image, tag, name, restart, ports, env, vols, adv, allNetworks]) // eslint-disable-line react-hooks/exhaustive-deps
  // Advanced fields are checked as you type; the basic ones on Deploy, as before.
  const advIssues = issues.filter((p) => p.section !== 'basic')
  const basicErr = (f: string) => (tried ? issues.filter((p) => p.field === f).map((p) => p.msg) : [])
  const hostIssue = stackProblem(host)
  const clash =
    containers.find((c) => c.hostId === hostId && c.name === name) || stacks.data?.find((s) => s.hostId === hostId && s.name === name && s.managed)
      ? `${host?.name ?? 'This host'} already has a container or stack called ${name}.`
      : null

  const deploy = useMutation({
    mutationFn: () => api<StackSaveResult>('/api/stacks', { method: 'POST', body: { hostId, name, compose, env: adv.envFile, deploy: true } }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['stacks'] })
      if (res.deployError) toast('bad', `Saved, but the deploy didn't start: ${res.deployError}`)
      if (res.jobId) openJob(res.jobId)
      navigate({ to: '/stacks/$stackId', params: { stackId: res.stack.id } })
    },
    onError: (e) => setError(errorText(e)),
  })

  const submit = async () => {
    const all = issues.map((p) => p.msg)
    setTried(true)
    if (issues.length) {
      // open every section that needs attention
      setOpenSections((old) => new Set([...old, ...issues.map((p) => p.section).filter((s) => s !== 'basic')]))
      return setError(all.length === 1 ? all[0] : `${all.length} things to fix: ${[...new Set(all)].join(' ')}`)
    }
    const p = hostIssue ?? clash
    if (p) return setError(p)
    setError(null)
    if (
      adv.privileged &&
      !(await confirm({
        title: 'Deploy a privileged container?',
        body: (
          <>
            <strong className="text-red-200">{name}</strong> will run with full root access to {host?.name ?? 'the host'}: every capability and every device. Anyone who controls this
            container controls the host.
          </>
        ),
        confirm: 'Deploy privileged',
        danger: true,
      }))
    )
      return
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
                disabled={!image || typedTag}
                className="mono w-full rounded-[2px] bg-ink-950/70 px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60"
              />
              <datalist id="tag-options">
                {tags.data?.map((t) => (
                  <option key={t.name} value={t.name} />
                ))}
              </datalist>
              <span className="mt-1.5 block text-xs text-ink-100/50">
                {typedTag
                  ? 'Taken from the image name you typed.'
                  : tags.isFetching
                    ? 'Loading versions…'
                    : tags.data
                      ? `${tags.data.length} recent versions to pick from`
                      : 'Usually “latest” or a version number.'}
              </span>
            </label>
            <div>
              <Input label="Name" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} disabled={!image} placeholder="my-app" hint="Container and stack name." />
              <FieldError msgs={basicErr('name')} />
            </div>
            <label className="block">
              <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Host</span>
              <HostSelect value={hostId} onChange={changeHost} disabled={!image} className="w-full rounded-[2px] bg-ink-950/70 px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60" />
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
            hint={
              adv.networkMode === 'host'
                ? "Off with host networking (see Advanced → Networking): the app listens on the host's ports directly, so there's nothing to map."
                : adv.networkMode === 'none'
                  ? 'Off: the container has no network (see Advanced → Networking).'
                  : "Host port: reach it at http://<server>:<host port>. Container port: what the app listens on (see the image's docs)."
            }
            items={ports}
            setItems={setPorts}
            blank={{ host: '', container: '', proto: 'tcp' }}
            disabled={!image || adv.networkMode !== ''}
            error={basicErr('ports')}
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
            error={basicErr('env')}
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
            error={basicErr('vols')}
            render={(v, set) => (
              <>
                <input aria-label="Source" placeholder="data or /srv/app" value={v.source} onChange={(e) => set({ ...v, source: e.target.value.trim() })} className={field + ' min-w-0 flex-1'} />
                <span className="text-ink-100/40">→</span>
                <input aria-label="Path in container" placeholder="/data" value={v.target} onChange={(e) => set({ ...v, target: e.target.value.trim() })} className={field + ' min-w-0 flex-1'} />
              </>
            )}
          />

          {image && (
            <AdvancedOptions
              adv={adv}
              set={patchAdv}
              open={openSections}
              onToggle={toggleSection}
              problems={advIssues}
              counts={setCounts(adv)}
              networks={userNetworks}
              networksLoading={networks.isPending && networks.fetchStatus !== 'idle'}
              networksError={!host?.online ? 'the host is offline' : networks.error ? errorText(networks.error) : null}
              envHasTZ={env.some((e) => e.key === 'TZ')}
            />
          )}
          {image && <ComposePreview compose={compose} />}
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
