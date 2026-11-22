import { useEffect, useMemo, useRef, useState } from 'react'
import { ArrowDownToLine, Download, Eraser, Search, WrapText, X } from 'lucide-react'
import type { Container } from '../api'
import { IconButton, StatusDot, cx, download } from '../ui'

const MAX_CHARS = 2_000_000 // keep the browser responsive on chatty containers

export function LogsPanel({ container, onClose }: { container: Container; onClose: () => void }) {
  const [text, setText] = useState('')
  const [status, setStatus] = useState<'connecting' | 'live' | 'ended' | 'error'>('connecting')
  const [follow, setFollow] = useState(true)
  const [wrap, setWrap] = useState(true)
  const [query, setQuery] = useState('')
  const pre = useRef<HTMLPreElement>(null)

  useEffect(() => {
    setText('')
    setStatus('connecting')
    const es = new EventSource(`/api/hosts/${container.hostId}/containers/${encodeURIComponent(container.id)}/logs?tail=500`)
    es.onopen = () => setStatus('live')
    es.addEventListener('log', (e) => {
      const chunk = JSON.parse((e as MessageEvent).data) as string
      setText((t) => {
        const next = t + chunk
        return next.length > MAX_CHARS ? next.slice(next.length - MAX_CHARS) : next
      })
    })
    es.addEventListener('error', (e) => {
      const data = (e as MessageEvent).data
      if (data) setText((t) => t + `\n[tainerhub] ${JSON.parse(data)}\n`)
    })
    es.addEventListener('end', () => {
      setStatus('ended')
      es.close()
    })
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) setStatus((s) => (s === 'ended' ? s : 'error'))
    }
    return () => es.close()
  }, [container.hostId, container.id])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && document.activeElement?.tagName !== 'INPUT' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const lines = useMemo(() => {
    const all = text.split('\n')
    if (!query) return all
    const q = query.toLowerCase()
    return all.filter((l) => l.toLowerCase().includes(q))
  }, [text, query])

  useEffect(() => {
    if (follow && pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [lines, follow])

  return (
    <div className="fixed inset-x-0 bottom-0 z-40 mx-auto flex h-[46vh] max-w-[1500px] flex-col rounded-t-md border-t-2 border-lime-400 bg-panel shadow-[0_-20px_60px_-10px_rgba(0,0,0,0.8)] ring-1 ring-white/15 animate-[drawer_200ms_ease-out]">
      <div className="flex flex-wrap items-center gap-3 border-b border-white/[0.06] px-5 py-3">
        <StatusDot tone={status === 'live' ? 'good' : status === 'error' ? 'bad' : 'idle'} pulse={status === 'live'} />
        <div className="min-w-0">
          <div className="truncate font-display text-base font-bold text-white">{container.name}</div>
          <div className="text-xs text-ink-100/50">
            {container.hostName} · {status === 'live' ? 'streaming' : status}
          </div>
        </div>
        <div className="relative ml-auto">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-ink-100/50" />
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Filter lines"
            className="w-48 rounded-[3px] bg-white/[0.04] py-1.5 pl-8 pr-2 text-xs ring-1 ring-inset ring-white/10 outline-none focus:ring-lime-500/60"
          />
        </div>
        <div className="flex items-center gap-0.5">
          <IconButton icon={ArrowDownToLine} label={follow ? 'Auto-scroll on' : 'Auto-scroll off'} className={cx(follow && 'bg-lime-500/15 text-lime-300')} onClick={() => setFollow((f) => !f)} />
          <IconButton icon={WrapText} label={wrap ? 'Wrap on' : 'Wrap off'} className={cx(wrap && 'bg-lime-500/15 text-lime-300')} onClick={() => setWrap((w) => !w)} />
          <IconButton icon={Download} label="Download logs" onClick={() => download(`${container.hostName}-${container.name}.log`, text)} />
          <IconButton icon={Eraser} label="Clear" onClick={() => setText('')} />
          <IconButton icon={X} label="Close (Esc)" onClick={onClose} />
        </div>
      </div>
      <pre
        ref={pre}
        onScroll={(e) => {
          const el = e.currentTarget
          const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24
          if (!atBottom && follow) setFollow(false)
        }}
        className={cx('mono flex-1 overflow-auto px-5 py-3 text-[12px] leading-relaxed text-ink-100/80', wrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre')}
      >
        {text ? lines.join('\n') : status === 'connecting' ? 'Connecting…' : 'No output.'}
      </pre>
    </div>
  )
}
