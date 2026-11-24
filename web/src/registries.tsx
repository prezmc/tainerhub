import { useEffect, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound, Pencil, Plus, ShieldCheck, Trash2 } from 'lucide-react'
import { api, errorText, isRegistryAuthError, needsReauth, timeAgo } from './api'
import { useHostsQuery } from './fleet'
import { Badge, Button, Card, ErrorNote, IconButton, Input, Modal, useConfirm, useToast } from './ui'

export interface RegistryLogin {
  id: string
  registry: string
  username: string
  /** The password itself never leaves the hub. */
  passwordSet: boolean
  createdBy?: string
  createdAt: string
  updatedAt: string
  lastUsedAt: string | null
}

type TestResult = { ok: boolean; host: string; status?: string; error?: string }

/** Shown under a failed pull whose error looks like a missing or wrong registry login. */
export function RegistryAuthHint({ error }: { error?: string | null }) {
  if (!isRegistryAuthError(error)) return null
  return (
    <div className="flex items-start gap-2 rounded-[3px] bg-amber-500/10 px-3 py-2.5 text-sm text-amber-200 ring-1 ring-inset ring-amber-500/25">
      <KeyRound className="mt-0.5 size-4 shrink-0" />
      <span>
        Private images need a registry login: an admin can add one under{' '}
        <Link to="/settings" hash="registries" className="font-semibold underline underline-offset-2">
          Settings → Container registries
        </Link>{' '}
        (a Docker Hub login also lifts its pull rate limits).
      </span>
    </div>
  )
}

