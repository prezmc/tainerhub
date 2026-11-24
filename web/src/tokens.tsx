import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, KeySquare, Plus, Trash2 } from 'lucide-react'
import { api, errorText, needsReauth, roleRank, timeAgo, type Role, type User } from './api'
import { Badge, Button, Card, CopyButton, ErrorNote, IconButton, Input, Modal, useConfirm, useToast } from './ui'

/** An API token's metadata (the token itself is only ever shown once). */
export interface ApiToken {
  id: string
  userId: string
  /** Set in the admin list only. */
  username?: string
  name: string
  prefix: string
  role: Role
  createdAt: string
  expiresAt: string | null
  lastUsedAt: string | null
  lastUsedIp: string
}

const ROLES: Role[] = ['viewer', 'operator', 'admin', 'owner']
const EXPIRY = [
  { days: 30, label: '30 days' },
  { days: 90, label: '90 days' },
  { days: 365, label: '1 year' },
  { days: 0, label: 'Never' },
]

const shown = (t: ApiToken) => `thp_${t.prefix}…`
const expiryText = (t: ApiToken) => {
  if (!t.expiresAt) return 'never expires'
  const at = new Date(t.expiresAt)
  return at.getTime() <= Date.now() ? `expired ${at.toLocaleDateString()}` : `expires ${at.toLocaleDateString()}`
}
const roleTone = (r: Role) => (r === 'owner' || r === 'admin' ? 'warn' : r === 'operator' ? 'info' : 'idle')

const selectClass = 'w-full rounded-[2px] bg-ink-950/70 px-2 py-2 text-sm capitalize text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60'

/** The Account page card: the user's own tokens, creating and revoking them. */
export function ApiTokensCard({ user, onReauth }: { user: User; onReauth: () => void }) {
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const tokens = useQuery({ queryKey: ['me', 'tokens'], queryFn: () => api<ApiToken[]>('/api/me/tokens') })
  const roles = ROLES.filter((r) => roleRank[r] <= roleRank[user.role])
  const [name, setName] = useState('')
  const [role, setRole] = useState<Role>('viewer')
  const [days, setDays] = useState(90)
  const [error, setError] = useState<string | null>(null)
  const [created, setCreated] = useState<(ApiToken & { token: string }) | null>(null)

  const refresh = () => qc.invalidateQueries({ queryKey: ['me', 'tokens'] })
  const create = useMutation({
    mutationFn: () => api<ApiToken & { token: string }>('/api/me/tokens', { method: 'POST', body: { name, role, expiresInDays: days } }),
    onSuccess: (t) => {
      setCreated(t)
      setName('')
      setError(null)
      refresh()
    },
    // Creating a token needs a recent sign-in, like other sign-in changes.
    onError: (e) => (needsReauth(e) ? onReauth() : setError(errorText(e))),
  })
  const revoke = useMutation({
    mutationFn: (t: ApiToken) => api(`/api/me/tokens/${t.id}`, { method: 'DELETE' }),
    onSuccess: (_, t) => {
      toast('good', `Token "${t.name}" revoked`)
      refresh()
    },
    onError: (e) => toast('bad', errorText(e)),
  })

  const list = tokens.data ?? []
  return (
    <Card className="p-6">
      <div className="mb-5 flex items-start gap-3">
        <div className="rounded-[3px] bg-violet-500/10 p-2.5 text-violet-300 ring-1 ring-inset ring-violet-500/20">
          <KeySquare className="size-5" />
        </div>
        <div>
          <h2 className="font-semibold text-white">API tokens</h2>
          <p className="text-sm text-ink-100/60">
            Let scripts and tools use TainerHub's API as you. A token never does more than its access level, or more than your own role allows. It can't manage users, tokens or how
            you sign in.
          </p>
        </div>
      </div>
      <ErrorNote error={tokens.isError ? errorText(tokens.error) : null} />
      {list.length > 0 && (
        <ul className="mb-5 divide-y divide-white/[0.05] overflow-hidden rounded-[3px] ring-1 ring-white/[0.07]">
          {list.map((t) => (
            <li key={t.id} className="flex items-center gap-3 bg-white/[0.015] px-4 py-3">
              <KeySquare className="size-4 shrink-0 text-ink-100/50" />
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="truncate text-sm font-medium text-white">{t.name}</span>
                  <span className="mono text-xs text-ink-100/50">{shown(t)}</span>
                  <Badge tone={roleTone(t.role)}>{t.role}</Badge>
                </div>
                <div className="text-xs text-ink-100/50">
                  Created {new Date(t.createdAt).toLocaleDateString()} · {expiryText(t)} · last used {timeAgo(t.lastUsedAt)}
                  {t.lastUsedIp && <> from <span className="mono">{t.lastUsedIp}</span></>}
                </div>
              </div>
              <IconButton
                icon={Trash2}
                tone="bad"
                label={`Revoke ${t.name}`}
                onClick={async () => {
                  if (await confirm({ title: `Revoke "${t.name}"?`, body: 'Anything using this token stops working straight away.', confirm: 'Revoke', danger: true })) revoke.mutate(t)
                }}
              />
            </li>
          ))}
        </ul>
      )}
      <form
        className="grid gap-3 sm:grid-cols-[1fr_9rem_8rem_auto] sm:items-end"
        onSubmit={(e) => {
          e.preventDefault()
          create.mutate()
        }}
      >
        <Input label="Create a token" placeholder="Name it, e.g. backup script" value={name} onChange={(e) => setName(e.target.value)} maxLength={60} required />
        <label className="block">
          <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Access</span>
          <select aria-label="Access level" value={role} onChange={(e) => setRole(e.target.value as Role)} className={selectClass}>
            {roles.map((r) => (
              <option key={r} value={r} className="bg-panel">
                {r}
              </option>
            ))}
          </select>
        </label>
        <label className="block">
          <span className="mb-1.5 block text-sm font-medium text-ink-100/80">Expires</span>
          <select aria-label="Expires after" value={days} onChange={(e) => setDays(Number(e.target.value))} className={selectClass}>
            {EXPIRY.map((x) => (
              <option key={x.days} value={x.days} className="bg-panel">
                {x.label}
              </option>
            ))}
          </select>
        </label>
        <Button variant="primary" icon={Plus} loading={create.isPending}>
          Create
        </Button>
        <div className="sm:col-span-4">
          <ErrorNote error={error} />
        </div>
      </form>

      <Modal open={!!created} onClose={() => setCreated(null)} title="Your new API token" wide>
        {created && <NewToken token={created} />}
        <div className="mt-5 flex justify-end">
          <Button variant="primary" onClick={() => setCreated(null)}>
            Done
          </Button>
        </div>
      </Modal>
    </Card>
  )
}

