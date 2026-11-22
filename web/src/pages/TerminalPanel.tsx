import { useEffect, useRef, useState } from 'react'
import { RotateCcw, SquareTerminal, X } from 'lucide-react'
import { api, errorText, type Container } from '../api'
import { IconButton, StatusDot, cx } from '../ui'
import '@xterm/xterm/css/xterm.css'

const SHELLS = [
  ['auto', 'Shell (bash or sh)'],
  ['sh', 'sh'],
  ['bash', 'bash'],
] as const

const b64ToBytes = (b64: string) => Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))
const textToB64 = (s: string) => {
  let bin = ''
  for (const b of new TextEncoder().encode(s)) bin += String.fromCharCode(b)
  return btoa(bin)
}

// Forest palette for the terminal (same tokens as the rest of the UI).
const theme = {
  background: '#0b1310',
  foreground: '#dbe7e0',
  cursor: '#c4f04a',
  cursorAccent: '#0b1310',
  selectionBackground: 'rgba(196, 240, 74, 0.28)',
  black: '#15201b',
  brightBlack: '#5f756b',
  red: '#fb7185',
  brightRed: '#fda4af',
  green: '#34d399',
  brightGreen: '#c4f04a',
  yellow: '#fbbf24',
  brightYellow: '#fde68a',
  blue: '#7dd3fc',
  brightBlue: '#bae6fd',
  magenta: '#f0abfc',
  brightMagenta: '#f5d0fe',
  cyan: '#67e8f9',
  brightCyan: '#a5f3fc',
  white: '#dbe7e0',
  brightWhite: '#ffffff',
}

