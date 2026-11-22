import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { ArrowRight, Terminal, UserPlus } from 'lucide-react'
import { api, errorText, type User } from '../api'
import { Button, ErrorNote, Input, RecoveryCodes } from '../ui'
import { AuthShell } from './AuthShell'

export function SetupPage() {
  const [token, setToken] = useState(() => new URLSearchParams(window.location.search).get('token') ?? '')
  const [pasted, setPasted] = useState('')
  const [username, setUsername] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [codes, setCodes] = useState<string[] | null>(null)
  const qc = useQueryClient()
  const navigate = useNavigate()

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (password !== confirm) {
      setError("The passwords don't match.")
      return
    }
    setBusy(true)
    setError(null)
    try {
      const res = await api<{ user: User; recoveryCodes: string[] }>('/api/setup/password', { method: 'POST', body: { token, username, displayName, password } })
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
        <div className="space-y-3 text-sm text-ink-100/80">
          <p>Open the setup link the installer printed. Don't have it, or it's older than 15 minutes? Run this on the server and paste what it prints below.</p>
          <div className="mono flex items-center gap-2 rounded-[3px] bg-black/40 px-3 py-2 text-xs text-ink-100/80 ring-1 ring-white/10">
            <Terminal className="size-3.5 shrink-0 text-ink-100/50" />
            docker exec tainerhub /tainerhub setup-link
          </div>
          <form
            className="flex items-end gap-2 pt-1"
            onSubmit={(e) => {
              e.preventDefault()
              // Accept the whole link, "/setup?token=...", or just the token.
              const m = pasted.match(/token=([^&\s]+)/)
              const t = (m ? m[1] : pasted).trim()
              if (!t) return
              window.history.replaceState(null, '', `/setup?token=${encodeURIComponent(t)}`)
              setToken(t)
            }}
          >
            <div className="flex-1">
              <Input label="Setup link or token" value={pasted} onChange={(e) => setPasted(e.target.value)} placeholder="/setup?token=…" autoFocus />
            </div>
            <Button variant="primary" icon={ArrowRight}>
              Continue
            </Button>
          </form>
        </div>
      </AuthShell>
    )
  }

  return (
    <AuthShell title="Create your account" subtitle="This is the owner account. You can add passkeys later, once TainerHub is on HTTPS.">
      <form onSubmit={submit} className="space-y-4">
        <Input label="Username" value={username} onChange={(e) => setUsername(e.target.value)} autoFocus required autoComplete="username" placeholder="admin" />
        <Input label="Display name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="Your name" />
        <Input label="Password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={10} autoComplete="new-password" hint="At least 10 characters." />
        <Input label="Confirm password" type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required autoComplete="new-password" />
        <ErrorNote error={error} />
        <Button variant="primary" icon={UserPlus} className="w-full" disabled={!username || !password} loading={busy}>
          Create account
        </Button>
      </form>
    </AuthShell>
  )
}
