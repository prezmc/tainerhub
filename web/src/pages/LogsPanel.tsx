import { useEffect, useMemo, useRef, useState } from 'react'
import { ArrowDownToLine, Download, Eraser, Search, WrapText, X } from 'lucide-react'
import type { Container } from '../api'
import { IconButton, StatusDot, cx, download, useLayer } from '../ui'

const MAX_LINES = 10_000 // keep the browser responsive on chatty containers

/**
 * Log lines as an array (complete lines plus the unfinished tail of the last
 * chunk). `dropped` counts lines trimmed from the front, so a line's
 * absolute number is dropped + index; `gen` changes when the log is cleared.
 */
type Buf = { lines: string[]; tail: string; dropped: number; gen: number }
const EMPTY: Buf = { lines: [], tail: '', dropped: 0, gen: 0 }

function append(b: Buf, chunk: string): Buf {
  const parts = (b.tail + chunk).split('\n')
  const tail = parts.pop() ?? ''
  if (parts.length === 0) return { ...b, tail }
  let lines = b.lines.concat(parts)
  let dropped = b.dropped
  if (lines.length > MAX_LINES) {
    dropped += lines.length - MAX_LINES
    lines = lines.slice(-MAX_LINES)
  }
  return { lines, tail, dropped, gen: b.gen }
}

type FilterCache = { q: string; gen: number; end: number; idx: number[] }

/** Lines matching q. Only lines that arrived since the last call are checked. */
function useFiltered(buf: Buf, q: string): string[] | null {
  const cache = useRef<FilterCache | null>(null)
  return useMemo(() => {
    if (!q) return null
    const end = buf.dropped + buf.lines.length
    const c = cache.current
    const reuse = c && c.q === q && c.gen === buf.gen && c.end <= end
    const from = reuse ? Math.max(c.end, buf.dropped) : buf.dropped
    const fresh: number[] = []
    for (let a = from; a < end; a++) if (buf.lines[a - buf.dropped].toLowerCase().includes(q)) fresh.push(a)
    let idx = reuse ? c.idx.concat(fresh) : fresh
    let first = 0
    while (first < idx.length && idx[first] < buf.dropped) first++
    if (first) idx = idx.slice(first)
    cache.current = { q, gen: buf.gen, end, idx }
    const out = idx.map((a) => buf.lines[a - buf.dropped])
    if (buf.tail && buf.tail.toLowerCase().includes(q)) out.push(buf.tail)
    return out
  }, [buf, q])
}

export function LogsPanel({ container, onClose }: { container: Container; onClose: () => void }) {
  const [buf, setBuf] = useState<Buf>(EMPTY)
  const [status, setStatus] = useState<'connecting' | 'live' | 'ended' | 'error'>('connecting')
  const [follow, setFollow] = useState(true)
  const [wrap, setWrap] = useState(true)
  const [query, setQuery] = useState('')
  const pre = useRef<HTMLPreElement>(null)
  const panel = useRef<HTMLDivElement>(null)

  useEffect(() => {
    setBuf((b) => ({ ...EMPTY, gen: b.gen + 1 }))
    setStatus('connecting')
    // Chunks are buffered and applied once per animation frame, so a chatty
    // container costs one render per frame, not one per chunk.
    let pending = ''
    let frame = 0
    const flush = () => {
      frame = 0
      const chunk = pending
      pending = ''
      if (chunk) setBuf((b) => append(b, chunk))
    }
    const push = (chunk: string) => {
      pending += chunk
      if (!frame) frame = requestAnimationFrame(flush)
    }
    const parse = (data: string): string | null => {
      try {
        return String(JSON.parse(data))
      } catch {
        return null
      }
    }
    const es = new EventSource(`/api/hosts/${container.hostId}/containers/${encodeURIComponent(container.id)}/logs?tail=500`)
    es.onopen = () => setStatus('live')
    es.addEventListener('log', (e) => {
      const chunk = parse((e as MessageEvent).data)
      if (chunk !== null) push(chunk)
    })
    es.addEventListener('error', (e) => {
      const data = (e as MessageEvent).data
      if (data) push(`\n[tainerhub] ${parse(data) ?? data}\n`)
    })
    es.addEventListener('end', () => {
      setStatus('ended')
      es.close()
    })
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) setStatus((s) => (s === 'ended' ? s : 'error'))
    }
    return () => {
      es.close()
      if (frame) cancelAnimationFrame(frame)
    }
  }, [container.hostId, container.id])

  // Escape in the filter field just leaves the field; otherwise it closes the drawer.
  useLayer(true, panel, () => {
    const el = document.activeElement as HTMLElement | null
    if (el?.tagName === 'INPUT' && panel.current?.contains(el)) el.blur()
    else onClose()
  })

  const filtered = useFiltered(buf, query.toLowerCase())
  const hasText = buf.lines.length > 0 || buf.tail !== ''
  const all = () => (buf.tail ? buf.lines.concat(buf.tail) : buf.lines).join('\n')
  const shown = useMemo(() => (filtered ? filtered.join('\n') : (buf.tail ? buf.lines.concat(buf.tail) : buf.lines).join('\n')), [filtered, buf])

  useEffect(() => {
    if (follow && pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [shown, follow])

  return (
    <div ref={panel} className="fixed inset-x-0 bottom-0 z-[45] mx-auto flex h-[46vh] max-w-[1500px] flex-col rounded-t-md border-t-2 border-lime-400 bg-panel shadow-[0_-20px_60px_-10px_rgba(0,0,0,0.8)] ring-1 ring-white/15 animate-[drawer_200ms_ease-out]">
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
          <IconButton icon={Download} label="Download logs" onClick={() => download(`${container.hostName}-${container.name}.log`, all())} />
          <IconButton icon={Eraser} label="Clear" onClick={() => setBuf((b) => ({ ...EMPTY, gen: b.gen + 1 }))} />
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
        {hasText ? shown : status === 'connecting' ? 'Connecting…' : 'No output.'}
      </pre>
    </div>
  )
}
