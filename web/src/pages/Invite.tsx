import { useEffect, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { ArrowRight, UserPlus } from 'lucide-react'
import { api, errorText, type User } from '../api'
import { Button, ErrorNote, Input, RecoveryCodes } from '../ui'
import { AuthShell } from './AuthShell'

/** Where an invite link lands: pick a username and password, then you're in. */
export function InvitePage() {
  const token = new URLSearchParams(window.location.search).get('token') ?? ''
  const [role, setRole] = useState<string | null>(null)
  const [checkError, setCheckError] = useState<string | null>(null)
  const [username, setUsername] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [codes, setCodes] = useState<string[] | null>(null)
  const qc = useQueryClient()
  const navigate = useNavigate()

  useEffect(() => {
    if (!token) {
      setCheckError('This page needs the link from your invite.')
      return
    }
    api<{ role: string }>('/api/invite/check', { method: 'POST', body: { token } })
      .then((r) => setRole(r.role))
      .catch((e) => setCheckError(errorText(e)))
  }, [token])

  if (codes) {
    return (
      <AuthShell title="Welcome aboard" subtitle="Your account is ready and you're signed in.">
        <div className="space-y-5">
          <RecoveryCodes codes={codes} />
          <Button
            variant="primary"
            icon={ArrowRight}
            className="w-full"
            onClick={async () => {
              await qc.invalidateQueries()
              navigate({ to: '/' })
            }}
          >
            I've saved them, continue
          </Button>
        </div>
      </AuthShell>
    )
  }

  if (checkError) {
    return (
      <AuthShell title="Invite not valid" subtitle="Ask whoever invited you for a new link.">
        <ErrorNote error={checkError} />
      </AuthShell>
    )
  }

  return (
    <AuthShell title="Join TainerHub" subtitle={role ? `You've been invited as ${role === 'admin' ? 'an' : 'a'} ${role}. Choose how you'll sign in.` : 'Checking your invite…'}>
      <form
        className="space-y-4"
        onSubmit={async (e) => {
          e.preventDefault()
          if (password !== confirm) return setError("The passwords don't match.")
          setBusy(true)
          setError(null)
          try {
            const res = await api<{ user: User; recoveryCodes: string[] }>('/api/invite/accept', { method: 'POST', body: { token, username, displayName, password } })
            window.history.replaceState(null, '', '/invite') // drop the one-time token
            setCodes(res.recoveryCodes)
          } catch (err) {
            setError(errorText(err))
          } finally {
            setBusy(false)
          }
        }}
      >
        <Input label="Username" value={username} onChange={(e) => setUsername(e.target.value)} autoFocus required autoComplete="username" />
        <Input label="Display name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="How others see you" />
        <Input label="Password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={10} autoComplete="new-password" hint="At least 10 characters." />
        <Input label="Confirm password" type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required autoComplete="new-password" />
        <ErrorNote error={error} />
        <Button variant="primary" icon={UserPlus} className="w-full" disabled={!role || !username || !password} loading={busy}>
          Create my account
        </Button>
      </form>
    </AuthShell>
  )
}
