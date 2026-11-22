import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { ArrowLeft, Fingerprint, KeyRound, UserRound } from 'lucide-react'
import { api, errorText, passkeyLogin } from '../api'
import { Button, ErrorNote, Input } from '../ui'
import { AuthShell } from './AuthShell'

export function LoginPage() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [recovery, setRecovery] = useState(false)
  const [username, setUsername] = useState('')
  const [code, setCode] = useState('')
  const qc = useQueryClient()
  const navigate = useNavigate()

  const finish = async (path: '/' | '/account') => {
    await qc.invalidateQueries()
    navigate({ to: path })
  }

  const withPasskey = async () => {
    setBusy(true)
    setError(null)
    try {
      await passkeyLogin()
      await finish('/')
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const withCode = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api('/api/login/recovery', { method: 'POST', body: { username, code } })
      await finish('/account') // nudge them to register a new passkey
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <AuthShell title="Welcome back" subtitle={recovery ? 'Sign in with a one-time recovery code' : 'Sign in to manage your containers'}>
      {!recovery ? (
        <div className="space-y-4">
          <Button variant="primary" icon={Fingerprint} className="w-full py-3 text-[15px]" onClick={withPasskey} loading={busy}>
            {busy ? 'Waiting for your passkey…' : 'Sign in with passkey'}
          </Button>
          <ErrorNote error={error} />
          <div className="flex items-center gap-3 text-xs text-indigo-100/40">
            <span className="h-px flex-1 bg-white/10" />
            or
            <span className="h-px flex-1 bg-white/10" />
          </div>
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
      ) : (
        <form onSubmit={withCode} className="space-y-4">
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
            Back to passkey sign-in
          </Button>
        </form>
      )}
    </AuthShell>
  )
}
