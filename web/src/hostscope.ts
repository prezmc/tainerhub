// Host-scoped access: an admin can limit a user, or an invite, to some
// hosts. Their role then applies on those hosts only, and the hub hides
// every other host from them (see internal/api/scope.go).
import type { Role } from './api'

/** A user's or invite's hosts: every host, or only `hosts` (IDs). */
export interface Scope {
  scoped: boolean
  hosts: string[]
}

type Named = { id: string; name: string; hubHost?: boolean }
type Who = { role: Role; scoped?: boolean } | undefined

export const allHosts: Scope = { scoped: false, hosts: [] }

/** The scope of a user or invite from the API (`hosts` is left out for every host). */
export const scopeOf = (x: { scoped?: boolean; hosts?: string[] | null }): Scope => ({ scoped: !!x.scoped, hosts: x.scoped ? (x.hosts ?? []) : [] })

/**
 * An admin with every host. Hub-wide pages (users, settings, registries,
 * adding and removing hosts) are theirs; an admin limited to some hosts
 * manages those hosts only.
 */
export const hubAdmin = (u: Who) => !!u && (u.role === 'admin' || u.role === 'owner') && !u.scoped

/** "All hosts", or the hosts' names in alphabetical order. */
export function scopeText(scope: Scope, hosts: Named[]): string {
  if (!scope.scoped) return 'All hosts'
  const names = hosts.filter((h) => scope.hosts.includes(h.id)).map((h) => h.name)
  names.sort((a, b) => a.localeCompare(b))
  const gone = scope.hosts.filter((id) => !hosts.some((h) => h.id === id)).length
  if (gone) names.push(gone === 1 ? '1 removed host' : `${gone} removed hosts`)
  return names.length ? names.join(', ') : 'No hosts'
}

/** Ticks or unticks a host. */
export const toggleHost = (ids: string[], id: string) => (ids.includes(id) ? ids.filter((x) => x !== id) : [...ids, id])

/** What's wrong with a choice, as the hub would say it; null when it will do. */
export function scopeProblem(role: Role, scope: Scope, hosts: Named[]): string | null {
  if (!scope.scoped) return null
  const picked = hosts.filter((h) => scope.hosts.includes(h.id))
  if (picked.length === 0) return 'Pick at least one host, or give access to all hosts.'
  if (role === 'admin' && picked.some((h) => h.hubHost))
    return "An admin limited to some hosts can't include the hub's own server: root there is root on the hub. Make them an operator, or give them every host."
  return null
}

/** The `hosts` field the API takes: null for every host. Removed hosts are dropped. */
export const scopeBody = (scope: Scope, hosts: Named[]) => (scope.scoped ? scope.hosts.filter((id) => hosts.some((h) => h.id === id)) : null)

/** Whether two scopes are the same choice. */
export const sameScope = (a: Scope, b: Scope) => a.scoped === b.scoped && (!a.scoped || (a.hosts.length === b.hosts.length && a.hosts.every((id) => b.hosts.includes(id))))
