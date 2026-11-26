// Small recommendations for a new container: this computer's time zone, and
// a free host port when the one asked for is taken. Dependency-free for tests.

/** This computer's time zone (IANA name, like Europe/London), or "" if the browser won't say. */
export function localTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone ?? ''
  } catch {
    return ''
  }
}

/** Whether a TZ value is UTC under one of its names: the usual example value in READMEs. */
export const isUtcLike = (v: string) => /^(etc\/)?(utc|gmt|uct|zulu|universal)([+-]?0)?$/i.test(v.trim())

/**
 * The host ports already published on host hostId, as "8080/tcp" -> the
 * container using it. Containers of stack skip are left out (a redeploy
 * keeps its own ports).
 */
export function usedHostPorts(containers: { name: string; hostId: string; project?: string; ports?: string[] }[], hostId: string, skip = ''): Map<string, string> {
  const used = new Map<string, string>()
  for (const c of containers) {
    if (c.hostId !== hostId || (skip && c.project === skip)) continue
    for (const p of c.ports ?? []) {
      const [pub, rest] = p.split('->')
      if (rest === undefined) continue
      const proto = rest.split('/')[1] ?? 'tcp'
      const port = pub.split(':').pop() ?? pub // "0.0.0.0:8080" in some versions
      if (/^\d+$/.test(port)) used.set(`${port}/${proto}`, c.name)
    }
  }
  return used
}

/** The first host port after port that isn't taken, or "". */
export function nextFreePort(port: string, proto: string, taken: (key: string) => boolean): string {
  const n = Number(port)
  if (!Number.isInteger(n) || n < 1) return ''
  for (let p = n + 1; p <= 65535 && p <= n + 200; p++) if (!taken(`${p}/${proto}`)) return String(p)
  return ''
}

/**
 * Host ports in rows that clash with the host or with an earlier row, each
 * with the next free port. Rows without a host port are skipped.
 */
export function portConflicts(rows: { host: string; proto: string }[], used: Map<string, string>): { index: number; key: string; by: string; suggestion: string }[] {
  const mine = new Set<string>()
  const out: { index: number; key: string; by: string; suggestion: string }[] = []
  const taken = (k: string) => used.has(k) || mine.has(k) || rows.some((r) => `${r.host}/${r.proto}` === k)
  rows.forEach((r, index) => {
    if (!/^\d+$/.test(r.host)) return
    const key = `${r.host}/${r.proto}`
    const by = used.get(key) ?? (mine.has(key) ? 'another port in this form' : '')
    if (by) {
      const suggestion = nextFreePort(r.host, r.proto, (k) => taken(k) || out.some((o) => `${o.suggestion}/${r.proto}` === k))
      out.push({ index, key, by, suggestion })
    }
    mine.add(key)
  })
  return out
}