/** A terminal into a container, docked at the bottom like the logs drawer. */
export function TerminalPanel({ container, onClose }: { container: Container; onClose: () => void }) {
  const box = useRef<HTMLDivElement>(null)
  const [shell, setShell] = useState<string>('auto')
  const [custom, setCustom] = useState('')
  const [attempt, setAttempt] = useState(0) // bump to open a fresh session
  const [state, setState] = useState<'connecting' | 'open' | 'ended' | 'error'>('connecting')

  useEffect(() => {
    let disposed = false
    let es: EventSource | null = null
    let sid = ''
    let cleanup = () => {}
    setState('connecting')
    ;(async () => {
      const [{ Terminal }, { FitAddon }, { WebglAddon }] = await Promise.all([import('@xterm/xterm'), import('@xterm/addon-fit'), import('@xterm/addon-webgl')])
      if (disposed || !box.current) return
      const term = new Terminal({ fontFamily: '"JetBrains Mono Variable", ui-monospace, monospace', fontSize: 13, cursorBlink: true, theme, scrollback: 5000 })
      const fit = new FitAddon()
      term.loadAddon(fit)
      term.open(box.current)
      // Draw with WebGL: xterm's default DOM renderer injects <style> blocks,
      // which TainerHub's Content-Security-Policy (style-src 'self') blocks.
      try {
        const gl = new WebglAddon()
        gl.onContextLoss(() => gl.dispose())
        term.loadAddon(gl)
      } catch {
        /* no WebGL: the DOM renderer still works, just with plainer styling */
      }
      fit.fit()
      cleanup = () => term.dispose()

      try {
        const res = await api<{ session: string }>(`/api/hosts/${container.hostId}/containers/${encodeURIComponent(container.id)}/exec`, {
          method: 'POST',
          body: { shell: shell === 'custom' ? custom : shell, cols: term.cols, rows: term.rows },
        })
        sid = res.session
      } catch (e) {
        term.write(`\x1b[31m${errorText(e)}\x1b[0m\r\n`)
        setState('error')
        return
      }
      if (disposed) {
        void api(`/api/exec/${sid}`, { method: 'DELETE' }).catch(() => {})
        return
      }

      // Keystrokes go out in order, batched every few milliseconds.
      let pending = ''
      let timer: ReturnType<typeof setTimeout> | null = null
      let chain = Promise.resolve()
      const flush = () => {
        timer = null
        const data = pending
        pending = ''
        chain = chain.then(() => api(`/api/exec/${sid}/input`, { method: 'POST', body: { data: textToB64(data) } }).then(() => undefined)).catch(() => {})
      }
      term.onData((d) => {
        pending += d
        if (!timer) timer = setTimeout(flush, 8)
      })

      let resizeTimer: ReturnType<typeof setTimeout> | null = null
      const ro = new ResizeObserver(() => {
        fit.fit()
        if (resizeTimer) clearTimeout(resizeTimer)
        resizeTimer = setTimeout(() => {
          void api(`/api/exec/${sid}/resize`, { method: 'POST', body: { cols: term.cols, rows: term.rows } }).catch(() => {})
        }, 150)
      })
      ro.observe(box.current!)

      es = new EventSource(`/api/exec/${sid}`)
      es.addEventListener('ready', () => {
        setState('open')
        term.focus()
      })
      es.addEventListener('out', (e) => term.write(b64ToBytes((e as MessageEvent).data)))
      es.addEventListener('end', (e) => {
        const d = JSON.parse((e as MessageEvent).data || '{}') as { error?: string }
        term.write(`\r\n\x1b[2m[${d.error ? d.error : 'session ended'}]\x1b[0m\r\n`)
        setState('ended')
        es?.close()
      })
      es.onerror = () => {
        if (es?.readyState === EventSource.CLOSED) setState((s) => (s === 'ended' ? s : 'error'))
      }
      const prev = cleanup
      cleanup = () => {
        ro.disconnect()
        prev()
      }
    })()
    return () => {
      disposed = true
      es?.close()
      if (sid) void api(`/api/exec/${sid}`, { method: 'DELETE' }).catch(() => {})
      cleanup()
    }
  }, [container.hostId, container.id, attempt]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="fixed inset-x-0 bottom-0 z-40 mx-auto flex h-[52vh] max-w-[1500px] flex-col rounded-t-md border-t-2 border-lime-400 bg-panel shadow-[0_-20px_60px_-10px_rgba(0,0,0,0.8)] ring-1 ring-white/15 animate-[drawer_200ms_ease-out]">
      <div className="flex flex-wrap items-center gap-3 border-b border-white/[0.06] px-5 py-2.5">
        <SquareTerminal className="size-4 text-lime-300" />
        <div className="min-w-0">
          <div className="truncate font-display text-base font-bold text-white">{container.name}</div>
          <div className="flex items-center gap-1.5 text-xs text-ink-100/50">
            <StatusDot tone={state === 'open' ? 'good' : state === 'error' ? 'bad' : 'idle'} />
            {container.hostName} · {{ connecting: 'connecting', open: 'connected', ended: 'ended', error: 'failed' }[state]}
          </div>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <select
            aria-label="Shell"
            value={shell}
            onChange={(e) => setShell(e.target.value)}
            className="rounded-[2px] bg-ink-950/70 px-2 py-1 text-xs text-white ring-1 ring-inset ring-white/12 outline-none"
          >
            {SHELLS.map(([v, l]) => (
              <option key={v} value={v} className="bg-panel">
                {l}
              </option>
            ))}
            <option value="custom" className="bg-panel">
              Other command…
            </option>
          </select>
          {shell === 'custom' && (
            <input
              aria-label="Command"
              value={custom}
              onChange={(e) => setCustom(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && setAttempt((a) => a + 1)}
              placeholder="e.g. python3"
              className="mono w-36 rounded-[2px] bg-ink-950/70 px-2 py-1 text-xs text-white ring-1 ring-inset ring-white/12 outline-none"
            />
          )}
          <IconButton icon={RotateCcw} label="Open a new session with this shell" onClick={() => setAttempt((a) => a + 1)} />
          <IconButton icon={X} label="Close terminal" onClick={onClose} />
        </div>
      </div>
      <div ref={box} className={cx('min-h-0 flex-1 overflow-hidden bg-[#0b1310] px-3 py-2', state === 'connecting' && 'opacity-80')} />
    </div>
  )
}
