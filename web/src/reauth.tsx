import { useEffect, useState } from 'react'
import { useRouter } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { LogIn, ShieldAlert } from 'lucide-react'
import { api, onReauthNeeded } from './api'
import { Button, Modal } from './ui'

/** Signs out and goes to the sign-in page, coming back to this page afterwards. */
export function useSignInAgain() {
  const qc = useQueryClient()
  const router = useRouter()
  const [busy, setBusy] = useState(false)
  const go = async () => {
    setBusy(true)
    const here = window.location.pathname + window.location.search
    try {
      await api('/api/logout', { method: 'POST' })
    } catch {
      /* the sign-in page works either way */
    } finally {
      qc.clear()
      router.history.push('/login?next=' + encodeURIComponent(here))
    }
  }
  return { go, busy }
}

/**
 * Shown whenever the hub answers 403 {reauth: true}: account changes,
 * invites, role changes and other sensitive actions need a recent sign-in.
 */
export function ReauthPrompt() {
  const [open, setOpen] = useState(false)
  const { go, busy } = useSignInAgain()
  useEffect(() => onReauthNeeded(() => setOpen(true)), [])
  return (
    <Modal open={open} onClose={() => setOpen(false)} title="Sign in again to continue">
      <div className="mb-6 flex items-start gap-3 text-sm leading-relaxed text-ink-100/80">
        <div className="rounded-[3px] bg-amber-500/10 p-2.5 text-amber-300 ring-1 ring-inset ring-amber-500/20">
          <ShieldAlert className="size-5" />
        </div>
        <p>For your safety, this change needs a sign-in from the last 10 minutes. Sign in again, and you'll come back here to try once more.</p>
      </div>
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={() => setOpen(false)}>
          Not now
        </Button>
        <Button variant="primary" icon={LogIn} loading={busy} onClick={go} autoFocus>
          Sign in again
        </Button>
      </div>
    </Modal>
  )
}