// Settings → Container registries: logins for private registries. The hub
// keeps passwords encrypted and only ever shows that one is set; it sends a
// login to a host only inside a pull from that registry.
export function RegistriesCard() {
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const list = useQuery({ queryKey: ['registries'], queryFn: () => api<RegistryLogin[]>('/api/registries') })
  const [editing, setEditing] = useState<RegistryLogin | 'new' | null>(null)
  const [tests, setTests] = useState<Partial<Record<string, TestResult>>>({})
  const loaded = !!list.data
  useEffect(() => {
    // Links from failed pulls land here: the card loads after the page does.
    if (loaded && window.location.hash === '#registries') document.getElementById('registries')?.scrollIntoView({ block: 'start' })
  }, [loaded])

  const test = useMutation({
    mutationFn: (r: RegistryLogin) => api<TestResult>(`/api/registries/${r.id}/test`, { method: 'POST', body: {} }),
    onMutate: (r) => setTests((t) => ({ ...t, [r.id]: undefined })),
    onSuccess: (res, r) => setTests((t) => ({ ...t, [r.id]: res })),
    onError: (e) => toast('bad', errorText(e)),
  })
  const remove = useMutation({
    mutationFn: (r: RegistryLogin) => api(`/api/registries/${r.id}`, { method: 'DELETE' }),
    onSuccess: (_, r) => {
      toast('good', `Removed the login for ${r.registry}`)
      qc.invalidateQueries({ queryKey: ['registries'] })
    },
    // A stale sign-in already opened the "Sign in again" dialog.
    onError: (e) => !needsReauth(e) && toast('bad', errorText(e)),
  })

  const rows = list.data ?? []
  return (
    <Card className="px-5 py-4">
      <div id="registries" className="mb-3 flex flex-wrap items-center gap-3 scroll-mt-24">
        <KeyRound className="size-5 text-lime-300" />
        <div className="min-w-0 flex-1">
          <h2 className="font-display text-lg font-semibold text-white">Container registries</h2>
          <p className="text-sm text-ink-100/60">
            Logins for private images (GHCR, GitLab, your own registry). A Docker Hub login also lifts its anonymous pull rate limits. Passwords are stored
            encrypted and never shown again; hosts get a login only for the pull that needs it.
          </p>
        </div>
        <Button variant="primary" icon={Plus} onClick={() => setEditing('new')}>
          Add login
        </Button>
      </div>
      <ErrorNote error={list.error ? errorText(list.error) : null} />
      {rows.length === 0 ? (
        !list.isPending && <p className="py-2 text-sm text-ink-100/50">No registry logins yet: only public images can be pulled.</p>
      ) : (
        <div className="divide-y divide-line rounded-[3px] ring-1 ring-line">
          {rows.map((r) => {
            const t = tests[r.id]
            return (
              <div key={r.id} className="px-3 py-2.5">
                <div className="flex flex-wrap items-center gap-3">
                  <span className="min-w-0 flex-1">
                    <span className="block truncate font-mono text-[13px] text-white">{r.registry}</span>
                    <span className="block text-xs text-ink-100/55">
                      {r.username} · password {r.passwordSet ? '•••• set' : 'missing'} · {r.lastUsedAt ? `last used ${timeAgo(r.lastUsedAt)}` : 'not used yet'}
                    </span>
                  </span>
                  <Button icon={ShieldCheck} loading={test.isPending && test.variables?.id === r.id} onClick={() => test.mutate(r)}>
                    Test
                  </Button>
                  <IconButton icon={Pencil} label={`Edit ${r.registry}`} onClick={() => setEditing(r)} />
                  <IconButton
                    icon={Trash2}
                    label={`Remove ${r.registry}`}
                    onClick={async () => {
                      if (
                        await confirm({
                          title: `Remove the login for ${r.registry}?`,
                          body: 'Pulls from this registry go back to anonymous: private images will fail to pull (running containers keep running).',
                          confirm: 'Remove',
                          danger: true,
                        })
                      )
                        remove.mutate(r)
                    }}
                  />
                </div>
                {t && (
                  <div className="mt-1.5">
                    {t.ok ? (
                      <Badge tone="good">
                        {t.status || 'Login Succeeded'} (from {t.host})
                      </Badge>
                    ) : (
                      <ErrorNote error={`Tested from ${t.host}: ${t.error}`} />
                    )}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}
      <RegistryModal editing={editing} onClose={() => setEditing(null)} />
      <TestHostNote />
    </Card>
  )
}

function TestHostNote() {
  const hosts = useHostsQuery().data ?? []
  if (hosts.length === 0 || hosts.some((h) => h.online)) return null
  return <p className="mt-2 text-xs text-ink-100/50">Testing needs a host online: the check runs through its Docker Engine.</p>
}

function RegistryModal({ editing, onClose }: { editing: RegistryLogin | 'new' | null; onClose: () => void }) {
  const qc = useQueryClient()
  const toast = useToast()
  const isNew = editing === 'new'
  const current = editing && editing !== 'new' ? editing : null
  const [registry, setRegistry] = useState('')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [openedFor, setOpenedFor] = useState<RegistryLogin | 'new' | null>(null)
  if (editing !== openedFor) {
    // A new dialog: start from the login being edited (never its password).
    setOpenedFor(editing)
    setRegistry('')
    setUsername(current?.username ?? '')
    setPassword('')
    setError(null)
  }
  const save = useMutation({
    mutationFn: () =>
      isNew
        ? api<RegistryLogin>('/api/registries', { method: 'POST', body: { registry, username, password } })
        : api<RegistryLogin>(`/api/registries/${current!.id}`, { method: 'PUT', body: { username, password } }),
    onSuccess: (r) => {
      toast('good', isNew ? `Saved the login for ${r.registry}` : `Updated the login for ${r.registry}`)
      setPassword('')
      qc.invalidateQueries({ queryKey: ['registries'] })
      onClose()
    },
    onError: (e) => setError(needsReauth(e) ? 'Sign in again, then save once more.' : errorText(e)),
  })
  const ghcr = (isNew ? registry : current?.registry ?? '').toLowerCase().includes('ghcr.io')
  const hub = /^(https?:\/\/)?(index\.|registry-1\.)?docker\.io/i.test(isNew ? registry.trim() : current?.registry ?? '')
  return (
    <Modal open={editing !== null} onClose={onClose} title={isNew ? 'Add a registry login' : `Edit ${current?.registry ?? ''}`}>
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault()
          setError(null)
          save.mutate()
        }}
      >
        {isNew && (
          <Input
            label="Registry"
            placeholder="ghcr.io"
            value={registry}
            onChange={(e) => setRegistry(e.target.value)}
            autoFocus
            required
            className="mono"
            hint="Just the address: ghcr.io, registry.gitlab.com, docker.io (Docker Hub, for higher rate limits) or myregistry.lan:5000"
          />
        )}
        <Input label="Username" value={username} onChange={(e) => setUsername(e.target.value)} required={isNew} autoComplete="off" autoFocus={!isNew} />
        <Input
          label={isNew ? 'Password or access token' : 'New password or token'}
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required={isNew}
          autoComplete="new-password"
          placeholder={isNew ? '' : '•••• set (leave empty to keep it)'}
          hint={
            ghcr
              ? 'For GHCR, use a personal access token (classic) with the read:packages scope, not your GitHub password.'
              : hub
                ? 'For Docker Hub, use a personal access token (read-only is enough). Logged-in pulls get higher rate limits.'
                : 'Prefer a read-only access token over your account password where the registry offers one.'
          }
        />
        <ErrorNote error={error} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" icon={KeyRound} loading={save.isPending}>
            Save
          </Button>
        </div>
      </form>
    </Modal>
  )
}
