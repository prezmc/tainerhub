import {
  startAuthentication,
  startRegistration,
  type PublicKeyCredentialCreationOptionsJSON,
  type PublicKeyCredentialRequestOptionsJSON,
} from '@simplewebauthn/browser'

export class ApiError extends Error {
  status: number
  data: Record<string, unknown>
  constructor(message: string, status: number, data: Record<string, unknown> = {}) {
    super(message)
    this.status = status
    this.data = data
  }
}

/** Calls the hub API. Every request carries the X-TainerHub header the hub requires (CSRF guard). */
export async function api<T = unknown>(path: string, opts: { method?: string; body?: unknown } = {}): Promise<T> {
  const res = await fetch(path, {
    method: opts.method ?? 'GET',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-TainerHub': '1' },
    body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
  })
  if (res.status === 204) return undefined as T
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new ApiError((data as { error?: string }).error ?? res.statusText, res.status, data as Record<string, unknown>)
  return data as T
}

export type Role = 'owner' | 'admin' | 'operator' | 'viewer'

export interface User {
  id: string
  username: string
  displayName: string
  role: Role
  createdAt: string
}

export interface Passkey {
  id: string
  name: string
  createdAt: string
  lastUsedAt: string | null
}

export interface Me {
  user: User
  passkeys: Passkey[]
  recoveryCodesLeft: number
  hasPassword: boolean
  /** False until the hub has an https:// public URL (passkeys need HTTPS). */
  passkeysEnabled: boolean
  /** The https:// address passkeys work on ("" while HTTPS is off). */
  publicURL: string
}

export interface HubState {
  setupRequired: boolean
  version: string
  passkeys: boolean
  publicURL: string
}

/** Passkeys work only when the hub has them on and this page is open at the hub's secure address (they're tied to that name). */
export const passkeysUsable = (hubEnabled: boolean | undefined, publicURL: string | undefined) =>
  !!hubEnabled && window.isSecureContext && !!publicURL && window.location.origin === publicURL

export interface Hello {
  hostname: string
  docker_version: string
  os: string
  arch: string
  ncpu: number
  mem_total: number
  agent_version: string
  /** Missing on agents older than stack support. */
  stacks?: { dir: string; compose_version?: string; error?: string }
}

export interface Host {
  id: string
  name: string
  createdAt: string
  enrolledAt: string | null
  lastSeenAt: string | null
  online: boolean
  /** IP the host's agent connects from. */
  address?: string
  /** The host also runs the hub. */
  hubHost?: boolean
  hello?: Hello
  containers: number
  running: number
  unhealthy: number
}

export interface Container {
  id: string
  name: string
  image: string
  state: string
  status: string
  created: number
  project?: string
  ports?: string[]
  hostId: string
  hostName: string
  /** A newer image is available for this container's tag. */
  update?: boolean
  cpu?: number // % of one core
  mem?: number // bytes
  mem_limit?: number
}

export interface Activity {
  at: string
  kind: 'container' | 'host' | 'user'
  action: string
  hostId?: string
  host?: string
  container?: string
  actor?: string
}

export interface JoinTokenResponse {
  host: Host
  joinToken: string
  expiresAt: string
  agentAddr: string
  compose: string
}

export interface AuditEntry {
  id: number
  at: string
  actor: string
  action: string
  target: string
  detail?: Record<string, string>
  ip: string
}

export const roleRank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3, owner: 4 }
export const can = (u: User | undefined, role: Role) => !!u && roleRank[u.role] >= roleRank[role]

// ---- passkey ceremonies ----

export async function passkeySetup(input: { token: string; username: string; displayName: string; passkeyName: string }) {
  const opts = await api<{ publicKey: PublicKeyCredentialCreationOptionsJSON }>('/api/setup/begin', { method: 'POST', body: input })
  const cred = await startRegistration({ optionsJSON: opts.publicKey })
  return api<{ user: User; recoveryCodes: string[] }>('/api/setup/finish', { method: 'POST', body: cred })
}

export async function passkeyLogin() {
  const opts = await api<{ publicKey: PublicKeyCredentialRequestOptionsJSON }>('/api/login/begin', { method: 'POST', body: {} })
  const cred = await startAuthentication({ optionsJSON: opts.publicKey })
  return api<{ user: User }>('/api/login/finish', { method: 'POST', body: cred })
}

export async function passkeyAdd(name: string) {
  const opts = await api<{ publicKey: PublicKeyCredentialCreationOptionsJSON }>('/api/me/passkeys/begin', { method: 'POST', body: { name } })
  const cred = await startRegistration({ optionsJSON: opts.publicKey })
  await api('/api/me/passkeys/finish', { method: 'POST', body: cred })
}

/** True when the hub refused a change because it needs a recent sign-in. */
export const needsReauth = (e: unknown) => e instanceof ApiError && e.status === 403 && e.data.reauth === true

/**
 * A same-origin path to return to after signing in, or null. Only plain
 * paths like "/account" pass: not "//host", backslashes or control
 * characters, which browsers can read as another site.
 */
export function safeNext(raw: string | null | undefined): string | null {
  if (!raw || !/^\/(?![/\\])[^\\\u0000-\u001f\u007f]*$/.test(raw)) return null
  return raw
}

