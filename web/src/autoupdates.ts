// Automatic updates: types for /api/auto-updates and small helpers (kept
// free of React so `npm test` can check them).

export type AutoResult = 'ok' | 'failed' | 'skipped'

export interface AutoUpdateTarget {
  id: string
  kind: 'stack' | 'container'
  stackId?: string
  stackName?: string
  containerName?: string
  hostId: string
  hostName: string
  enabledBy: string
  createdAt: string
  lastRunAt: string | null
  lastResult: AutoResult | ''
  lastError: string
}

export interface AutoSettings {
  enabled: boolean
  days: string[]
  time: string
  timeZone: string
  maxMinutes: number
}

export interface AutoWindow {
  settings: AutoSettings
  zone: string
  next: string | null
  nextText?: string
}

export interface AutoOutcome {
  targetId: string
  kind: 'stack' | 'container'
  name: string
  stackId?: string
  hostId: string
  host: string
  result: AutoResult
  reason?: string
  images?: string[]
  oldImage?: string
  newImage?: string
  jobId?: string
  at: string
}

export interface AutoRun {
  id: string
  trigger: 'window' | 'manual'
  startedBy?: string
  startedAt: string
  deadline: string
  finishedAt: string | null
  running: boolean
  error?: string
  outcomes: AutoOutcome[]
}

export interface AutoUpdates {
  targets: AutoUpdateTarget[]
  window: AutoWindow
  run: AutoRun | null
}

export const WEEK = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'] as const

const dayNames: Record<string, string> = { mon: 'Mon', tue: 'Tue', wed: 'Wed', thu: 'Thu', fri: 'Fri', sat: 'Sat', sun: 'Sun' }

/** "every day", "weekdays", "weekends" or "Mon, Wed, Fri". */
export function daysText(days: string[]): string {
  const set = new Set(days)
  if (WEEK.every((d) => set.has(d))) return 'every day'
  const weekdays = WEEK.slice(0, 5)
  if (set.size === 5 && weekdays.every((d) => set.has(d))) return 'weekdays'
  if (set.size === 2 && set.has('sat') && set.has('sun')) return 'weekends'
  return WEEK.filter((d) => set.has(d))
    .map((d) => dayNames[d])
    .join(', ')
}

/** "2 h", "90 min". */
export function durationText(minutes: number): string {
  if (minutes % 60 === 0) return `${minutes / 60} h`
  return `${minutes} min`
}

/** The window in one line: "every day at 03:00 (Europe/Berlin), up to 2 h". */
export function windowText(w: AutoWindow): string {
  return `${daysText(w.settings.days)} at ${w.settings.time} (${w.zone}), up to ${durationText(w.settings.maxMinutes)}`
}

export const resultTone = (r: AutoResult | ''): 'good' | 'bad' | 'idle' => (r === 'ok' ? 'good' : r === 'failed' ? 'bad' : 'idle')

/** "updated", "failed", "skipped: host offline", "not run yet". */
export function resultText(result: AutoResult | '', reason?: string): string {
  switch (result) {
    case 'ok':
      return 'updated'
    case 'failed':
      return 'failed'
    case 'skipped':
      return reason ? `skipped: ${reason}` : 'skipped'
    default:
      return 'not run yet'
  }
}

/** The target for a stack or a standalone container, if it's opted in. */
export function findTarget(targets: AutoUpdateTarget[] | undefined, q: { stackId?: string; hostId?: string; name?: string }) {
  return targets?.find((t) => (q.stackId ? t.kind === 'stack' && t.stackId === q.stackId : t.kind === 'container' && t.hostId === q.hostId && t.containerName === q.name))
}

/** Why a container can't be opted in on its own (null if it can), mirroring the hub. */
export function containerAutoProblem(c: { name: string; project?: string; image: string }): string | null {
  if (c.name.startsWith('tainerhub')) return "TainerHub's own containers are updated from Hosts → Updates."
  if (c.project) return `It belongs to the compose project ${c.project}, so it updates with its stack: turn automatic updates on for the stack instead.`
  if (c.image.includes('@') || c.image.startsWith('sha256:')) return 'It uses a pinned image (by digest or ID), so there is never a newer one.'
  return null
}
