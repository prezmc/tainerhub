import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
} from 'react'
import { Check, CircleAlert, CircleCheck, Copy, Download, LoaderCircle, X, type LucideIcon } from 'lucide-react'

export const cx = (...c: (string | false | null | undefined)[]) => c.filter(Boolean).join(' ')

// ---------------- buttons & inputs ----------------

type Variant = 'primary' | 'secondary' | 'danger' | 'ghost' | 'dangerGhost'

const variants: Record<Variant, string> = {
  primary:
    'bg-gradient-to-r from-sky-500 to-purple-500 text-white shadow-lg shadow-purple-950/50 ring-1 ring-inset ring-white/20 hover:brightness-110',
  secondary: 'bg-white/[0.07] text-white ring-1 ring-inset ring-white/15 backdrop-blur hover:bg-white/[0.12]',
  danger: 'bg-gradient-to-r from-rose-500 to-orange-500 text-white shadow-lg shadow-rose-950/40 ring-1 ring-inset ring-white/20 hover:brightness-110',
  ghost: 'text-indigo-100/80 hover:bg-white/[0.06] hover:text-white',
  dangerGhost: 'text-rose-300 hover:bg-rose-500/10 hover:text-rose-200',
}

export function Button({
  variant = 'secondary',
  icon: Icon,
  loading,
  className,
  children,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; icon?: LucideIcon; loading?: boolean }) {
  return (
    <button
      {...props}
      disabled={props.disabled || loading}
      className={cx(
        'inline-flex select-none items-center justify-center gap-2 rounded-full px-4 py-2 text-sm font-semibold transition duration-150 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-sky-400 active:scale-[0.98] disabled:pointer-events-none disabled:opacity-50',
        variants[variant],
        className,
      )}
    >
      {loading ? <LoaderCircle className="size-4 animate-spin" /> : Icon && <Icon className="size-4" />}
      {children}
    </button>
  )
}

export function IconButton({
  icon: Icon,
  label,
  tone = 'default',
  loading,
  className,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { icon: LucideIcon; label: string; tone?: 'default' | 'good' | 'warn' | 'bad'; loading?: boolean }) {
  const tones = {
    default: 'hover:bg-white/[0.08] hover:text-white',
    good: 'hover:bg-emerald-500/15 hover:text-emerald-300',
    warn: 'hover:bg-amber-500/15 hover:text-amber-300',
    bad: 'hover:bg-rose-500/15 hover:text-rose-300',
  }
  return (
    <button
      {...props}
      aria-label={label}
      title={label}
      disabled={props.disabled || loading}
      className={cx(
        'inline-flex size-8 items-center justify-center rounded-lg text-indigo-100/60 transition focus-visible:outline-2 focus-visible:outline-sky-400 disabled:pointer-events-none disabled:opacity-40',
        tones[tone],
        className,
      )}
    >
      {loading ? <LoaderCircle className="size-4 animate-spin" /> : <Icon className="size-4" />}
    </button>
  )
}

export function Input({ label, hint, icon: Icon, className, ...props }: InputHTMLAttributes<HTMLInputElement> & { label?: string; hint?: string; icon?: LucideIcon }) {
  return (
    <label className="block">
      {label && <span className="mb-1.5 block text-sm font-medium text-indigo-100/80">{label}</span>}
      <span className="relative block">
        {Icon && <Icon className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-indigo-100/50" />}
        <input
          {...props}
          className={cx(
            'w-full rounded-xl bg-white/[0.06] px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/15 outline-none transition placeholder:text-indigo-100/40 focus:ring-2 focus:ring-purple-400/70',
            Icon && 'pl-9',
            className,
          )}
        />
      </span>
      {hint && <span className="mt-1.5 block text-xs text-indigo-100/50">{hint}</span>}
    </label>
  )
}

export function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="rounded border border-white/10 bg-white/5 px-1.5 py-0.5 font-sans text-[11px] text-indigo-100/60">{children}</kbd>
}

// ---------------- surfaces ----------------

export function Card({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cx('rounded-3xl bg-white/[0.05] ring-1 ring-white/[0.11] backdrop-blur-xl', className)}>{children}</div>
}

export function PageHeader({ title, subtitle, children }: { title: string; subtitle?: ReactNode; children?: ReactNode }) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 className="font-display text-3xl font-bold tracking-tight text-white">{title}</h1>
        {subtitle && <p className="mt-1 text-sm text-indigo-100/70">{subtitle}</p>}
      </div>
      <div className="flex items-center gap-2">{children}</div>
    </div>
  )
}

export function ErrorNote({ error }: { error?: string | null }) {
  if (!error) return null
  return (
    <div className="flex items-start gap-2 rounded-lg bg-rose-500/10 px-3 py-2.5 text-sm text-rose-200 ring-1 ring-inset ring-rose-500/25">
      <CircleAlert className="mt-0.5 size-4 shrink-0" />
      <span>{error}</span>
    </div>
  )
}

