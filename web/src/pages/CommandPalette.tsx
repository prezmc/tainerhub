import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import {
  Boxes,
  CornerDownLeft,
  Fingerprint,
  Info,
  Layers,
  LayoutDashboard,
  Pause,
  Play,
  RotateCw,
  ScrollText,
  Search,
  Server,
  ShieldCheck,
  Square,
  Trash2,
  Zap,
  type LucideIcon,
} from 'lucide-react'
import { can, type Container } from '../api'
import { ACTIONS, useContainerAction, useFleet, type Action } from '../fleet'
import { Kbd, StatusDot, cx, toneOf } from '../ui'
import { useMe } from './AppLayout'

type Item = { id: string; icon: LucideIcon; label: string; hint?: string; tone?: 'danger'; run: () => void; dot?: Container }

const actionIcons: Record<Action, LucideIcon> = { start: Play, stop: Square, restart: RotateCw, pause: Pause, unpause: Play, kill: Zap, remove: Trash2 }
const VERBS = [...ACTIONS, 'logs', 'details'] as const

/**
 * ⌘K palette: jump to pages and containers, or type commands such as
 * "restart pihole", "logs npm" or "stop server-2/mosquitto".
 */
export function CommandPalette({ open, onClose, onLogs, onDetails }: { open: boolean; onClose: () => void; onLogs: (c: Container) => void; onDetails: (c: Container) => void }) {
  const me = useMe()
  const navigate = useNavigate()
  const { containers, hosts } = useFleet()
  const { run } = useContainerAction()
  const [q, setQ] = useState('')
  const [sel, setSel] = useState(0)
  const input = useRef<HTMLInputElement>(null)
  const operator = can(me.user, 'operator')

  useEffect(() => {
    if (open) {
      setQ('')
      setSel(0)
      setTimeout(() => input.current?.focus(), 10)
    }
  }, [open])

  const items = useMemo<Item[]>(() => {
    const close = (fn: () => void) => () => {
      onClose()
      fn()
    }
    const text = q.trim().toLowerCase()
    const m = text.match(/^(\w+)\s+(.*)$/)
    const verb = m && (VERBS as readonly string[]).includes(m[1]) ? (m[1] as Action | 'logs' | 'details') : null
    const term = verb ? m![2] : text
    const match = (c: Container) => !term || `${c.hostName}/${c.name}`.toLowerCase().includes(term) || c.image.toLowerCase().includes(term)

    if (verb) {
      if (verb !== 'logs' && verb !== 'details' && !operator) return []
      return containers
        .filter(match)
        .slice(0, 8)
        .map((c) => ({
          id: `${verb}-${c.hostId}-${c.id}`,
          icon: verb === 'logs' ? ScrollText : verb === 'details' ? Info : actionIcons[verb],
          label: `${verb[0].toUpperCase()}${verb.slice(1)} ${c.name}`,
          hint: c.hostName,
          tone: verb === 'kill' || verb === 'remove' ? 'danger' : undefined,
          dot: c,
          run: close(() => (verb === 'logs' ? onLogs(c) : verb === 'details' ? onDetails(c) : run(c, verb))),
        }))
    }

    const pages: Item[] = [
      { id: 'p-overview', icon: LayoutDashboard, label: 'Overview', run: close(() => navigate({ to: '/' })) },
      { id: 'p-containers', icon: Boxes, label: 'Containers', run: close(() => navigate({ to: '/containers' })) },
      { id: 'p-resources', icon: Layers, label: 'Resources: images, volumes, networks', run: close(() => navigate({ to: '/resources' })) },
      { id: 'p-hosts', icon: Server, label: 'Hosts', run: close(() => navigate({ to: '/hosts' })) },
      { id: 'p-account', icon: Fingerprint, label: 'Account & passkeys', run: close(() => navigate({ to: '/account' })) },
      ...(can(me.user, 'admin') ? [{ id: 'p-audit', icon: ShieldCheck, label: 'Audit log', run: close(() => navigate({ to: '/audit' })) }] : []),
    ].filter((p) => !text || p.label.toLowerCase().includes(text))
    const cs: Item[] = containers
      .filter(match)
      .slice(0, text ? 8 : 5)
      .map((c) => ({ id: `c-${c.hostId}-${c.id}`, icon: Info, label: c.name, hint: `${c.hostName} · details`, dot: c, run: close(() => onDetails(c)) }))
    const hs: Item[] = hosts
      .filter((h) => text && h.name.toLowerCase().includes(text))
      .map((h) => ({ id: `h-${h.id}`, icon: Server, label: h.name, hint: h.online ? 'online' : 'offline', run: close(() => navigate({ to: '/hosts' })) }))
    return [...pages, ...cs, ...hs]
  }, [q, containers, hosts, operator, me.user, navigate, onClose, onLogs, onDetails, run])

  useEffect(() => setSel(0), [q])

  if (!open) return null

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') onClose()
    else if (e.key === 'ArrowDown') {
      e.preventDefault()
      setSel((s) => Math.min(s + 1, items.length - 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setSel((s) => Math.max(s - 1, 0))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      items[sel]?.run()
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center p-4 pt-[12vh]" onKeyDown={onKey}>
      <div className="absolute inset-0 bg-black/60 backdrop-blur-sm" onClick={onClose} />
      <div role="dialog" aria-modal="true" aria-label="Command palette" className="relative w-full max-w-2xl overflow-hidden rounded-3xl bg-[#140f33]/95 shadow-2xl shadow-black/60 ring-1 ring-white/15 backdrop-blur-xl animate-[pop_140ms_ease-out]">
        <div className="flex items-center gap-3 border-b border-white/10 px-5 py-4">
          <Search className="size-5 text-indigo-100/50" />
          <input
            ref={input}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Search, or type a command: restart pihole, logs npm, stop mosquitto"
            className="flex-1 bg-transparent text-base text-white outline-none placeholder:text-indigo-100/40"
            aria-label="Command"
          />
          <Kbd>Esc</Kbd>
        </div>
        <ul className="max-h-[50vh] overflow-y-auto p-2" role="listbox">
          {items.length === 0 && <li className="px-4 py-8 text-center text-sm text-indigo-100/50">{operator ? 'Nothing matches.' : 'Nothing matches, or your role cannot run that.'}</li>}
          {items.map((it, i) => (
            <li key={it.id} role="option" aria-selected={i === sel}>
              <button
                onMouseEnter={() => setSel(i)}
                onClick={it.run}
                className={cx('flex w-full items-center gap-3 rounded-2xl px-4 py-2.5 text-left text-sm transition', i === sel ? 'bg-white/10 text-white' : 'text-indigo-100/80')}
              >
                <it.icon className={cx('size-4', it.tone === 'danger' ? 'text-rose-300' : 'text-sky-300')} />
                {it.dot && <StatusDot tone={toneOf(it.dot.state, it.dot.status)} />}
                <span className="font-medium">{it.label}</span>
                {it.hint && <span className="text-xs text-indigo-100/50">{it.hint}</span>}
                {i === sel && <CornerDownLeft className="ml-auto size-4 text-indigo-100/40" />}
              </button>
            </li>
          ))}
        </ul>
        <div className="flex flex-wrap gap-x-4 gap-y-1 border-t border-white/10 px-5 py-3 text-xs text-indigo-100/50">
          <span>
            <span className="mono text-sky-300">restart</span> · <span className="mono text-sky-300">stop</span> · <span className="mono text-sky-300">start</span> ·{' '}
            <span className="mono text-sky-300">logs</span> · <span className="mono text-sky-300">details</span> · <span className="mono text-sky-300">pause</span> · <span className="mono text-sky-300">kill</span> ·{' '}
            <span className="mono text-sky-300">remove</span> + a container name
          </span>
          <span className="ml-auto">↑↓ to move · ↵ to run</span>
        </div>
      </div>
    </div>
  )
}
