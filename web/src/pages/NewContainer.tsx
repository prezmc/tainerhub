import { useCallback, useMemo, useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, ArrowLeft, BadgeCheck, ClipboardPaste, Download, Rocket, Search, Server, Star, Undo2 } from 'lucide-react'
import { api, errorText, stackProblem, type NetworkRes, type StackSaveResult } from '../api'
import { Badge, Button, Card, ErrorNote, Input, PageHeader, cx, useConfirm, useToast } from '../ui'
import { useFleet } from '../fleet'
import { useOpenJob, useStacks } from '../stacks'
import { HostSelect, useDefaultHost } from '../placement'
import { allProblems, blankAdvanced, composeFor, isUserNetwork, setCounts, type Advanced, type Env, type Port, type Section, type Vol } from '../compose'
import { AdvancedOptions, ComposePreview, FieldError, Rows, field } from './ContainerOptions'
import { ComposeImportDialog, ImportedNotice } from './ComposeImport'
import type { Change, Imported, Note } from '../composeimport'
import { setStackDraft, type StackDraft } from '../stackdraft'
import { PreflightPanel, useFreshPreflight, usePreflight, type PreflightFix } from '../preflight'
import { generateSecret } from '../placeholder'
import { ShareBadge, useShareVolumes } from '../sharesui'
import { isUtcLike, localTimeZone, portConflicts, usedHostPorts } from '../recommend'

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
  // The name follows the image until the user types their own.
  const [nameEdited, setNameEdited] = useState(false)
  const [hostId, setHostId] = useState('')
  const [restart, setRestart] = useState<string>('unless-stopped')
  const [ports, setPorts] = useState<Port[]>([{ host: '', container: '', proto: 'tcp' }])
  const [env, setEnv] = useState<Env[]>([])
  const [vols, setVols] = useState<Vol[]>([])
  const [adv, setAdv] = useState<Advanced>(blankAdvanced)
  const [openSections, setOpenSections] = useState<Set<Section>>(() => new Set())
  const [error, setError] = useState<string | null>(null)
  const [tried, setTried] = useState(false) // basic fields show their messages after the first Deploy
  const [pasting, setPasting] = useState(false)
  const [imported, setImported] = useState<{ service: string; notes: Note[]; changes: Change[] } | null>(null)

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
    setNameEdited(false)
    setError(null)
  }

  // "Paste compose": the form's own fields, filled from one service.
  const applyImport = (r: Imported) => {
    setLastFix(null)
    setImage(r.image)
    setTag(r.tag)
    setName(r.name)
    setNameEdited(true)
    setRestart(r.restart)
    setPorts(r.ports.length ? r.ports : [{ host: '', container: '', proto: 'tcp' }])
    setEnv(r.env)
    setVols(r.vols)
    setAdv(r.adv)
    setOpenSections(new Set(r.open))
    setImported({ service: r.service, notes: r.notes, changes: r.changes.filter((c) => !c.id.startsWith('var:') || c.to !== '') })
    setTried(false)
    setError(null)
    setPasting(false)
  }
  const deployAsStack = (d: StackDraft) => {
    setStackDraft(d)
    setPasting(false)
    navigate({ to: '/stacks/new', search: { host: hostId || undefined } })
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
  const host = hosts.find((h) => h.id === hostId)
  // Network share volumes on the host: a container mounts the existing
  // volume (external) rather than the stack making its own empty one.
  const shares = useShareVolumes(hostId, !!image && !!host?.online)
  const shareNames = shares.map((s) => s.name)
  const spec = { name, image: fullImage, restart, ports, env, vols, adv, externalVols: shareNames }
  const compose = useMemo(() => (image ? composeFor(spec) : ''), [image, tag, name, restart, ports, env, vols, adv, shareNames.join()]) // eslint-disable-line react-hooks/exhaustive-deps
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

  // Host ports in use on the chosen host, and clashes with the form's, shown as you type.
  const usedPorts = useMemo(() => usedHostPorts(containers, hostId), [containers, hostId])
  const clashes = adv.networkMode ? [] : portConflicts(ports, usedPorts)
  const myZone = localTimeZone()
  const utcTZ = env.find((e) => e.key === 'TZ' && isUtcLike(e.value))

  // The check against the chosen host: it runs once the form itself is valid.
  const preflightInput = { hostId, name, compose, env: adv.envFile }
  const formOK = issues.length === 0
  const canCheck = !!image && !!host?.online && !hostIssue && formOK
  const preflight = usePreflight(preflightInput, canCheck)
  const freshPreflight = useFreshPreflight()
  const preflightWaiting = !host ? 'Pick a host to check the settings against it.' : !host.online ? `${host.name} is offline, so nothing can be checked.` : hostIssue ? hostIssue : !formOK ? `It checks the settings against ${host.name} once they're complete. First: ${issues[0].msg}` : null

  // The last fix applied from the check, with the form as it was, so it can be undone.
  const [lastFix, setLastFix] = useState<{ label: string; undo: () => void } | null>(null)
  const applyFix = (f: PreflightFix) => {
    const before = { image, tag, name, nameEdited, ports, env, vols, adv }
    setLastFix({
      label: f.label,
      undo: () => {
        setImage(before.image)
        setTag(before.tag)
        setName(before.name)
        setNameEdited(before.nameEdited)
        setPorts(before.ports)
        setEnv(before.env)
        setVols(before.vols)
        setAdv(before.adv)
        setLastFix(null)
      },
    })
    switch (f.kind) {
      case 'port': {
        const [num, proto] = (f.target ?? '').split('/')
        setPorts((old) => old.map((p) => (p.host === num && p.proto === proto ? { ...p, host: f.value ?? '' } : p)))
        break
      }
      case 'name':
        setName(f.value ?? '')
        setNameEdited(true)
        break
      case 'cpus':
        patchAdv({ cpus: f.value ?? '' })
        break
      case 'memory':
        patchAdv({ memory: f.value ?? '', memoryUnit: 'm' })
        break
      case 'gpu-off':
        patchAdv({ gpu: false })
        break
      case 'network-create':
        patchAdv({ networks: adv.networks.filter((n) => n !== f.target), newNetwork: f.target ?? '' })
        setOpenSections((old) => new Set([...old, 'network']))
        break
      case 'network-drop':
        patchAdv({ networks: adv.networks.filter((n) => n !== f.target) })
        break
      case 'volume-source':
        setVols((old) => old.map((v) => (v.target.split(':')[0] === f.target ? { ...v, source: f.value ?? '' } : v)))
        break
      case 'env-generate':
        setEnv((old) => old.map((e) => (e.key === f.target ? { ...e, value: generateSecret() } : e)))
        break
      case 'tag':
        if (typedTag) setImage(image.includes('@') ? image.split('@')[0] : image.slice(0, image.lastIndexOf(':')))
        setTag(f.value ?? 'latest')
        break
    }
  }
  // A fix the form can't make right now (it creates one network at most).
  const canFix = (f: PreflightFix) => f.kind !== 'network-create' || !adv.newNetwork || adv.newNetwork === f.target

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
    if (host?.online) {
      try {
        const rep = await freshPreflight(preflightInput)
        const blocks = rep.items.filter((i) => i.level === 'block')
        if (blocks.length) return setError(blocks.length === 1 ? `Fix this first: ${blocks[0].title}.` : `Fix these first: ${blocks.map((b) => b.title).join('; ')}.`)
      } catch {
        // The check itself failed (an old or busy agent): the deploy will say what's wrong, if anything.
      }
    }
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
      <PageHeader title="New container" subtitle="Find an image on Docker Hub, set it up, and run it on one of your hosts.">
        <Button variant="secondary" icon={ClipboardPaste} onClick={() => setPasting(true)}>
          Paste compose
        </Button>
      </PageHeader>
      <ComposeImportDialog
        open={pasting}
        onClose={() => setPasting(false)}
        onApply={applyImport}
        onStack={deployAsStack}
        replacing={!!image}
        hostId={hostId}
        hostName={hosts.find((h) => h.id === hostId)?.name ?? ''}
        hostOnline={!!hosts.find((h) => h.id === hostId)?.online}
        usedPorts={usedPorts}
      />

      {/* ---- host: first, since the checks and folder suggestions depend on it ---- */}
      <Card className="mb-6 flex flex-col gap-3 p-5 sm:flex-row sm:items-start sm:gap-6">
        <div className="shrink-0 sm:w-64">
          <h2 className="flex items-center gap-2 font-display text-lg font-bold text-white">
            <Server className="size-4 text-lime-300" />
            Host
          </h2>
          <p className="mt-1 text-xs text-ink-100/50">Where the container will run. The checks and folder suggestions below are for this host.</p>
        </div>
        <label className="block min-w-0 flex-1 sm:max-w-2xl">
          <span className="sr-only">Host to install on</span>
          <HostSelect value={hostId} onChange={changeHost} className="w-full rounded-[2px] bg-ink-950/70 px-3 py-2.5 text-sm font-medium text-white ring-1 ring-inset ring-lime-400/30 outline-none focus:ring-lime-400/60" />
        </label>
      </Card>

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
            Have a compose file from the project’s docs?{' '}
            <button type="button" onClick={() => setPasting(true)} className="text-lime-300 underline-offset-2 hover:underline">
              Paste it
            </button>{' '}
            to fill in everything below.
          </p>
          <p className="mt-2 text-xs text-ink-100/50">
            Not on Docker Hub? Type the full name instead, like <code className="mono text-lime-300">ghcr.io/home-assistant/home-assistant</code>:
          </p>
          <div className="mt-2">
            <Input
              value={image}
              onChange={(e) => {
                setImage(e.target.value.trim())
                if (!nameEdited) setName(defaultName(e.target.value))
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
          {imported && <ImportedNotice service={imported.service} notes={imported.notes} changes={imported.changes} onDismiss={() => setImported(null)} />}
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label className="block">
              <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Version (tag)</span>
              <input
                list="tag-options"
                value={typedTag ? image.slice(Math.max(image.lastIndexOf(':'), image.lastIndexOf('@')) + 1) : tag}
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
              <Input label="Name" value={name} onChange={(e) => {
                setName(e.target.value.toLowerCase())
                setNameEdited(true)
              }} disabled={!image} placeholder="my-app" hint="Container and stack name." />
              <FieldError msgs={basicErr('name')} />
            </div>
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
          {image && clashes.length > 0 && (
            <ul className="-mt-3 space-y-1 text-xs" aria-label="Port conflicts">
              {clashes.map((c) => (
                <li key={c.index} className="flex flex-wrap items-center gap-x-2 gap-y-1 text-amber-200">
                  <AlertTriangle className="size-3.5 shrink-0" />
                  <span>
                    Port {c.key} is already used by {c.by}
                    {c.by !== 'another port in this form' && host ? ` on ${host.name}` : ''}.
                  </span>
                  {c.suggestion && (
                    <button
                      type="button"
                      className="text-lime-300 underline-offset-2 hover:underline"
                      onClick={() => setPorts((old) => old.map((p, i) => (i === c.index ? { ...p, host: c.suggestion } : p)))}
                    >
                      Use {c.suggestion}
                    </button>
                  )}
                </li>
              ))}
            </ul>
          )}
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
          {image && utcTZ && myZone && !isUtcLike(myZone) && (
            <p className="-mt-3 flex flex-wrap items-center gap-x-2 text-xs text-ink-100/60">
              <span>
                TZ is {utcTZ.value}, the usual example value. This computer's time zone is {myZone}.
              </span>
              <button type="button" className="text-lime-300 underline-offset-2 hover:underline" onClick={() => setEnv((old) => old.map((e) => (e.key === 'TZ' ? { ...e, value: myZone } : e)))}>
                Use {myZone}
              </button>
            </p>
          )}
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
                <input aria-label="Source" placeholder="data or /srv/app" list="share-volumes" value={v.source} onChange={(e) => set({ ...v, source: e.target.value.trim() })} className={field + ' min-w-0 flex-1'} />
                <span className="text-ink-100/40">→</span>
                <input aria-label="Path in container" placeholder="/data" value={v.target} onChange={(e) => set({ ...v, target: e.target.value.trim() })} className={field + ' min-w-0 flex-1'} />
              </>
            )}
          />

          {image && shares.length > 0 && (
            <div className="-mt-3 flex flex-wrap items-center gap-2 text-xs text-ink-100/60">
              <span>Network shares on {host?.name}:</span>
              {shares.map((sv) => (
                <button
                  key={sv.name}
                  type="button"
                  disabled={vols.some((v) => v.source === sv.name)}
                  title={`Mount ${sv.name} in this container`}
                  className="inline-flex items-center gap-1.5 rounded-[2px] px-2 py-1 ring-1 ring-inset ring-line transition hover:ring-lime-400/50 disabled:opacity-40"
                  onClick={() =>
                    setVols((old) => {
                      const empty = old.findIndex((v) => !v.source && !v.target)
                      const row = { source: sv.name, target: old.some((v) => v.target === '/data') ? '' : '/data' }
                      return empty >= 0 ? old.map((v, i) => (i === empty ? row : v)) : [...old, row]
                    })
                  }
                >
                  <span className="mono text-white">{sv.name}</span>
                  <ShareBadge share={sv.share} />
                </button>
              ))}
              <datalist id="share-volumes">
                {shares.map((sv) => (
                  <option key={sv.name} value={sv.name} />
                ))}
              </datalist>
            </div>
          )}
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
          {image && <PreflightPanel check={preflight} hostName={host?.name ?? ''} onFix={applyFix} canFix={canFix} waiting={preflightWaiting} />}
          {image && lastFix && (
            <p className="-mt-3 flex flex-wrap items-center gap-2 text-xs text-ink-100/70" role="status">
              <span>
                Changed in the form: <strong className="font-medium text-white">{lastFix.label}</strong>.
              </span>
              <Button type="button" variant="ghost" icon={Undo2} className="!px-2 !py-1 text-[11px]" onClick={lastFix.undo}>
                Undo
              </Button>
              <button type="button" className="text-ink-100/45 hover:text-white" onClick={() => setLastFix(null)}>
                Keep it
              </button>
            </p>
          )}
          <ErrorNote error={error} />
          {image && hostIssue && <p className="text-xs text-amber-200">{hostIssue}</p>}
          <div className="flex flex-wrap items-center gap-3">
            <Button variant="primary" icon={Rocket} onClick={submit} loading={deploy.isPending} disabled={!image}>
              {host ? `Deploy to ${host.name}` : 'Deploy'}
            </Button>
            <span className="text-xs text-ink-100/50">Runs as a one-service stack, so you can edit, redeploy or update it later from Stacks.</span>
          </div>
        </Card>
      </div>
    </div>
  )
}