export function EmptyState({ icon: Icon, title, children }: { icon: LucideIcon; title: string; children?: ReactNode }) {
  return (
    <Card className="flex flex-col items-center px-6 py-14 text-center">
      <div className="mb-4 rounded-2xl bg-gradient-to-br from-emerald-500/20 via-sky-500/20 to-indigo-500/20 p-4 ring-1 ring-white/10">
        <Icon className="size-7 text-sky-300" />
      </div>
      <h2 className="font-display text-xl font-bold text-white">{title}</h2>
      <div className="mt-2 max-w-md text-sm text-indigo-100/70">{children}</div>
    </Card>
  )
}

// ---------------- status ----------------

export type Tone = 'good' | 'warn' | 'bad' | 'idle' | 'info' | 'accent'

export const toneOf = (state: string, status?: string): Tone => {
  if (status?.includes('(unhealthy)') || state === 'dead') return 'bad'
  if (state === 'running') return 'good'
  if (state === 'paused' || state === 'restarting') return 'warn'
  if (state === 'created') return 'info'
  return 'idle'
}

const dotColor: Record<Tone, string> = {
  good: 'bg-emerald-400',
  warn: 'bg-amber-400',
  bad: 'bg-rose-400',
  info: 'bg-sky-400',
  accent: 'bg-purple-400',
  idle: 'bg-slate-500',
}
const dotGlow: Record<Tone, string> = {
  good: 'shadow-[0_0_8px_1px] shadow-emerald-500/50',
  warn: 'shadow-[0_0_8px_1px] shadow-amber-500/50',
  bad: 'shadow-[0_0_8px_1px] shadow-rose-500/60',
  info: '',
  accent: '',
  idle: '',
}

export function StatusDot({ tone, pulse }: { tone: Tone; pulse?: boolean }) {
  return (
    <span className="relative inline-flex size-2.5 shrink-0">
      {pulse && <span className={cx('absolute inset-0 animate-ping rounded-full opacity-50', dotColor[tone])} />}
      <span className={cx('relative inline-flex size-2.5 rounded-full', dotColor[tone], dotGlow[tone])} />
    </span>
  )
}

const badgeTone: Record<Tone, string> = {
  good: 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/25',
  warn: 'bg-amber-500/10 text-amber-300 ring-amber-500/25',
  bad: 'bg-rose-500/10 text-rose-300 ring-rose-500/25',
  info: 'bg-sky-500/10 text-sky-300 ring-sky-500/25',
  accent: 'bg-purple-500/10 text-purple-300 ring-purple-500/25',
  idle: 'bg-white/[0.04] text-indigo-100/60 ring-white/10',
}

export function Badge({ tone = 'idle', children, className }: { tone?: Tone; children: ReactNode; className?: string }) {
  return <span className={cx('inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-xs font-medium ring-1 ring-inset', badgeTone[tone], className)}>{children}</span>
}

export function StateBadge({ state, status }: { state: string; status?: string }) {
  const tone = toneOf(state, status)
  return <Badge tone={tone}>{status?.includes('(unhealthy)') ? 'unhealthy' : state}</Badge>
}

/** Horizontal stacked bar, e.g. running / stopped / unhealthy. */
export function SegmentBar({ parts, className }: { parts: { value: number; className: string; label: string }[]; className?: string }) {
  const total = parts.reduce((a, p) => a + p.value, 0)
  return (
    <div className={cx('flex h-1.5 gap-px overflow-hidden rounded-full bg-white/[0.06]', className)} role="img" aria-label={parts.map((p) => `${p.value} ${p.label}`).join(', ')}>
      {total > 0 &&
        parts.map((p) => (p.value > 0 ? <div key={p.label} className={cx('h-full transition-all duration-500', p.className)} style={{ flexGrow: p.value }} /> : null))}
    </div>
  )
}

// ---------------- copy / codes ----------------

export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [done, setDone] = useState(false)
  return (
    <Button
      icon={done ? Check : Copy}
      onClick={async () => {
        await navigator.clipboard.writeText(text)
        setDone(true)
        setTimeout(() => setDone(false), 1500)
      }}
    >
      {done ? 'Copied' : label}
    </Button>
  )
}

export function download(name: string, text: string) {
  const a = document.createElement('a')
  a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
  a.download = name
  a.click()
  setTimeout(() => URL.revokeObjectURL(a.href), 1000)
}

export function RecoveryCodes({ codes }: { codes: string[] }) {
  const text = codes.join('\n')
  return (
    <div className="space-y-4">
      <p className="text-sm text-indigo-100/80">
        Store these in your password manager. Each code signs you in once if you lose your passkeys.{' '}
        <strong className="text-white">They won't be shown again.</strong>
      </p>
      <div className="mono grid grid-cols-2 gap-x-6 gap-y-2 rounded-2xl bg-black/30 p-4 text-sm tracking-wider text-indigo-50 ring-1 ring-white/10">
        {codes.map((c) => (
          <span key={c}>{c}</span>
        ))}
      </div>
      <div className="flex gap-2">
        <CopyButton text={text} label="Copy codes" />
        <Button icon={Download} onClick={() => download('tainerhub-recovery-codes.txt', `TainerHub recovery codes\n\n${text}\n`)}>
          Download
        </Button>
      </div>
    </div>
  )
}

