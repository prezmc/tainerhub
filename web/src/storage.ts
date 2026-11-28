// Disk use per container, as the agent measured it (every half hour).
// Older agents don't send it: those containers show no figure and sort last.

export interface ContainerSize {
  /** Bytes the container wrote itself (its writable layer), not the image. */
  rw: number
  /** Bytes in the Docker volumes it mounts (local ones; shared volumes count for each container). */
  volumes?: number
  /** Unix seconds of the measurement. */
  at: number
}

/** Total bytes: what it wrote plus its volumes, or undefined when not measured. */
export function storageOf(c: { size?: ContainerSize }): number | undefined {
  return c.size ? c.size.rw + (c.size.volumes ?? 0) : undefined
}

/** Biggest first; containers without a figure last, then by name. */
export function byStorage(a: { name: string; size?: ContainerSize }, b: { name: string; size?: ContainerSize }): number {
  return (storageOf(b) ?? -1) - (storageOf(a) ?? -1) || a.name.localeCompare(b.name)
}

/** Disk sizes in decimal units, like `docker system df`: 0 B, 12 KB, 40 MB, 1.2 GB. */
export function bytes(n?: number): string {
  if (n === undefined) return '–'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (n >= 1000 && i < units.length - 1) {
    n /= 1000
    i++
  }
  return `${i > 0 && n < 10 ? n.toFixed(1) : Math.round(n)} ${units[i]}`
}

/** The line under the figure, e.g. "40 MB written by the container, 1.2 GB in volumes". */
export function storageDetail(s: ContainerSize): string {
  const rw = `${bytes(s.rw)} written by the container`
  return s.volumes ? `${rw}, ${bytes(s.volumes)} in volumes` : rw
}
