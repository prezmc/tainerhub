import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Fingerprint, KeyRound, LockKeyhole, Plus, RefreshCw, Save, Trash2 } from 'lucide-react'
import { api, errorText, passkeyAdd, passkeysUsable, timeAgo } from '../api'
import { Badge, Button, Card, ErrorNote, IconButton, Input, Modal, PageHeader, RecoveryCodes, cx, useConfirm, useToast } from '../ui'
import { useMe } from './AppLayout'

export function AccountPage() {
  const me = useMe()
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const [name, setName] = useState('')
  const [codes, setCodes] = useState<string[] | null>(null)

  const add = useMutation({
    mutationFn: () => passkeyAdd(name),
    onSuccess: () => {
      setName('')
      toast('good', 'Passkey added')
      qc.invalidateQueries({ queryKey: ['me'] })
    },
    onError: (e) => toast('bad', errorText(e)),
  })
  const del = useMutation({
    mutationFn: (id: string) => api(`/api/me/passkeys/${id}`, { method: 'DELETE' }),
    onSuccess: () => {
      toast('good', 'Passkey removed')
      qc.invalidateQueries({ queryKey: ['me'] })
    },
    onError: (e) => toast('bad', errorText(e)),
  })
  const regen = useMutation({
    mutationFn: () => api<{ recoveryCodes: string[] }>('/api/me/recovery-codes', { method: 'POST' }),
    onSuccess: (r) => {
      setCodes(r.recoveryCodes)
      qc.invalidateQueries({ queryKey: ['me'] })
    },
    onError: (e) => toast('bad', errorText(e)),
  })

  const low = me.recoveryCodesLeft <= 3
  const canAddPasskey = passkeysUsable(me.passkeysEnabled)
  const onlyWayIn = (n: number) => !me.hasPassword && n <= 1

  return (
    <div className="max-w-3xl">
      <PageHeader title="Account & sign-in" subtitle={`Signed in as ${me.user.username}`}>
        <Badge tone="info" className="capitalize">
          {me.user.role}
        </Badge>
      </PageHeader>

      <div className="space-y-6">
        <PasswordCard hasPassword={me.hasPassword} />

        <Card className="p-6">
          <div className="mb-5 flex items-start gap-3">
            <div className="rounded-[3px] bg-amber-500/10 p-2.5 text-amber-300 ring-1 ring-inset ring-amber-500/20">
              <Fingerprint className="size-5" />
            </div>
            <div>
              <h2 className="font-semibold text-white">Passkeys</h2>
              <p className="text-sm text-ink-100/60">
                {canAddPasskey
                  ? 'Sign in with Touch ID, Windows Hello, your phone or a security key. Register one on each device you use.'
                  : me.passkeysEnabled
                    ? 'Passkeys need a secure connection. Open TainerHub at its https:// address to add one.'
                    : 'Passkeys need HTTPS. Put TainerHub behind your reverse proxy and set TAINERHUB_PUBLIC_URL to its https:// address (see the README), then add them here.'}
              </p>
            </div>
          </div>
          {me.passkeys.length > 0 && <ul className="mb-5 divide-y divide-white/[0.05] overflow-hidden rounded-[3px] ring-1 ring-white/[0.07]">
            {me.passkeys.map((p) => (
              <li key={p.id} className="flex items-center gap-3 bg-white/[0.015] px-4 py-3">
                <KeyRound className="size-4 text-ink-100/50" />
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm font-medium text-white">{p.name}</div>
                  <div className="text-xs text-ink-100/50">
                    Added {new Date(p.createdAt).toLocaleDateString()} · last used {timeAgo(p.lastUsedAt)}
                  </div>
                </div>
                <IconButton
                  icon={Trash2}
                  tone="bad"
                  label={onlyWayIn(me.passkeys.length) ? "Set a password before removing your only passkey" : 'Remove passkey'}
                  disabled={onlyWayIn(me.passkeys.length)}
                  onClick={async () => {
                    if (await confirm({ title: `Remove "${p.name}"?`, body: 'That device will no longer be able to sign in.', confirm: 'Remove', danger: true })) del.mutate(p.id)
                  }}
                />
              </li>
            ))}
          </ul>}
          {canAddPasskey && <form
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              add.mutate()
            }}
          >
            <div className="flex-1">
              <Input label="Add a passkey" placeholder="Name it, e.g. iPhone" value={name} onChange={(e) => setName(e.target.value)} maxLength={60} />
            </div>
            <Button variant="primary" icon={Plus} loading={add.isPending}>
              Add
            </Button>
          </form>}
        </Card>

        <Card className="p-6">
          <div className="flex flex-wrap items-center gap-4">
            <div className="flex flex-1 items-start gap-3">
              <div
                className={cx(
                  'rounded-[3px] p-2.5 ring-1 ring-inset',
                  low ? 'bg-rose-500/10 text-rose-300 ring-rose-500/20' : 'bg-cyan-500/10 text-cyan-300 ring-cyan-500/20',
                )}
              >
                <KeyRound className="size-5" />
              </div>
              <div>
                <h2 className="font-semibold text-white">Recovery codes</h2>
                <p className="text-sm text-ink-100/60">
                  <span className={low ? 'font-medium text-rose-300' : 'text-ink-50'}>{me.recoveryCodesLeft}</span> unused. Making new codes cancels the old ones.
                </p>
              </div>
            </div>
            <Button
              icon={RefreshCw}
              loading={regen.isPending}
              onClick={async () => {
                if (await confirm({ title: 'Generate new recovery codes?', body: 'Your current codes stop working immediately.', confirm: 'Generate' })) regen.mutate()
              }}
            >
              Generate new codes
            </Button>
          </div>
        </Card>
      </div>

      <Modal open={!!codes} onClose={() => setCodes(null)} title="Your new recovery codes">
        {codes && <RecoveryCodes codes={codes} />}
      </Modal>
    </div>
  )
}