// ---------------- modal ----------------

export function Modal({ open, onClose, title, children, wide }: { open: boolean; onClose: () => void; title: string; children: ReactNode; wide?: boolean }) {
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])
  if (!open) return null
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-black/60 backdrop-blur-sm" onClick={onClose} />
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className={cx('relative max-h-[90vh] w-full overflow-y-auto rounded-3xl bg-[#140f33]/95 p-6 shadow-2xl shadow-black/60 ring-1 ring-white/15 backdrop-blur-xl animate-[pop_150ms_ease-out]', wide ? 'max-w-2xl' : 'max-w-md')}
      >
        <div className="mb-4 flex items-center justify-between">
          <h2 className="font-display text-xl font-bold text-white">{title}</h2>
          <IconButton icon={X} label="Close" onClick={onClose} />
        </div>
        {children}
      </div>
    </div>
  )
}

// ---------------- confirm dialog ----------------

type ConfirmOpts = { title: string; body?: ReactNode; confirm?: string; danger?: boolean }
const ConfirmCtx = createContext<(o: ConfirmOpts) => Promise<boolean>>(async () => false)
export const useConfirm = () => useContext(ConfirmCtx)

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<(ConfirmOpts & { resolve: (v: boolean) => void }) | null>(null)
  const ask = useCallback((o: ConfirmOpts) => new Promise<boolean>((resolve) => setState({ ...o, resolve })), [])
  const close = useCallback(
    (v: boolean) => {
      setState((s) => {
        s?.resolve(v)
        return null
      })
    },
    [],
  )
  const cancel = useCallback(() => close(false), [close])
  return (
    <ConfirmCtx.Provider value={ask}>
      {children}
      <Modal open={!!state} onClose={cancel} title={state?.title ?? ''}>
        {state?.body && <div className="mb-6 text-sm leading-relaxed text-indigo-100/80">{state.body}</div>}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={cancel}>
            Cancel
          </Button>
          <Button variant={state?.danger ? 'danger' : 'primary'} onClick={() => close(true)} autoFocus>
            {state?.confirm ?? 'Confirm'}
          </Button>
        </div>
      </Modal>
    </ConfirmCtx.Provider>
  )
}

// ---------------- toasts ----------------

type Toast = { id: number; tone: 'good' | 'bad'; text: string }
const ToastCtx = createContext<(tone: Toast['tone'], text: string) => void>(() => {})
export const useToast = () => useContext(ToastCtx)

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const next = useRef(1)
  const push = useCallback((tone: Toast['tone'], text: string) => {
    const id = next.current++
    setToasts((t) => [...t.slice(-3), { id, tone, text }])
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), tone === 'bad' ? 7000 : 3500)
  }, [])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="pointer-events-none fixed bottom-4 right-4 z-[60] flex w-80 flex-col gap-2" aria-live="polite">
        {toasts.map((t) => (
          <div
            key={t.id}
            className="pointer-events-auto flex items-start gap-2.5 rounded-2xl bg-[#140f33]/95 px-4 py-3 text-sm text-white shadow-2xl ring-1 ring-white/10 backdrop-blur animate-[slide_200ms_ease-out]"
          >
            {t.tone === 'good' ? <CircleCheck className="mt-0.5 size-4 shrink-0 text-emerald-400" /> : <CircleAlert className="mt-0.5 size-4 shrink-0 text-rose-400" />}
            <span className="flex-1">{t.text}</span>
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  )
}

// ---------------- dropdown menu ----------------

export type MenuItem = { label: string; icon: LucideIcon; onClick: () => void; danger?: boolean; hidden?: boolean }

export function Menu({ trigger, items }: { trigger: (toggle: () => void) => ReactNode; items: MenuItem[] }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false)
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && setOpen(false)
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])
  return (
    <div ref={ref} className="relative">
      {trigger(() => setOpen((o) => !o))}
      {open && (
        <div role="menu" className="absolute right-0 top-9 z-40 min-w-44 overflow-hidden rounded-2xl bg-[#140f33]/95 p-1 shadow-2xl ring-1 ring-white/15 backdrop-blur-xl animate-[pop_120ms_ease-out]">
          {items
            .filter((i) => !i.hidden)
            .map((i) => (
              <button
                key={i.label}
                role="menuitem"
                onClick={() => {
                  setOpen(false)
                  i.onClick()
                }}
                className={cx(
                  'flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-left text-sm transition',
                  i.danger ? 'text-rose-300 hover:bg-rose-500/10' : 'text-indigo-50 hover:bg-white/[0.06]',
                )}
              >
                <i.icon className="size-4 opacity-80" />
                {i.label}
              </button>
            ))}
        </div>
      )}
    </div>
  )
}
