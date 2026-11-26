import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
  type RefObject,
} from 'react'
import { createPortal } from 'react-dom'
import { Check, CircleAlert, CircleCheck, Copy, Download, LoaderCircle, X, type LucideIcon } from 'lucide-react'

export const cx = (...c: (string | false | null | undefined)[]) => c.filter(Boolean).join(' ')

// ---------------- buttons & inputs ----------------

type Variant = 'primary' | 'secondary' | 'danger' | 'ghost' | 'dangerGhost'

// Solid variants are "tags" (see .tag in index.css): an outer layer that
// paints the 1px edge and an inner layer that paints the fill, both with the
// top-right and bottom-left corners clipped.
const tagged: Partial<Record<Variant, { edge: string; fill: string }>> = {
  primary: { edge: 'bg-lime-400 group-hover:bg-lime-300', fill: 'bg-lime-400 text-ink-950 group-hover:bg-lime-300' },
  secondary: { edge: 'bg-ink-500/70 group-hover:bg-lime-400/80', fill: 'bg-ink-900 text-ink-50 group-hover:text-lime-200' },
  danger: { edge: 'bg-rose-400', fill: 'bg-rose-500 text-white group-hover:bg-rose-400' },
}
const plain: Partial<Record<Variant, string>> = {
  ghost: 'text-ink-100/75 hover:bg-white/[0.06] hover:text-lime-200',
  dangerGhost: 'text-rose-300 hover:bg-rose-500/10 hover:text-rose-200',
}
const label = 'font-display text-[12.5px] font-semibold uppercase tracking-[0.08em]'

export function Button({
  variant = 'secondary',
  icon: Icon,
  loading,
  className,
  children,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; icon?: LucideIcon; loading?: boolean }) {
  const inner = (
    <>
      {loading ? <LoaderCircle className="size-4 animate-spin" /> : Icon && <Icon className="size-4" strokeWidth={2.25} />}
      {children}
    </>
  )
  const t = tagged[variant]
  const base = 'select-none transition duration-150 active:translate-y-px disabled:pointer-events-none disabled:opacity-45'
  if (!t) {
    return (
      <button
        {...props}
        disabled={props.disabled || loading}
        className={cx(
          base,
          label,
          'inline-flex items-center justify-center gap-2 px-3.5 py-2 focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-lime-400',
          plain[variant],
          className,
        )}
      >
        {inner}
      </button>
    )
  }
  return (
    <button
      {...props}
      disabled={props.disabled || loading}
      className={cx(base, 'group tag inline-flex p-px outline-none focus-visible:!bg-white', t.edge, className)}
    >
      <span className={cx('tag-fill inline-flex w-full items-center justify-center gap-2 px-4 py-[7px] transition-colors duration-150', label, t.fill)}>{inner}</span>
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
        'tag tag-sm inline-flex size-8 items-center justify-center text-ink-100/60 transition focus-visible:bg-lime-400/20 focus-visible:outline-none disabled:pointer-events-none disabled:opacity-40',
        tones[tone],
        className,
      )}
    >
      {loading ? <LoaderCircle className="size-4 animate-spin" /> : <Icon className="size-4" />}
    </button>
  )
}

export function Input({ label, hint, icon: Icon, className, ...props }: InputHTMLAttributes<HTMLInputElement> & { label?: string; hint?: string; icon?: LucideIcon }) {
  // Explicit ids, so screen readers announce the label (not the placeholder)
  // and read the hint as the field's description.
  const auto = useId()
  const id = props.id ?? auto
  return (
    <label className="block" htmlFor={id}>
      {label && <span className="mb-1.5 block text-sm font-medium text-ink-100/80" id={`${id}-label`}>{label}</span>}
      <span className="relative block">
        {Icon && <Icon className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-ink-100/50" />}
        <input
          {...props}
          id={id}
          aria-labelledby={props['aria-label'] || !label ? props['aria-labelledby'] : `${id}-label`}
          aria-describedby={hint ? `${id}-hint` : props['aria-describedby']}
          className={cx(
            'w-full rounded-[2px] bg-ink-950/70 px-3 py-2 text-sm text-white ring-1 ring-inset ring-white/12 outline-none transition placeholder:text-ink-100/35 focus:shadow-[inset_3px_0_0] focus:shadow-lime-400 focus:ring-lime-400/60',
            Icon && 'pl-9',
            className,
          )}
        />
      </span>
      {hint && <span className="mt-1.5 block text-xs text-ink-100/50" id={`${id}-hint`}>{hint}</span>}
    </label>
  )
}

export function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="rounded-[2px] border border-white/10 border-b-2 bg-white/5 px-1.5 py-0.5 font-mono text-[11px] text-ink-100/60">{children}</kbd>
}

