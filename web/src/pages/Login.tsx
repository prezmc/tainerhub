import { useState } from 'react'
import { useRouter } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowLeft, Fingerprint, KeyRound, LockKeyhole, LogIn, UserRound } from 'lucide-react'
import { api, errorText, passkeyLogin, passkeysUsable, safeNext, type HubState } from '../api'
import { Button, ErrorNote, Input } from '../ui'
import { AuthShell } from './AuthShell'

export function LoginPage() {
  const state = useQuery({ queryKey: ['state'], queryFn: () => api<HubState>('/api/state') })
  const passkeys = passkeysUsable(state.data?.passkeys, state.data?.publicURL)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [recovery, setRecovery] = useState(false)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const qc = useQueryClient()
  const router = useRouter()
  // Where to go after signing in, e.g. back to Account after a re-sign-in.
  const next = safeNext(new URLSearchParams(window.location.search).get('next')) ?? '/'

  const run = async (fn: () => Promise<unknown>, then: string) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
      await qc.invalidateQueries()
      router.history.push(then)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  if (recovery) {
    return (
      <AuthShell title="Welcome back" subtitle="Sign in with a one-time recovery code">
        <form
          onSubmit={(e) => {
            e.preventDefault()
            // Then on to Account, to set a new password or passkey.
            run(() => api('/api/login/recovery', { method: 'POST', body: { username, code } }), '/account')
          }}
          className="space-y-4"
        >
          <Input label="Username" icon={UserRound} value={username} onChange={(e) => setUsername(e.target.value)} required autoComplete="username" autoFocus />
          <Input label="Recovery code" icon={KeyRound} value={code} onChange={(e) => setCode(e.target.value)} required className="mono tracking-widest" placeholder="XXXX-XXXX-XXXX" autoComplete="one-time-code" />
          <ErrorNote error={error} />
          <Button variant="primary" className="w-full" loading={busy}>
            Sign in
          </Button>
          <Button
            type="button"
            variant="ghost"
            icon={ArrowLeft}
            className="w-full"
            onClick={() => {
              setRecovery(false)
              setError(null)
            }}
          >
            Back
          </Button>
        </form>
      </AuthShell>
    )
  }

  return (
    <AuthShell title="Welcome back" subtitle="Sign in to manage your containers">
      <div className="space-y-4">
        {passkeys && (
          <>
            <Button variant="primary" icon={Fingerprint} className="w-full" onClick={() => run(passkeyLogin, next)} loading={busy}>
              Sign in with passkey
            </Button>
            <div className="flex items-center gap-3 text-xs text-ink-100/40">
              <span className="h-px flex-1 bg-white/10" />
              or use your password
              <span className="h-px flex-1 bg-white/10" />
            </div>
          </>
        )}
        <form
          onSubmit={(e) => {
            e.preventDefault()
            run(() => api('/api/login/password', { method: 'POST', body: { username, password } }), next)
          }}
          className="space-y-4"
        >
          <Input label="Username" icon={UserRound} value={username} onChange={(e) => setUsername(e.target.value)} required autoComplete="username" autoFocus={!passkeys} />
          <Input label="Password" icon={LockKeyhole} type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" />
          <ErrorNote error={error} />
          <Button variant={passkeys ? 'secondary' : 'primary'} icon={LogIn} className="w-full" loading={busy}>
            Sign in
          </Button>
        </form>
        <Button
          variant="ghost"
          icon={KeyRound}
          className="w-full"
          onClick={() => {
            setRecovery(true)
            setError(null)
          }}
        >
          Use a recovery code
        </Button>
      </div>
    </AuthShell>
  )
}
