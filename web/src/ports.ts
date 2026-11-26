// Published ports: a container's mappings, and every host port in use on a
// host. Docker lists a port published on every address twice (0.0.0.0 and
// ::); these merge them. Dependency-free for tests.

import { nextFreePort } from './recommend.ts'

/** One published port, as the agent reports it (wire.PortBinding). */
export interface PortBinding {
  ip?: string
  hostPort: number
  port: number
  proto: string
}

/** Whether a host address means every address (0.0.0.0, :: or none). */
export const anyAddress = (ip?: string) => !ip || ip === '0.0.0.0' || ip === '::' || ip === '[::]'

/** Whether a host address only reaches the host itself. */
export const loopback = (ip?: string) => !!ip && (/^127\./.test(ip) || ip === '::1' || ip === '[::1]' || ip.toLowerCase() === 'localhost')

/** Where a published port listens, in words. */
export function addressText(addresses: string[]): string {
  if (addresses.length === 0) return 'all addresses'
  return addresses.join(', ') + (addresses.every(loopback) ? ' (this host only)' : '')
}

const byNumber = (a: number, b: number) => a - b

/**
 * "8080->80/tcp", "0.0.0.0:8080->80/tcp" or "[::1]:8080->80/tcp" as a
 * binding; null when the port isn't published ("80/tcp").
 */
export function parsePublished(s: string): PortBinding | null {
  const [pub, rest] = s.split('->')
  if (rest === undefined) return null
  const [port, proto = 'tcp'] = rest.split('/')
  const i = pub.lastIndexOf(':')
  const hostPort = Number(i < 0 ? pub : pub.slice(i + 1))
  const ip = i < 0 ? '' : pub.slice(0, i).replace(/^\[(.*)\]$/, '$1')
  if (!Number.isInteger(hostPort) || hostPort < 1 || !/^\d+$/.test(port)) return null
  return { ip, hostPort, port: Number(port), proto }
}

/** A container's published ports: with their addresses from current agents, from the compact list otherwise. */
export function bindingsOf(c: { ports?: string[]; published?: PortBinding[] }): PortBinding[] {
  if (c.published?.length) return c.published
  return (c.ports ?? []).map(parsePublished).filter((b): b is PortBinding => !!b)
}

/** One host port held by one container. addresses is empty when it listens on every address. */
export interface HostPort {
  hostPort: number
  proto: string
  addresses: string[]
  container: string
  port: number
}

export interface HostPorts {
  ports: HostPort[]
  /** Containers on the host's network: they can listen on any port without publishing it. */
  hostNetwork: string[]
}

type FleetContainer = { name: string; hostId: string; ports?: string[]; published?: PortBinding[]; hostNetwork?: boolean }

/** Every host port the containers on host hostId publish, by port, protocol and container. */
export function hostPorts(containers: FleetContainer[], hostId: string): HostPorts {
  const rows = new Map<string, HostPort & { any: boolean; ips: Set<string> }>()
  const hostNetwork: string[] = []
  for (const c of containers) {
    if (c.hostId !== hostId) continue
    if (c.hostNetwork) hostNetwork.push(c.name)
    for (const b of bindingsOf(c)) {
      const key = `${b.hostPort}/${b.proto}/${c.name}/${b.port}`
      let r = rows.get(key)
      if (!r) rows.set(key, (r = { hostPort: b.hostPort, proto: b.proto, addresses: [], container: c.name, port: b.port, any: false, ips: new Set() }))
      if (anyAddress(b.ip)) r.any = true
      else r.ips.add(b.ip!)
    }
  }
  const ports = [...rows.values()]
    .map(({ any, ips, ...r }) => ({ ...r, addresses: any ? [] : [...ips].sort() }))
    .sort((a, b) => byNumber(a.hostPort, b.hostPort) || a.proto.localeCompare(b.proto) || a.container.localeCompare(b.container) || byNumber(a.port, b.port))
  return { ports, hostNetwork: hostNetwork.sort() }
}

/** What a host port number is used for: null when it isn't a port number, else the rows using it and the next free port for each protocol in use. */
export function checkPort(ports: HostPort[], input: string): { port: number; used: HostPort[]; next: Record<string, string> } | null {
  const s = input.trim()
  if (!/^\d+$/.test(s)) return null
  const port = Number(s)
  if (port < 1 || port > 65535) return null
  const taken = new Set(ports.map((p) => `${p.hostPort}/${p.proto}`))
  const used = ports.filter((p) => p.hostPort === port)
  const next: Record<string, string> = {}
  for (const proto of new Set(used.map((p) => p.proto))) next[proto] = nextFreePort(String(port), proto, (k) => taken.has(k))
  return { port, used, next }
}

/** A port in a container's details (wire.DetailPort). host is "8080" or "127.0.0.1:8080"; ip is the raw address from current agents. */
export interface DetailPort {
  container: string
  host?: string
  ip?: string
}

/** One of a container's ports: published at hostPort on addresses (empty: every address), or not published (no hostPort). */
export interface PortMapping {
  container: string
  hostPort?: string
  addresses: string[]
}

/** A container's ports from its details, published ones first, each sorted by number, every-address duplicates merged. */
export function portMappings(ports: DetailPort[]): PortMapping[] {
  const out = new Map<string, PortMapping>()
  for (const p of ports) {
    let hostPort: string | undefined
    let ip = p.ip ?? ''
    if (p.host) {
      const i = p.host.lastIndexOf(':')
      hostPort = i < 0 ? p.host : p.host.slice(i + 1)
      if (!p.ip && i >= 0) ip = p.host.slice(0, i).replace(/^\[(.*)\]$/, '$1')
    }
    const key = `${p.container}|${hostPort ?? ''}`
    let m = out.get(key)
    if (!m) out.set(key, (m = { container: p.container, hostPort, addresses: [] }))
    if (anyAddress(ip)) (m as PortMapping & { any?: boolean }).any = true
    else if (!m.addresses.includes(ip)) m.addresses.push(ip)
  }
  const num = (s?: string) => (s ? parseInt(s, 10) || 0 : 0)
  return [...out.values()]
    .map((m) => {
      const { any, ...rest } = m as PortMapping & { any?: boolean }
      return { ...rest, addresses: any ? [] : rest.addresses.sort() }
    })
    .sort((a, b) => (a.hostPort ? 0 : 1) - (b.hostPort ? 0 : 1) || num(a.container) - num(b.container) || a.container.localeCompare(b.container) || num(a.hostPort) - num(b.hostPort))
}
