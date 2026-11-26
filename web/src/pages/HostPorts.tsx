import { useMemo, useState } from 'react'
import { Plug } from 'lucide-react'
import { hostAddress, type Host } from '../api'
import { useFleet } from '../fleet'
import { addressText, checkPort, hostPorts, loopback } from '../ports'
import { Input, Modal, cx } from '../ui'

/** The host ports in use on host h, from the live fleet data. */
export function useHostPorts(h: Host) {
  const { containers } = useFleet()
  return useMemo(() => hostPorts(containers, h.id), [containers, h.id])
}

/** A button showing how many host ports h's containers publish; opens the list. */
export function HostPortsButton({ h, className }: { h: Host; className?: string }) {
  const [open, setOpen] = useState(false)
  const { ports } = useHostPorts(h)
  const n = new Set(ports.map((p) => `${p.hostPort}/${p.proto}`)).size
  return (
    <>
      <button onClick={() => setOpen(true)} className={cx('inline-flex items-center gap-1.5 hover:text-white hover:underline', className)} title={`Host ports in use on ${h.name}`}>
        <Plug className="size-3.5" />
        <span className="tabular-nums">{n}</span> {n === 1 ? 'port' : 'ports'} in use
      </button>
      {open && <HostPortsDialog h={h} onClose={() => setOpen(false)} />}
    </>
  )
}

/** Every host port h's containers publish, which container holds it, and a check for whether a port is free. */
export function HostPortsDialog({ h, onClose }: { h: Host; onClose: () => void }) {
  const { ports, hostNetwork } = useHostPorts(h)
  const [q, setQ] = useState('')
  const check = checkPort(ports, q)
  return (
    <Modal open onClose={onClose} title={`Ports on ${h.name}`} wide>
      <p className="mb-4 text-sm text-ink-100/60">Host ports published by running containers. A stopped container's ports are free until it starts again.</p>
      <Input label="Is a port free?" inputMode="numeric" placeholder="e.g. 8080" value={q} onChange={(e) => setQ(e.target.value)} maxLength={5} />
      {q.trim() && (
        <p className="mt-2 text-sm" role="status">
          {!check ? (
            <span className="text-ink-100/60">Enter a port from 1 to 65535.</span>
          ) : check.used.length === 0 ? (
            <span className="text-emerald-300">{check.port} is free{hostNetwork.length > 0 ? ', unless a container on the host network listens on it' : ''}.</span>
          ) : (
            <span className="text-amber-200">
              {check.port} is in use by {check.used.map((p) => `${p.container} (${p.proto})`).join(', ')}. Next free:{' '}
              {Object.entries(check.next)
                .map(([proto, n]) => `${n || 'none nearby'}/${proto}`)
                .join(', ')}
              .
            </span>
          )}
        </p>
      )}

      {ports.length === 0 ? (
        <p className="mt-5 text-sm text-ink-100/50">No container on {h.name} publishes a port.</p>
      ) : (
        <table className="mt-5 w-full table-fixed text-sm">
          <thead>
            <tr className="border-b border-white/[0.08] text-left text-xs text-ink-100/50">
              <th className="w-24 pb-1.5 font-medium">Host port</th>
              <th className="pb-1.5 font-medium">Container</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/[0.05]">
            {ports.map((p) => (
              <tr key={`${p.hostPort}/${p.proto}/${p.container}/${p.port}`} className="align-top">
                <td className="mono py-1.5 text-lime-200">
                  {p.proto === 'tcp' ? <PublishedLink hostPort={String(p.hostPort)} addresses={p.addresses} host={h} /> : p.hostPort}
                  <span className="text-ink-100/50">/{p.proto}</span>
                </td>
                <td className="min-w-0 py-1.5">
                  <div className="truncate text-ink-50" title={p.container}>
                    {p.container} <span className="mono ml-1 text-xs text-ink-100/50">→ {p.port}</span>
                  </div>
                  <div className="truncate text-xs text-ink-100/50">{addressText(p.addresses)}</div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {hostNetwork.length > 0 && (
        <p className="mt-4 text-xs text-ink-100/55">
          On the host's network, so they can listen on any port without publishing it: {hostNetwork.join(', ')}.
        </p>
      )}
    </Modal>
  )
}

/** A host port as a link to http://<address>:<port>, unless only the host itself can reach it. */
export function PublishedLink({ hostPort, addresses, host }: { hostPort: string; addresses: string[]; host?: Host }) {
  const ip = addresses.find((a) => !loopback(a))
  const addr = addresses.length === 0 ? hostAddress(host) : ip
  if (!addr) return <>{hostPort}</>
  const url = `http://${addr.includes(':') ? `[${addr}]` : addr}:${hostPort}`
  return (
    <a href={url} target="_blank" rel="noreferrer" className="hover:underline" title={`Open ${url}`}>
      {hostPort}
    </a>
  )
}