function NewToken({ token: t }: { token: ApiToken & { token: string } }) {
  const curl = `curl -H "Authorization: Bearer ${t.token}" ${window.location.origin}/api/hosts`
  return (
    <div className="space-y-4 text-sm text-ink-100/80">
      <div className="flex items-start gap-2.5 rounded-[3px] bg-amber-500/10 p-3 text-amber-200 ring-1 ring-inset ring-amber-500/20">
        <AlertTriangle className="mt-0.5 size-4 shrink-0" />
        <p>Copy it now: it won't be shown again. Anyone who has it can act as you with {t.role} access, so keep it somewhere safe, like a password manager or your script's secrets.</p>
      </div>
      <div>
        <div className="mb-1.5 font-medium text-white">
          {t.name} <Badge tone={roleTone(t.role)}>{t.role}</Badge>
        </div>
        <pre className="mono overflow-x-auto whitespace-pre-wrap break-all rounded-[3px] bg-black/40 p-3 text-xs text-lime-200 ring-1 ring-line">{t.token}</pre>
        <div className="mt-2 flex justify-end">
          <CopyButton text={t.token} label="Copy token" />
        </div>
      </div>
      <div>
        <div className="mb-1.5 font-medium text-white">Try it</div>
        <pre className="mono overflow-x-auto whitespace-pre-wrap break-all rounded-[3px] bg-black/40 p-3 text-xs text-ink-100/80 ring-1 ring-line">{curl}</pre>
        <div className="mt-2 flex justify-end">
          <CopyButton text={curl} label="Copy command" variant="ghost" />
        </div>
      </div>
    </div>
  )
}

/** The Users page section: every token an admin may see, with Revoke. */
export function AllTokensSection() {
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const tokens = useQuery({ queryKey: ['tokens'], queryFn: () => api<ApiToken[]>('/api/tokens') })
  const revoke = useMutation({
    mutationFn: (t: ApiToken) => api(`/api/tokens/${t.id}`, { method: 'DELETE' }),
    onSuccess: (_, t) => {
      toast('good', `Token "${t.name}" revoked`)
      qc.invalidateQueries({ queryKey: ['tokens'] })
      qc.invalidateQueries({ queryKey: ['me', 'tokens'] })
    },
    onError: (e) => toast('bad', errorText(e)),
  })
  const list = tokens.data ?? []
  if (!tokens.isError && list.length === 0) return null
  return (
    <section className="mt-8">
      <h2 className="mb-3 text-xs font-semibold uppercase tracking-wider text-ink-100/50">API tokens</h2>
      <ErrorNote error={tokens.isError ? errorText(tokens.error) : null} />
      {list.length > 0 && (
        <Card className="divide-y divide-line">
          {list.map((t) => (
            <div key={t.id} className="flex flex-wrap items-center gap-3 px-5 py-3 text-sm">
              <KeySquare className="size-4 text-ink-100/40" />
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-white">{t.name}</span>
                  <span className="mono text-xs text-ink-100/50">{shown(t)}</span>
                </div>
                <div className="text-xs text-ink-100/50">
                  {t.username} · {expiryText(t)} · last used {timeAgo(t.lastUsedAt)}
                  {t.lastUsedIp && <> from <span className="mono">{t.lastUsedIp}</span></>}
                </div>
              </div>
              <Badge tone={roleTone(t.role)}>{t.role}</Badge>
              <Button
                variant="dangerGhost"
                className="!px-2.5 !py-1 text-[11px]"
                onClick={async () => {
                  if (await confirm({ title: `Revoke "${t.name}"?`, body: `${t.username}'s scripts using this token stop working straight away.`, confirm: 'Revoke', danger: true })) revoke.mutate(t)
                }}
              >
                Revoke
              </Button>
            </div>
          ))}
        </Card>
      )}
    </section>
  )
}
