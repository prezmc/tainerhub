// TainerHub's own containers (the hub, its database, the agents) and the
// volumes and networks they use. Stopping or removing them from TainerHub
// cuts TainerHub off, so the UI warns first. Kept free of React so
// `npm test` can check it.

export type OwnKind = 'hub' | 'database' | 'agent' | 'other'

export interface OwnContainer {
  kind: OwnKind
  /** What it is, in a sentence: "TainerHub's hub". */
  what: string
  /**
   * The agent refuses to stop, kill, pause or remove it: it is in the
   * compose project the host's agent runs in (on the hub's server, that
   * holds the hub and its database too). Otherwise the action would go
   * through.
   */
  refused: boolean
  /** The compose project to bring it back with, if known. */
  project?: string
}

type ContainerLike = { name: string; image: string; project?: string; hostName: string }

/** The image's repository name without registry, owner or tag: "ghcr.io/prezmc/tainerhub-agent:1.2" → "tainerhub-agent". */
export function imageName(ref: string): string {
  let s = ref.split('@')[0]
  const slash = s.lastIndexOf('/')
  const colon = s.lastIndexOf(':')
  if (colon > slash) s = s.slice(0, colon)
  return s.slice(slash + 1)
}

const sameProject = (a?: string, b?: string) => !!a && !!b && a === b

/**
 * Says whether c is one of TainerHub's own containers, and which. ownProject
 * is the compose project the host's agent runs in (Hello.own_project), or
 * "" / undefined when the agent didn't say.
 */
export function ownContainer(c: ContainerLike, ownProject?: string): OwnContainer | null {
  const img = imageName(c.image)
  const inOwn = sameProject(c.project, ownProject)
  let kind: OwnKind | null = null
  if (c.name === 'tainerhub-agent' || img === 'tainerhub-agent') kind = 'agent'
  else if (c.name === 'tainerhub' || img === 'tainerhub') kind = 'hub'
  else if (c.name === 'tainerhub-postgres' || (inOwn && img === 'postgres')) kind = 'database'
  else if (inOwn || c.name.startsWith('tainerhub')) kind = 'other'
  if (!kind) return null
  const what = {
    hub: "TainerHub's hub",
    database: "TainerHub's database",
    agent: `the agent that connects ${c.hostName} to TainerHub`,
    other: 'part of TainerHub',
  }[kind]
  return { kind, what, refused: inOwn, project: c.project || undefined }
}

/** What breaks when the container stops or is removed, in plain words. */
export function ownConsequence(o: OwnContainer, hostName: string, removing: boolean): string {
  switch (o.kind) {
    case 'agent':
      return removing
        ? `${hostName} is cut off from TainerHub: nobody can see or manage its containers here until the agent runs again. Starting it again on the server brings the host back if its volume is kept; without its volume, it has to be reinstalled with a new join token (Hosts → Add host).`
        : `${hostName} is cut off from TainerHub until someone starts the agent again on the server. TainerHub can't start it for you.`
    case 'database':
      return 'TainerHub stops working for every host and every user until the database runs again on the server.' + (removing ? ' If its volume is deleted, every user, host and stack in TainerHub is lost.' : '')
    case 'hub':
      return 'TainerHub goes down for every host and every user, including you, until the hub runs again on the server.' + (removing ? " If its volume is deleted too, TainerHub can't be brought back without a backup: it holds the key every agent's certificate depends on." : '')
    default:
      return 'TainerHub may stop working until it runs again on the server.'
  }
}

const dockerVerb: Record<string, string> = { stop: 'stop', kill: 'kill', pause: 'pause', remove: 'rm -f', recreate: 'stop' }

/**
 * How to do it on purpose, on the server: the docker command for this
 * container, and the compose command that brings it back.
 */
export function ownServerCommands(c: { name: string }, action: string, o: OwnContainer): { run: string; back: string } {
  return {
    run: `docker ${dockerVerb[action] ?? action} ${c.name}`,
    back: o.project ? `docker compose -p ${o.project} up -d` : `docker start ${c.name}`,
  }
}

type ResourceLike = { name: string; project?: string; usedBy: string[] }

/**
 * Says whether a volume or network belongs to TainerHub itself: it is in the
 * compose project the host's agent runs in, its name starts with
 * "tainerhub" (as in the install files: tainerhub-data, tainerhub-db,
 * tainerhub-agent, maybe prefixed with the project), or one of TainerHub's
 * containers uses it.
 */
export function ownResource(r: ResourceLike, ownProject?: string): boolean {
  if (sameProject(r.project, ownProject)) return true
  if (/(^|_)tainerhub/.test(r.name)) return true
  return r.usedBy.some((u) => ownContainer({ name: u, image: '', hostName: '' }) !== null)
}
