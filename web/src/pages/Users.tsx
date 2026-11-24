import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link2, Trash2, UserPlus, Users as UsersIcon } from 'lucide-react'
import { api, errorText, roleRank, timeAgo, type Role, type User } from '../api'
import { Badge, Button, Card, CopyButton, ErrorNote, IconButton, Input, Modal, PageHeader, useConfirm, useToast } from '../ui'
import { useMe } from './AppLayout'
import { AllTokensSection } from '../tokens'

type UserRow = User & { hasPassword: boolean; manageable: boolean }
type Invite = { id: string; role: Role; note: string; createdBy: string; createdAt: string; expiresAt: string }

const ROLE_HELP: Record<Exclude<Role, 'owner'>, string> = {
  viewer: 'Sees everything, changes nothing.',
  operator: 'Also starts, stops, restarts and updates containers and stacks.',
  admin: 'Also edits stacks, deletes volumes, opens terminals, manages hosts and users.',
}

export function UsersPage() {
  const me = useMe()
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const data = useQuery({ queryKey: ['users'], queryFn: () => api<{ users: UserRow[]; invites: Invite[] }>('/api/users') })
  const [inviting, setInviting] = useState(false)
  const [role, setRole] = useState<Role>('operator')
  const [note, setNote] = useState('')
  const [link, setLink] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const roles = (['viewer', 'operator', 'admin'] as const).filter((r) => roleRank[r] <= roleRank[me.user.role])

  const refresh = () => qc.invalidateQueries({ queryKey: ['users'] })
  const invite = useMutation({
    mutationFn: () => api<{ path: string }>('/api/invites', { method: 'POST', body: { role, note } }),
    onSuccess: (r) => {
      setLink(window.location.origin + r.path)
      setNote('')
      refresh()
    },
    onError: (e) => setError(errorText(e)),
  })
  const setUserRole = useMutation({
    mutationFn: ({ u, r }: { u: UserRow; r: Role }) => api(`/api/users/${u.id}/role`, { method: 'PUT', body: { role: r } }),
    onSuccess: (_, { u, r }) => {
      toast('good', `${u.username} is now ${r === 'admin' ? 'an' : 'a'} ${r}`)
      refresh()
    },
    onError: (e) => toast('bad', errorText(e)),
  })
  const remove = useMutation({
    mutationFn: (u: UserRow) => api(`/api/users/${u.id}`, { method: 'DELETE' }),
    onSuccess: (_, u) => {
      toast('good', `${u.username} removed`)
      refresh()
    },
    onError: (e) => toast('bad', errorText(e)),
  })
  const revoke = useMutation({
    mutationFn: (i: Invite) => api(`/api/invites/${i.id}`, { method: 'DELETE' }),
    onSuccess: () => {
      toast('good', 'Invite revoked')
      refresh()
    },
    onError: (e) => toast('bad', errorText(e)),
  })

  return (
    <div className="max-w-4xl">
      <PageHeader title="Users" subtitle="Who can sign in, and what they can do.">
        <Button
          variant="primary"
          icon={UserPlus}
          onClick={() => {
            setInviting(true)
            setLink(null)
            setError(null)
          }}
        >
          Invite someone
        </Button>
      </PageHeader>

      <Card className="divide-y divide-line">
        {data.data?.users.map((u) => (
          <div key={u.id} className="flex flex-wrap items-center gap-3 px-5 py-3">
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2">
                <span className="font-semibold text-white">{u.displayName}</span>
                {u.id === me.user.id && <Badge tone="info">you</Badge>}
              </div>
              <div className="text-xs text-ink-100/50">
                {u.username} · joined {timeAgo(u.createdAt)}
              </div>
            </div>
            {u.manageable ? (
              <select
                aria-label={`Role for ${u.username}`}
                value={u.role}
                onChange={(e) => setUserRole.mutate({ u, r: e.target.value as Role })}
                className="rounded-[2px] bg-ink-950/70 px-2 py-1.5 text-sm capitalize text-white ring-1 ring-inset ring-white/12 outline-none focus:ring-lime-400/60"
              >
                {roles.map((r) => (
                  <option key={r} value={r} className="bg-panel">
                    {r}
                  </option>
                ))}
              </select>
            ) : (
              <Badge tone={u.role === 'owner' ? 'accent' : 'idle'} className="capitalize">
                {u.role}
              </Badge>
            )}
            <div className="w-8">
              {u.manageable && (
                <IconButton
                  icon={Trash2}
                  tone="bad"
                  label={`Remove ${u.username}`}
                  onClick={async () => {
                    if (
                      await confirm({
                        title: `Remove ${u.username}?`,
                        body: "They're signed out everywhere straight away and can't sign in again. Their past actions stay in the audit log.",
                        confirm: 'Remove user',
                        danger: true,
                      })
                    )
                      remove.mutate(u)
                  }}
                />
              )}
            </div>
          </div>
        ))}
      </Card>

      {data.data && data.data.invites.length > 0 && (
        <section className="mt-8">
          <h2 className="mb-3 text-xs font-semibold uppercase tracking-wider text-ink-100/50">Pending invites</h2>
          <Card className="divide-y divide-line">
            {data.data.invites.map((i) => (
              <div key={i.id} className="flex flex-wrap items-center gap-3 px-5 py-3 text-sm">
                <Link2 className="size-4 text-ink-100/40" />
                <div className="min-w-0 flex-1">
                  <div className="text-white">{i.note || 'Unnamed invite'}</div>
                  <div className="text-xs text-ink-100/50">
                    by {i.createdBy || 'someone'} · expires {new Date(i.expiresAt).toLocaleString()}
                  </div>
                </div>
                <Badge className="capitalize">{i.role}</Badge>
                <Button variant="dangerGhost" className="!px-2.5 !py-1 text-[11px]" onClick={() => revoke.mutate(i)}>
                  Revoke
                </Button>
              </div>
            ))}
          </Card>
        </section>
      )}

      <AllTokensSection />

      <Modal open={inviting} onClose={() => setInviting(false)} title={link ? 'Send this link' : 'Invite someone'}>
        {link ? (
          <div className="space-y-4 text-sm text-ink-100/80">
            <p>
              Send it to them however you like. It works once, for 24 hours, and makes them {role === 'admin' ? 'an' : 'a'} <strong className="text-white">{role}</strong>. They choose
              their own username and password.
            </p>
            <pre className="mono overflow-x-auto whitespace-pre-wrap break-all rounded-[3px] bg-black/40 p-3 text-xs text-lime-200 ring-1 ring-line">{link}</pre>
            <div className="flex justify-end gap-2">
              <Button variant="ghost" onClick={() => setInviting(false)}>
                Done
              </Button>
              <CopyButton text={link} label="Copy link" />
            </div>
          </div>
        ) : (
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault()
              setError(null)
              invite.mutate()
            }}
          >
            <fieldset className="space-y-2">
              <legend className="mb-1.5 text-sm font-medium text-ink-100/80">Role</legend>
              {roles.map((r) => (
                <label key={r} className="flex cursor-pointer items-start gap-2.5 rounded-[3px] p-2.5 ring-1 ring-inset ring-line has-[:checked]:bg-lime-400/[0.07] has-[:checked]:ring-lime-400/50">
                  <input type="radio" name="role" value={r} checked={role === r} onChange={() => setRole(r)} className="mt-1 accent-lime-400" />
                  <span>
                    <span className="block text-sm font-semibold capitalize text-white">{r}</span>
                    <span className="block text-xs text-ink-100/60">{ROLE_HELP[r]}</span>
                  </span>
                </label>
              ))}
            </fieldset>
            <Input label="Who is it for? (optional)" value={note} onChange={(e) => setNote(e.target.value)} maxLength={100} placeholder="Alex, for the media server" />
            <ErrorNote error={error} />
            <div className="flex justify-end gap-2">
              <Button type="button" variant="ghost" onClick={() => setInviting(false)}>
                Cancel
              </Button>
              <Button variant="primary" icon={Link2} loading={invite.isPending}>
                Create invite link
              </Button>
            </div>
          </form>
        )}
      </Modal>
      {data.data?.users.length === 1 && !inviting && (
        <p className="mt-4 flex items-center gap-2 text-sm text-ink-100/50">
          <UsersIcon className="size-4" /> Only you so far.
        </p>
      )}
    </div>
  )
}
