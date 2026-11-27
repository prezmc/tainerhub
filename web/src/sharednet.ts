// Containers that run in another container's network (network_mode
// container:<x> or compose's service:<x>), as with a VPN gateway such as
// gluetun. Their ports are published on that container, and they lose their
// network when it stops, restarts or is replaced. Kept free of React so
// `npm test` can check it.

type Ctr = { id: string; name: string; hostId: string; networkOf?: string }

/** Whether dep runs in gw's network. Docker names gw by name or by ID (full or shortened). */
export function usesNetworkOf(dep: Ctr, gw: Ctr): boolean {
  const t = dep.networkOf
  if (!t || dep.id === gw.id || dep.hostId !== gw.hostId) return false
  return t === gw.name || (t.length >= 12 && gw.id.startsWith(t))
}

/** The containers in all that run in gw's network, by name. */
export function networkUsers(gw: Ctr, all: Ctr[]): string[] {
  return all
    .filter((c) => usesNetworkOf(c, gw))
    .map((c) => c.name)
    .sort()
}

/** The container whose network dep runs in, if it's in all. */
export function networkOwner<T extends Ctr>(dep: Ctr, all: T[]): T | undefined {
  return all.find((g) => usesNetworkOf(dep, g))
}

/** What happens to the containers in gw's network when gw stops, restarts or is removed. */
export function sharedNetConsequence(users: string[], action: 'stop' | 'kill' | 'restart' | 'remove'): string {
  const who = users.join(', ')
  const them = users.length === 1 ? 'it' : 'them'
  const lose = `${who} ${users.length === 1 ? 'runs' : 'run'} in this container's network`
  switch (action) {
    case 'restart':
      return `${lose}: ${users.length === 1 ? 'it loses' : 'they lose'} all networking (internet and LAN) when it restarts. Restart ${them} afterwards to reconnect.`
    case 'remove':
      return `${lose}: without it, ${them === 'it' ? 'it has' : 'they have'} no network and can't start again until ${them === 'it' ? "it's" : "they're"} recreated with a new one.`
    default:
      return `${lose}: ${users.length === 1 ? 'it loses' : 'they lose'} all networking (internet and LAN) while it's down. After you start it again, restart ${them} too.`
  }
}