// ---------------- surfaces ----------------

export function Card({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cx('rounded-md bg-ink-900/55 ring-1 ring-line', className)}>{children}</div>
}

export function PageHeader({ title, subtitle, children }: { title: string; subtitle?: ReactNode; children?: ReactNode }) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 className="font-display text-3xl font-bold tracking-tight text-white">{title}</h1>
        {subtitle && <p className="mt-1 text-sm text-ink-100/70">{subtitle}</p>}
      </div>
      <div className="flex items-center gap-2">{children}</div>
    </div>
  )
}

export function ErrorNote({ error }: { error?: string | null }) {
  if (!error) return null
  return (
    <div className="flex items-start gap-2 rounded-[3px] bg-rose-500/10 px-3 py-2.5 text-sm text-rose-200 ring-1 ring-inset ring-rose-500/25">
      <CircleAlert className="mt-0.5 size-4 shrink-0" />
      <span>{error}</span>
    </div>
  )
}

export function EmptyState({ icon: Icon, title, children }: { icon: LucideIcon; title: string; children?: ReactNode }) {
  return (
    <Card className="flex flex-col items-center px-6 py-14 text-center">
      <div className="tag mb-4 bg-lime-400/12 p-4">
        <Icon className="size-7 text-lime-300" />
      </div>
      <h2 className="font-display text-xl font-bold text-white">{title}</h2>
      <div className="mt-2 max-w-md text-sm text-ink-100/70">{children}</div>
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
  info: 'bg-lime-400',
  accent: 'bg-cyan-400',
  idle: 'bg-ink-500',
}
const dotGlow: Record<Tone, string> = {
  good: '',
  warn: '',
  bad: '',
  info: '',
  accent: '',
  idle: '',
}

export function StatusDot({ tone, pulse }: { tone: Tone; pulse?: boolean }) {
  return (
    <span className="relative inline-flex size-2.5 shrink-0 items-center justify-center">
      {pulse && <span className={cx('absolute size-2 rotate-45 animate-ping opacity-50', dotColor[tone])} />}
      <span className={cx('relative inline-flex size-2 rotate-45', dotColor[tone], dotGlow[tone])} />
    </span>
  )
}

const badgeTone: Record<Tone, string> = {
  good: 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/25',
  warn: 'bg-amber-500/10 text-amber-300 ring-amber-500/25',
  bad: 'bg-rose-500/10 text-rose-300 ring-rose-500/25',
  info: 'bg-lime-500/10 text-lime-300 ring-lime-500/25',
  accent: 'bg-cyan-500/10 text-cyan-300 ring-cyan-500/25',
  idle: 'bg-white/[0.04] text-ink-100/60 ring-white/10',
}

export function Badge({ tone = 'idle', children, className }: { tone?: Tone; children: ReactNode; className?: string }) {
  return <span className={cx('inline-flex items-center gap-1 rounded-[2px] px-1.5 py-[3px] font-mono text-[10.5px] font-medium uppercase leading-none tracking-wider ring-1 ring-inset', badgeTone[tone], className)}>{children}</span>
}

export function StateBadge({ state, status }: { state: string; status?: string }) {
  const tone = toneOf(state, status)
  return <Badge tone={tone}>{status?.includes('(unhealthy)') ? 'unhealthy' : state}</Badge>
}

/** Horizontal stacked bar, e.g. running / stopped / unhealthy. */
export function SegmentBar({ parts, className }: { parts: { value: number; className: string; label: string }[]; className?: string }) {
  const total = parts.reduce((a, p) => a + p.value, 0)
  return (
    <div className={cx('flex h-1.5 gap-px overflow-hidden bg-white/[0.06]', className)} role="img" aria-label={parts.map((p) => `${p.value} ${p.label}`).join(', ')}>
      {total > 0 &&
        parts.map((p) => (p.value > 0 ? <div key={p.label} className={cx('h-full transition-all duration-500', p.className)} style={{ flexGrow: p.value }} /> : null))}
    </div>
  )
}

// ---------------- copy / codes ----------------

/**
 * Copies text. navigator.clipboard only exists on HTTPS pages (and
 * localhost), so on a plain-HTTP install fall back to the old
 * select-and-copy trick, which works everywhere.
 */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (window.isSecureContext && navigator.clipboard) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* fall through */
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.top = '-1000px'
  document.body.appendChild(ta)
  ta.select()
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch {
    ok = false
  }
  ta.remove()
  return ok
}