function PasswordCard({ hasPassword }: { hasPassword: boolean }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const [error, setError] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => api('/api/me/password', { method: 'POST', body: { current, new: next } }),
    onSuccess: () => {
      setCurrent('')
      setNext('')
      setAgain('')
      setError(null)
      toast('good', hasPassword ? 'Password changed' : 'Password set')
      qc.invalidateQueries({ queryKey: ['me'] })
    },
    onError: (e) => setError(errorText(e)),
  })
  return (
    <Card className="p-6">
      <div className="mb-5 flex items-start gap-3">
        <div className="rounded-[3px] bg-lime-400/10 p-2.5 text-lime-300 ring-1 ring-inset ring-lime-400/20">
          <LockKeyhole className="size-5" />
        </div>
        <div>
          <h2 className="font-semibold text-white">Password</h2>
          <p className="text-sm text-ink-100/60">{hasPassword ? 'Works everywhere, including before TainerHub is on HTTPS.' : 'You sign in with passkeys only. A password gives you a second way in.'}</p>
        </div>
      </div>
      <form
        className="grid gap-3 sm:grid-cols-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (next !== again) {
            setError("The new passwords don't match.")
            return
          }
          save.mutate()
        }}
      >
        {hasPassword && <Input label="Current password" type="password" value={current} onChange={(e) => setCurrent(e.target.value)} required autoComplete="current-password" />}
        <Input label="New password" type="password" value={next} onChange={(e) => setNext(e.target.value)} required minLength={10} autoComplete="new-password" />
        <Input label="Repeat new password" type="password" value={again} onChange={(e) => setAgain(e.target.value)} required autoComplete="new-password" />
        <div className="flex items-center gap-3 sm:col-span-3">
          <Button variant="primary" icon={Save} loading={save.isPending}>
            {hasPassword ? 'Change password' : 'Set password'}
          </Button>
          <ErrorNote error={error} />
        </div>
      </form>
    </Card>
  )
}
