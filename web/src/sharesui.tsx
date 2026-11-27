import { useId, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { FolderSymlink, PlugZap } from 'lucide-react'
import { api, errorText, type Host, type VolumeRes } from './api'
import { Badge, Button, ErrorNote, Input, Modal, cx, useToast } from './ui'
import { CIFS_VERSIONS, NFS_VERSIONS, blankShare, parseShareAddress, shareBody, shareProblem, shareText, type ShareForm, type ShareInfo, type ShareType } from './shares'

/** "SMB //nas/media" next to a volume that mounts a network share. */
export function ShareBadge({ share }: { share: ShareInfo }) {
  return (
    <Badge tone="info" className="max-w-[16rem] !normal-case !tracking-normal">
      <FolderSymlink className="size-3 shrink-0" />
      <span className="truncate" title={shareText(share)}>
        {shareText(share)}
      </span>
    </Badge>
  )
}

/** Share volumes on a host, for pickers. */
export function useShareVolumes(hostId: string, enabled: boolean) {
  const q = useQuery({
    queryKey: ['res', 'volumes', hostId],
    queryFn: () => api<VolumeRes[]>(`/api/hosts/${hostId}/volumes`),
    enabled: enabled && !!hostId,
    staleTime: 30_000,
    retry: false,
  })
  return (q.data ?? []).filter((v): v is VolumeRes & { share: ShareInfo } => !!v.share)
}

const select = 'w-full rounded-[2px] bg-ink-950/70 px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60'

/**
 * Resources → Volumes → New network share. The agent creates the volume,
 * mounts it once to check, and removes it again if that fails.
 */
export function NewShareModal({ open, onClose, hosts, defaultHost }: { open: boolean; onClose: () => void; hosts: Host[]; defaultHost?: string }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [f, setF] = useState<ShareForm>(() => blankShare())
  const [hostId, setHostId] = useState<string | undefined>(undefined)
  const [error, setError] = useState<string | null>(null)
  const [tried, setTried] = useState(false)
  const target = hostId ?? defaultHost ?? hosts[0]?.id
  const host = hosts.find((h) => h.id === target)
  const set = (p: Partial<ShareForm>) => setF((old) => ({ ...old, ...p }))
  const setType = (type: ShareType) => setF((old) => ({ ...old, type, version: type === 'cifs' ? '3.0' : '4', path: old.type === type ? old.path : '' }))
  // Pasting a whole address (\\nas\media, smb://…, nas:/export) into
  // either field fills in the server, the share and the right tab.
  const typed = (field: 'server' | 'path', v: string) => {
    const a = parseShareAddress(v)
    if (a) setF((old) => ({ ...old, type: a.type, server: a.server, path: a.path, version: a.type === old.type ? old.version : a.type === 'cifs' ? '3.0' : '4' }))
    else set(field === 'server' ? { server: v.trim() } : { path: v })
  }
  const problem = shareProblem(f)
  const close = () => {
    setF(blankShare())
    setError(null)
    setTried(false)
    onClose()
  }

  const create = useMutation({
    mutationFn: () => api<{ name: string; entries: number }>(`/api/hosts/${target}/volumes/share`, { method: 'POST', body: shareBody(f) }),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ['res', 'volumes', target] })
      toast('good', `${host?.name}: volume ${r.name} mounts ${shareText({ type: f.type, server: f.server.trim(), path: f.path.trim() })} (${r.entries} ${r.entries === 1 ? 'item' : 'items'} at the top)`)
      close()
    },
    onError: (e) => setError(errorText(e)),
  })

  return (
    <Modal open={open} onClose={close} title="New network share" wide>
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault()
          setTried(true)
          setError(null)
          if (!host) return setError('Pick a host')
          if (problem) return
          create.mutate()
        }}
      >
        <p className="text-sm leading-relaxed text-ink-100/70">
          Creates a Docker volume on the host that mounts a share from your NAS whenever a container uses it. Several containers can use the same volume, so they all see the same files at, say, <span className="mono">/data</span>. TainerHub mounts it once to check before keeping it.
        </p>
        <div>
          <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Host</span>
          <div className="flex flex-wrap gap-2">
            {hosts.map((h) => (
              <button
                type="button"
                key={h.id}
                onClick={() => setHostId(h.id)}
                className={cx('rounded-[2px] px-3.5 py-1.5 text-sm font-semibold transition', target === h.id ? 'bg-lime-400 text-ink-950' : 'text-ink-100/75 ring-1 ring-inset ring-line hover:ring-lime-400/40')}
              >
                {h.name}
              </button>
            ))}
          </div>
        </div>
        <div className="flex gap-1 rounded-[3px] bg-ink-950/60 p-0.5 ring-1 ring-line" role="tablist">
          {(
            [
              ['cifs', 'SMB / CIFS', 'Windows shares, Synology, QNAP, TrueNAS, Unraid'],
              ['nfs', 'NFS', 'Unix exports'],
            ] as const
          ).map(([t, label, sub]) => (
            <button
              key={t}
              type="button"
              role="tab"
              aria-selected={f.type === t}
              onClick={() => setType(t)}
              className={cx('flex-1 rounded-[2px] px-3 py-1.5 text-left transition', f.type === t ? 'bg-lime-400 text-ink-950' : 'text-ink-100/70 hover:text-white')}
            >
              <span className="block font-display text-[12px] font-semibold uppercase tracking-[0.08em]">{label}</span>
              <span className={cx('block text-[11px]', f.type === t ? 'text-ink-950/70' : 'text-ink-100/45')}>{sub}</span>
            </button>
          ))}
        </div>

        <div className="grid gap-3 sm:grid-cols-2">
          <Input
            label="Server"
            placeholder="192.168.1.10 or nas.lan"
            value={f.server}
            onChange={(e) => typed('server', e.target.value)}
            className="mono"
            autoFocus
            hint="IP address or host name (an IP address is safest). Or paste the whole address, like \\192.168.1.10\media."
          />
          {f.type === 'cifs' ? (
            <PrefixInput
              label="Share name"
              prefix={`//${f.server || 'server'}/`}
              placeholder="media"
              value={f.path}
              onChange={(v) => typed('path', v)}
              hint="Just the name the NAS gives the share. A folder in it works too: media/movies"
            />
          ) : (
            <PrefixInput label="Export path" prefix={`${f.server || 'server'}:`} placeholder="/volume1/media" value={f.path} onChange={(v) => typed('path', v.trim())} hint="The exported folder on the server." />
          )}
        </div>

        {f.type === 'cifs' && (
          <>
            <div className="grid gap-3 sm:grid-cols-2">
              <Input label="Username" value={f.username} onChange={(e) => set({ username: e.target.value.trim() })} autoComplete="off" hint="Empty: connect as guest." />
              <Input label="Password" type="password" value={f.password} onChange={(e) => set({ password: e.target.value })} autoComplete="new-password" hint="Kept by Docker on the host only, not by TainerHub." />
            </div>
            <div className="grid gap-3 sm:grid-cols-4">
              <Input label="Files owned by UID" value={f.uid} onChange={(e) => set({ uid: e.target.value.trim() })} className="mono" placeholder="1000" />
              <Input label="GID" value={f.gid} onChange={(e) => set({ gid: e.target.value.trim() })} className="mono" placeholder="1000" />
              <Input label="File mode" value={f.fileMode} onChange={(e) => set({ fileMode: e.target.value.trim() })} className="mono" placeholder="0644" />
              <Input label="Folder mode" value={f.dirMode} onChange={(e) => set({ dirMode: e.target.value.trim() })} className="mono" placeholder="0755" />
            </div>
            <p className="-mt-1 text-xs text-ink-100/55">
              The share's files appear to belong to this UID and GID inside containers. Match the PUID and PGID your containers run as (1000 for most linuxserver.io images), or they can't write.
            </p>
          </>
        )}

        <div className="grid gap-3 sm:grid-cols-2">
          <label className="block">
            <span className="mb-1.5 block text-sm font-medium text-ink-100/80">{f.type === 'cifs' ? 'SMB version' : 'NFS version'}</span>
            <select value={f.version} onChange={(e) => set({ version: e.target.value })} className={select}>
              {(f.type === 'cifs' ? CIFS_VERSIONS : NFS_VERSIONS).map((v) => (
                <option key={v} value={v} className="bg-panel">
                  {v === 'default' ? 'let the host pick' : v}
                </option>
              ))}
            </select>
          </label>
          {f.type === 'cifs' ? <Input label="Domain (optional)" value={f.domain} onChange={(e) => set({ domain: e.target.value.trim() })} placeholder="WORKGROUP" /> : <div />}
        </div>
        <label className="flex items-center gap-2 text-sm text-ink-100/80">
          <input type="checkbox" checked={f.readOnly} onChange={(e) => set({ readOnly: e.target.checked })} className="accent-lime-400" /> Read-only
        </label>
        <Input label="Volume name" placeholder="media" value={f.name} onChange={(e) => set({ name: e.target.value.trim() })} className="mono" hint={`What containers and compose files call it: volumes: - ${f.name || 'media'}:/data`} />

        {tried && problem && <ErrorNote error={problem} />}
        <ErrorNote error={error} />
        <p className="text-xs leading-relaxed text-ink-100/50">
          The host needs {f.type === 'cifs' ? <span className="mono">cifs-utils</span> : <span className="mono">nfs-common</span>} installed. Docker keeps the volume's options, including the password, in plain text on the host: anyone who can run <span className="mono">docker volume inspect</span> there can read them.
        </p>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button variant="primary" icon={PlugZap} loading={create.isPending}>
            {create.isPending ? 'Mounting to check…' : 'Test and create'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

/** A text field with a fixed, greyed start (the part already filled in above). */
function PrefixInput({ label, prefix, hint, value, onChange, placeholder }: { label: string; prefix: string; hint: string; value: string; onChange: (v: string) => void; placeholder: string }) {
  const id = useId()
  return (
    <label className="block" htmlFor={id}>
      <span className="mb-1.5 block text-sm font-medium text-ink-100/80">{label}</span>
      <span className="mono flex items-center rounded-[2px] bg-ink-950/70 text-sm ring-1 ring-inset ring-white/12 transition focus-within:shadow-[inset_3px_0_0] focus-within:shadow-lime-400 focus-within:ring-lime-400/60">
        <span className="max-w-[55%] shrink-0 truncate py-2 pl-3 text-ink-100/45" title={prefix} aria-hidden>
          {prefix}
        </span>
        <input id={id} value={value} onChange={(e) => onChange(e.target.value)} placeholder={placeholder} aria-describedby={`${id}-hint`} className="min-w-0 flex-1 bg-transparent py-2 pr-3 text-white outline-none placeholder:text-ink-100/35" />
      </span>
      <span className="mt-1.5 block text-xs text-ink-100/50" id={`${id}-hint`}>
        {hint}
      </span>
    </label>
  )
}
