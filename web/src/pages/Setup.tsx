import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { browserSupportsWebAuthn } from '@simplewebauthn/browser'
import { ArrowRight, Fingerprint, Terminal } from 'lucide-react'
import { errorText, passkeySetup } from '../api'
import { Button, ErrorNote, Input, RecoveryCodes } from '../ui'
import { AuthShell } from './AuthShell'

export function SetupPage() {
  const token = new URLSearchParams(window.location.search).get('token') ?? ''
  const [username, setUsername] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [passkeyName, setPasskeyName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [codes, setCodes] = useState<string[] | null>(null)
  const qc = useQueryClient()
  const navigate = useNavigate()

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const res = await passkeySetup({ token, username, displayName, passkeyName })
      // Drop the one-time token from the address bar and history.
      window.history.replaceState(null, '', '/setup')
      setCodes(res.recoveryCodes)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  if (codes) {
    return (
      <AuthShell title="You're all set" subtitle="Your owner account is ready and you're signed in.">
        <div className="space-y-5">
          <RecoveryCodes codes={codes} />
          <Button
            variant="primary"
            icon={ArrowRight}
            className="w-full"
            onClick={async () => {
              await qc.invalidateQueries()
              navigate({ to: '/hosts' })
            }}
          >
            I've saved them, continue
          </Button>
        </div>
      </AuthShell>
    )
  }

  if (!token) {
    return (
      <AuthShell title="Set up TainerHub" subtitle="One more step to secure your hub">
        <div className="space-y-3 text-sm text-indigo-100/80">
          <p>Open the one-time setup link printed in the hub's logs. It's valid for 15 minutes, and a fresh link is printed every 15 minutes until setup is done.</p>
          <div className="mono flex items-center gap-2 rounded-lg bg-black/40 px-3 py-2 text-xs text-indigo-100/80 ring-1 ring-white/10">
            <Terminal className="size-3.5 text-indigo-100/50" />
            docker logs tainerhub
          </div>
        </div>
      </AuthShell>
    )
  }

  return (
    <AuthShell title="Create your account" subtitle="You'll sign in with a passkey: Touch ID, Windows Hello, your phone or a security key.">
      {!browserSupportsWebAuthn() ? (
        <ErrorNote error="This browser doesn't support passkeys. Use a current Chrome, Safari, Edge or Firefox over HTTPS." />
      ) : (
        <form onSubmit={submit} className="space-y-4">
          <Input label="Username" value={username} onChange={(e) => setUsername(e.target.value)} autoFocus required autoComplete="username" placeholder="admin" />
          <Input label="Display name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="Your name" />
          <Input label="Name this passkey" value={passkeyName} onChange={(e) => setPasskeyName(e.target.value)} placeholder="MacBook Touch ID" hint="Helps you tell your passkeys apart later." />
          <ErrorNote error={error} />
          <Button variant="primary" icon={Fingerprint} className="w-full py-2.5" disabled={!username} loading={busy}>
            {busy ? 'Waiting for your passkey…' : 'Create account with passkey'}
          </Button>
        </form>
      )}
    </AuthShell>
  )
}
