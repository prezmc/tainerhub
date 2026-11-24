import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Fingerprint, KeyRound, LockKeyhole, LogIn, LogOut, MonitorSmartphone, Plus, RefreshCw, Save, ShieldAlert, Trash2 } from 'lucide-react'
import { api, errorText, needsReauth, passkeyAdd, passkeysUsable, timeAgo } from '../api'
import { Badge, Button, Card, ErrorNote, IconButton, Input, Modal, PageHeader, RecoveryCodes, cx, useConfirm, useToast } from '../ui'
import { useMe } from './AppLayout'
import { useSignInAgain } from '../reauth'
import { ApiTokensCard } from '../tokens'

export function AccountPage() {
  const me = useMe()
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const [name, setName] = useState('')
  const [codes, setCodes] = useState<string[] | null>(null)
  const [reauth, setReauth] = useState(false)
  // Sensitive changes need a recent sign-in; offer one instead of a bare error.
  const onError = (e: unknown) => (needsReauth(e) ? setReauth(true) : toast('bad', errorText(e))) // ReauthPrompt also opens

  const add = useMutation({
    mutationFn: () => passkeyAdd(name),
    onSuccess: () => {
      setName('')
      toast('good', 'Passkey added')
      qc.invalidateQueries({ queryKey: ['me'] })
    },
    onError,
  })
  const del = useMutation({
    mutationFn: (id: string) => api(`/api/me/passkeys/${id}`, { method: 'DELETE' }),
    onSuccess: () => {
      toast('good', 'Passkey removed')
      qc.invalidateQueries({ queryKey: ['me'] })
    },
    onError,
  })
  const regen = useMutation({
    mutationFn: () => api<{ recoveryCodes: string[] }>('/api/me/recovery-codes', { method: 'POST' }),
    onSuccess: (r) => {
      setCodes(r.recoveryCodes)
      qc.invalidateQueries({ queryKey: ['me'] })
    },
    onError,
  })

  const others = useMutation({
    mutationFn: () => api<{ signedOut: number }>('/api/me/sessions/sign-out-others', { method: 'POST' }),
    onSuccess: (r) => toast('good', r.signedOut === 1 ? 'Signed out 1 other session' : `Signed out ${r.signedOut} other sessions`),
    onError,
  })

  const low = me.recoveryCodesLeft <= 3
  const canAddPasskey = passkeysUsable(me.passkeysEnabled, me.publicURL)
  const onlyWayIn = (n: number) => !me.hasPassword && n <= 1

  return (
    <div className="max-w-3xl">
      <PageHeader title="Account & sign-in" subtitle={`Signed in as ${me.user.username}`}>
        <Badge tone="info" className="capitalize">
          {me.user.role}
        </Badge>
      </PageHeader>

      <div className="space-y-6">
        {reauth && <ReauthCard />}

        <PasswordCard hasPassword={me.hasPassword} onReauth={() => setReauth(true)} />

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
                    ? <>Passkeys work at TainerHub's secure address. <a href={me.publicURL + '/account'} className="font-semibold text-lime-300 underline underline-offset-2">Open {me.publicURL}</a> to add one.</>
                    : 'Passkeys need HTTPS. An admin can set it up under Settings (it takes a few minutes with Nginx Proxy Manager), then add them here.'}
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

        <ApiTokensCard user={me.user} onReauth={() => setReauth(true)} />

        <Card className="p-6">
          <div className="flex flex-wrap items-center gap-4">
            <div className="flex flex-1 items-start gap-3">
              <div className="rounded-[3px] bg-white/[0.04] p-2.5 text-ink-100/70 ring-1 ring-inset ring-white/10">
                <MonitorSmartphone className="size-5" />
              </div>
              <div>
                <h2 className="font-semibold text-white">Other sessions</h2>
                <p className="text-sm text-ink-100/60">Signed in on a device you no longer use, or one that isn't yours? Sign it out. This browser stays signed in.</p>
              </div>
            </div>
            <Button
              icon={LogOut}
              loading={others.isPending}
              onClick={async () => {
                if (await confirm({ title: 'Sign out other sessions?', body: 'Every other browser and device signed in to your account will have to sign in again.', confirm: 'Sign out others' })) others.mutate()
              }}
            >
              Sign out other sessions
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

/** Shown when the hub wants a recent sign-in before a sensitive change. */
function ReauthCard() {
  const { go: signInAgain, busy } = useSignInAgain()
  return (
    <Card className="p-6">
      <div className="flex flex-wrap items-center gap-4">
        <div className="flex flex-1 items-start gap-3">
          <div className="rounded-[3px] bg-amber-500/10 p-2.5 text-amber-300 ring-1 ring-inset ring-amber-500/20">
            <ShieldAlert className="size-5" />
          </div>
          <div>
            <h2 className="font-semibold text-white">Sign in again to continue</h2>
            <p className="text-sm text-ink-100/60">For your safety, changing how you sign in needs a sign-in from the last 10 minutes. Sign in again, then come back here and try once more.</p>
          </div>
        </div>
        <Button variant="primary" icon={LogIn} loading={busy} onClick={signInAgain}>
          Sign in again
        </Button>
      </div>
    </Card>
  )
}

function PasswordCard({ hasPassword, onReauth }: { hasPassword: boolean; onReauth: () => void }) {
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
      toast('good', `${hasPassword ? 'Password changed' : 'Password set'}. Other sessions were signed out.`)
      qc.invalidateQueries({ queryKey: ['me'] })
    },
    onError: (e) => (needsReauth(e) ? onReauth() : setError(errorText(e))),
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
        className="grid grid-cols-1 gap-3 sm:grid-cols-3"
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