export function CopyButton({ text, label = 'Copy', variant }: { text: string; label?: string; variant?: Variant }) {
  const [state, setState] = useState<'idle' | 'done' | 'failed'>('idle')
  return (
    <Button
      variant={variant}
      icon={state === 'done' ? Check : Copy}
      onClick={async () => {
        setState((await copyText(text)) ? 'done' : 'failed')
        setTimeout(() => setState('idle'), 2000)
      }}
    >
      {state === 'done' ? 'Copied' : state === 'failed' ? "Couldn't copy: select it instead" : label}
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
      <p className="text-sm text-ink-100/80">
        Store these in your password manager. Each code signs you in once if you lose your password or passkeys.{' '}
        <strong className="text-white">They won't be shown again.</strong>
      </p>
      <div className="mono grid grid-cols-2 gap-x-6 gap-y-2 rounded-md bg-black/30 p-4 text-sm tracking-wider text-ink-50 ring-1 ring-white/10">
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

// ---------------- layers: Escape, focus and inert ----------------

/*
 * Every overlay (dialog, drawer, menu, palette) registers as a layer while
 * it is open. Escape only reaches the topmost layer. Modal layers also keep
 * Tab inside themselves, move focus in when they open, give it back to
 * whatever had it when they close, and make the rest of the page inert.
 */
type Layer = { id: number; node: () => HTMLElement | null; modal: boolean; onEscape: () => boolean | void; restore: HTMLElement | null }
const layers: Layer[] = []
let nextLayer = 1
let lastFocusFrom: HTMLElement | null = null

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

function focusables(node: HTMLElement): HTMLElement[] {
  return [...node.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((el) => !el.closest('[inert]') && el.getClientRects().length > 0)
}

function topModal(): number {
  for (let i = layers.length - 1; i >= 0; i--) if (layers[i].modal) return i
  return -1
}

/** Makes everything under <body> inert except the top modal layer and anything opened above it. */
function syncInert() {
  const m = topModal()
  const keep = m < 0 ? [] : layers.slice(m).map((l) => l.node()).filter((n): n is HTMLElement => !!n)
  for (const child of [...document.body.children] as HTMLElement[]) {
    const off = m >= 0 && !child.hasAttribute('data-layer-keep') && !keep.some((n) => child.contains(n))
    if (off) child.setAttribute('inert', '')
    else child.removeAttribute('inert')
  }
}

function onLayerKey(e: KeyboardEvent) {
  const top = layers[layers.length - 1]
  if (!top) return
  if (e.key === 'Escape') {
    if (top.onEscape() === false) return
    e.preventDefault()
    e.stopPropagation()
  } else if (e.key === 'Tab' && top.modal) {
    const node = top.node()
    if (!node) return
    const els = focusables(node)
    if (els.length === 0) {
      e.preventDefault()
      node.focus()
      return
    }
    const first = els[0]
    const last = els[els.length - 1]
    const active = document.activeElement as HTMLElement | null
    if (!active || !node.contains(active)) {
      e.preventDefault()
      first.focus()
    } else if (e.shiftKey && active === first) {
      e.preventDefault()
      last.focus()
    } else if (!e.shiftKey && active === last) {
      e.preventDefault()
      first.focus()
    }
  }
}

if (typeof window !== 'undefined') {
  window.addEventListener('keydown', onLayerKey, true)
  document.addEventListener('focusin', (e) => {
    lastFocusFrom = e.relatedTarget instanceof HTMLElement ? e.relatedTarget : null
  })
}

/**
 * Registers an open overlay. `ref` is its outermost element (portaled to
 * <body> for modal layers, so the rest of the page can be made inert).
 * onEscape may return false to leave Escape alone.
 */
export function useLayer(open: boolean, ref: RefObject<HTMLElement | null>, onEscape: () => boolean | void, opts: { modal?: boolean; initialFocus?: boolean } = {}) {
  const esc = useRef(onEscape)
  esc.current = onEscape
  const { modal = false, initialFocus = modal } = opts
  // Whatever had focus when the layer was first rendered open: the trigger, usually.
  const opener = useRef<HTMLElement | null | undefined>(undefined)
  if (!open) opener.current = undefined
  else if (opener.current === undefined) opener.current = document.activeElement as HTMLElement | null
  useEffect(() => {
    if (!open) return
    const node = ref.current
    const active = document.activeElement as HTMLElement | null
    // If focus already moved inside (autoFocus), give it back to where it came from.
    const inside = !!node && !!active && node.contains(active)
    const restore = inside ? (opener.current && !node.contains(opener.current) ? opener.current : lastFocusFrom) : active
    const layer: Layer = { id: nextLayer++, node: () => ref.current, modal, onEscape: () => esc.current(), restore }
    layers.push(layer)
    syncInert()
    if (initialFocus && node && !node.contains(document.activeElement)) {
      const target = node.querySelector<HTMLElement>('[data-autofocus]') ?? focusables(node).find((el) => !el.hasAttribute('data-no-initial-focus')) ?? node
      target.focus({ preventScroll: true })
    }
    return () => {
      const i = layers.indexOf(layer)
      if (i >= 0) layers.splice(i, 1)
      syncInert()
      if (!initialFocus) return
      const now = document.activeElement
      const lost = !now || now === document.body || !now.isConnected || (node?.contains(now) ?? false)
      if (lost && layer.restore?.isConnected) layer.restore.focus({ preventScroll: true })
    }
  }, [open, modal, initialFocus, ref])
}

// ---------------- modal ----------------

export function Modal({ open, onClose, title, children, wide }: { open: boolean; onClose: () => void; title: string; children: ReactNode; wide?: boolean }) {
  const ref = useRef<HTMLDivElement>(null)
  const titleId = useId()
  useLayer(open, ref, onClose, { modal: true })
  if (!open) return null
  return createPortal(
    <div ref={ref} className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-black/70" onClick={onClose} />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className={cx('relative max-h-[90vh] w-full overflow-y-auto rounded-md border-t-2 border-lime-400 bg-panel p-6 shadow-2xl shadow-black/60 ring-1 ring-line outline-none animate-[pop_150ms_ease-out]', wide ? 'max-w-2xl' : 'max-w-md')}
      >
        <div className="mb-4 flex items-center justify-between">
          <h2 id={titleId} className="font-display text-xl font-bold text-white">
            {title}
          </h2>
          <IconButton icon={X} label="Close" onClick={onClose} data-no-initial-focus />
        </div>
        {children}
      </div>
    </div>,
    document.body,
  )
}

// ---------------- confirm dialog ----------------

type ConfirmOpts = {
  title: string
  body?: ReactNode
  /** The confirm button's label; null shows only a Close button (the dialog explains why it can't be done). */
  confirm?: string | null
  danger?: boolean
  /** A checkbox the user must tick before the confirm button works. */
  ack?: ReactNode
}
const ConfirmCtx = createContext<(o: ConfirmOpts) => Promise<boolean>>(async () => false)
export const useConfirm = () => useContext(ConfirmCtx)

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<ConfirmOpts | null>(null)
  const [acked, setAcked] = useState(false)
  const pending = useRef<((v: boolean) => void) | null>(null)
  const ask = useCallback(
    (o: ConfirmOpts) =>
      new Promise<boolean>((resolve) => {
        // A new question replaces any open one: that one counts as cancelled.
        pending.current?.(false)
        pending.current = resolve
        setAcked(false)
        setState(o)
      }),
    [],
  )
  const close = useCallback((v: boolean) => {
    const resolve = pending.current
    pending.current = null
    setState(null)
    resolve?.(v)
  }, [])
  const cancel = useCallback(() => close(false), [close])
  return (
    <ConfirmCtx.Provider value={ask}>
      {children}
      <Modal open={!!state} onClose={cancel} title={state?.title ?? ''}>
        {state?.body && <div className="mb-6 text-sm leading-relaxed text-ink-100/80">{state.body}</div>}
        {state?.ack && state.confirm !== null && (
          <label className="-mt-2 mb-6 flex items-start gap-2.5 rounded-[3px] bg-rose-500/[0.07] p-3 text-sm ring-1 ring-inset ring-rose-500/20">
            <input type="checkbox" checked={acked} onChange={(e) => setAcked(e.target.checked)} className="mt-0.5 accent-rose-500" />
            <span>{state.ack}</span>
          </label>
        )}
        <div className="flex justify-end gap-2">
          {state?.confirm === null ? (
            <Button variant="primary" onClick={cancel} autoFocus>
              Close
            </Button>
          ) : (
            <>
              <Button variant="ghost" onClick={cancel} autoFocus={!!state?.ack}>
                Cancel
              </Button>
              <Button variant={state?.danger ? 'danger' : 'primary'} onClick={() => close(true)} disabled={!!state?.ack && !acked} autoFocus={!state?.ack}>
                {state?.confirm ?? 'Confirm'}
              </Button>
            </>
          )}
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
      {createPortal(
        <div data-layer-keep className="pointer-events-none fixed bottom-4 right-4 z-[60] flex w-80 flex-col gap-2" aria-live="polite">
          {toasts.map((t) => (
            <div
              key={t.id}
              className={cx('pointer-events-auto flex items-start gap-2.5 rounded-[3px] border-l-2 bg-panel px-4 py-3 text-sm text-white shadow-2xl ring-1 ring-line animate-[slide_200ms_ease-out]', t.tone === 'good' ? 'border-emerald-400' : 'border-rose-400')}
            >
              {t.tone === 'good' ? <CircleCheck className="mt-0.5 size-4 shrink-0 text-emerald-400" /> : <CircleAlert className="mt-0.5 size-4 shrink-0 text-rose-400" />}
              <span className="flex-1">{t.text}</span>
            </div>
          ))}
        </div>,
        document.body,
      )}
    </ToastCtx.Provider>
  )
}

// ---------------- dropdown menu ----------------

export type MenuItem = { label: string; icon: LucideIcon; onClick: () => void; danger?: boolean; hidden?: boolean }
/** Spread onto the menu's trigger button. */
export type MenuTriggerProps = { 'aria-haspopup': 'menu'; 'aria-expanded': boolean; 'aria-controls'?: string }

export function Menu({ trigger, items }: { trigger: (toggle: () => void, props: MenuTriggerProps) => ReactNode; items: MenuItem[] }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  const list = useRef<HTMLDivElement>(null)
  const id = useId()
  const triggerEl = () => ref.current?.querySelector<HTMLElement>('[aria-haspopup]') ?? null
  const close = useCallback((refocus: boolean) => {
    setOpen(false)
    if (refocus) triggerEl()?.focus()
  }, [])
  useLayer(open, list, () => close(true))
  useEffect(() => {
    if (!open) return
    list.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus()
    const onDown = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false)
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [open])
  const onKey = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const els = [...(list.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])]
    const i = els.indexOf(document.activeElement as HTMLElement)
    const go = (n: number) => {
      e.preventDefault()
      els[(n + els.length) % els.length]?.focus()
    }
    if (e.key === 'ArrowDown') go(i + 1)
    else if (e.key === 'ArrowUp') go(i - 1)
    else if (e.key === 'Home') go(0)
    else if (e.key === 'End') go(els.length - 1)
    else if (e.key === 'Tab') setOpen(false)
  }
  return (
    <div ref={ref} className="relative">
      {trigger(() => setOpen((o) => !o), { 'aria-haspopup': 'menu', 'aria-expanded': open, 'aria-controls': open ? id : undefined })}
      {open && (
        <div
          ref={list}
          id={id}
          role="menu"
          onKeyDown={onKey}
          className="absolute right-0 top-9 z-40 min-w-44 overflow-hidden rounded-[3px] bg-panel p-1 shadow-2xl shadow-black/50 ring-1 ring-line animate-[pop_120ms_ease-out]"
        >
          {items
            .filter((i) => !i.hidden)
            .map((i) => (
              <button
                key={i.label}
                role="menuitem"
                tabIndex={-1}
                onClick={() => {
                  close(true)
                  i.onClick()
                }}
                className={cx(
                  'flex w-full items-center gap-2.5 rounded-[2px] border-l-2 border-transparent px-3 py-2 text-left text-sm outline-none transition',
                  i.danger
                    ? 'text-rose-300 hover:border-rose-400 hover:bg-rose-500/10 focus:border-rose-400 focus:bg-rose-500/10'
                    : 'text-ink-50 hover:border-lime-400 hover:bg-white/[0.05] focus:border-lime-400 focus:bg-white/[0.05]',
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