/** Turns WebAuthn/browser errors into something readable. */
export function errorText(e: unknown): string {
  if (e instanceof ApiError) return e.message
  if (e instanceof Error) {
    if (e.name === 'NotAllowedError') return 'Passkey prompt was cancelled or timed out.'
    if (e.name === 'InvalidStateError') return 'That passkey is already registered.'
    if (e.name === 'SecurityError') return 'Passkeys need HTTPS on the exact TainerHub address. Check TAINERHUB_PUBLIC_URL matches the URL in your browser.'
    return e.message
  }
  return String(e)
}

export function timeAgo(iso: string | null | undefined): string {
  if (!iso) return 'never'
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)} min ago`
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`
  return `${Math.floor(s / 86400)} d ago`
}

export function mem(n?: number): string {
  if (!n) return '–'
  if (n >= 1024 ** 3) return `${(n / 1024 ** 3).toFixed(1)} GB`
  return `${Math.round(n / 1024 ** 2)} MB`
}

export const isUnhealthy = (c: Container) =>
  c.status.includes('(unhealthy)') || c.state === 'restarting' || c.state === 'dead' || (c.state === 'exited' && /Exited \((?!0\))\d+\)/.test(c.status))

export function bytes(n: number): string {
  if (!n) return '–'
  const gb = n / 1024 ** 3
  return gb >= 1 ? `${gb.toFixed(gb >= 10 ? 0 : 1)} GB` : `${Math.round(n / 1024 ** 2)} MB`
}

// ---- resources & details ----

export interface ContainerDetail {
  id: string
  name: string
  image: string
  imageId: string
  created: string
  state: {
    status: string
    startedAt: string
    finishedAt: string
    exitCode: number
    oomKilled: boolean
    error?: string
    health?: { status: string; failingStreak: number; log: { start: string; end: string; exitCode: number; output: string }[] }
  }
  cmd?: string[]
  entrypoint?: string[]
  workingDir?: string
  user?: string
  env?: string[]
  labels?: Record<string, string>
  restartPolicy: string
  maxRetries?: number
  privileged: boolean
  networkMode: string
  memoryLimit?: number
  nanoCpus?: number
  mounts: { type: string; name?: string; source: string; destination: string; rw: boolean }[]
  networks: { name: string; ip?: string; gateway?: string }[]
  ports: { container: string; host?: string }[]
}

export interface DetailResponse {
  detail: ContainerDetail
  envRevealed: boolean
  hostId: string
  hostName: string
}

export interface ImageRes {
  id: string
  tags: string[]
  size: number
  created: number
  usedBy: string[]
}
export interface VolumeRes {
  name: string
  driver: string
  mountpoint: string
  created: string
  project?: string
  usedBy: string[]
}
export interface NetworkRes {
  id: string
  name: string
  driver: string
  scope: string
  internal: boolean
  builtin: boolean
  project?: string
  usedBy: string[]
}
export interface PruneResult {
  deleted: number
  reclaimed: number
}

export function size(n: number): string {
  if (!n) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), u.length - 1)
  return `${(n / 1024 ** i).toFixed(i >= 2 ? 1 : 0)} ${u[i]}`
}

// ---- compose stacks ----

export type StackStatus = 'running' | 'partial' | 'stopped' | 'down' | 'not deployed' | 'unknown'
export type StackAction = 'deploy' | 'pull' | 'start' | 'stop' | 'restart' | 'down'

export interface StackSummary {
  /** Missing for compose projects TainerHub doesn't manage. */
  id?: string
  name: string
  hostId: string
  hostName: string
  managed: boolean
  tainerhub?: boolean
  online: boolean
  status: StackStatus
  containers: number
  running: number
  unhealthy: number
  updatedAt?: string
  updatedBy?: string
  deployedAt?: string
  /** Saved changes that haven't been deployed yet. */
  pending: boolean
  job?: { id: string; action: string }
}

export interface Stack {
  id: string
  hostId: string
  name: string
  compose: string
  env: string
  createdAt: string
  updatedAt: string
  updatedBy: string
  deployedAt: string | null
}

export interface StackDetail {
  stack: Stack
  summary: StackSummary
  /** Only admins can read compose files and .env (they often hold passwords). */
  filesVisible: boolean
}

export interface StackSaveResult {
  stack: Stack
  jobId?: string
  deployError?: string
}

/**
 * The address to reach a host's published ports at. The hub's own server's
 * agent connects over Docker's internal network, so for it use the address
 * this page was opened on instead.
 */
export function hostAddress(h: Host | undefined): string | undefined {
  if (!h) return undefined
  if (h.hubHost) return window.location.hostname
  return h.address
}

/** "8080->80/tcp" or "0.0.0.0:8080" -> "8080"; null if not published. */
export function publishedPort(p: string): string | null {
  const m = p.includes('->') ? p.split('->')[0].match(/(\d+)$/) : p.match(/:(\d+)$/)
  return m ? m[1] : null
}

/** Why a host can't run stacks, or null if it can. */
export function stackProblem(h: Host | undefined): string | null {
  if (!h) return 'Unknown host.'
  if (!h.online || !h.hello) return `${h.name} is offline.`
  if (!h.hello.stacks) return `The agent on ${h.name} is too old for stacks. Update it to the latest tainerhub-agent image.`
  if (h.hello.stacks.error) return h.hello.stacks.error
  return null
}
