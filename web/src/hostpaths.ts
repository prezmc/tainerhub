// Recommends a host folder for a new container's volume, from how the host's
// other containers store their data: if sonarr mounts /srv/appdata/sonarr
// and radarr /srv/appdata/radarr, prowlarr's goes in /srv/appdata/prowlarr.
// Without such a pattern, the stack's own folder. Dependency-free for tests.

export type BindMount = { container: string; image: string; source: string; target: string }

export type PathRecommendation = {
  /** What goes in the form: an absolute folder, or ./x in the stack's folder. */
  value: string
  /** The folder on the host, for showing. */
  path: string
  kind: 'convention' | 'stack'
  why: string
}

const DEFAULT_STACKS_DIR = '/opt/tainerhub/stacks'

/** Names a container's app goes by: its name and its image's ("lscr.io/linuxserver/sonarr:4" -> sonarr). */
export function appNames(b: Pick<BindMount, 'container' | 'image'>): string[] {
  const img = (b.image.split('@')[0].split('/').pop() ?? '').split(':')[0]
  const names = [b.container, img].map((n) => n.toLowerCase()).filter((n) => n.length >= 2)
  return [...new Set(names)]
}

const last = (path: string) => path.split(':')[0].split('/').filter(Boolean).pop() ?? ''
const cleanSeg = (s: string) => s.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[-.]+/, '') || 'data'

type Layout = { root: string; app: string; suffix: string; target: string; container: string }

/** Splits a bind into root / <app> / suffix when one of its folders is named after its app. */
export function layoutOf(b: BindMount): Layout | null {
  const names = appNames(b)
  const segs = b.source.split('/').filter(Boolean)
  for (let i = segs.length - 1; i >= 1; i--) {
    if (names.includes(segs[i].toLowerCase())) {
      const suffix = segs.slice(i + 1).join('/')
      return { root: '/' + segs.slice(0, i).join('/'), app: segs[i].toLowerCase(), suffix: suffix ? '/' + suffix : '', target: b.target, container: b.container }
    }
  }
  return null
}

function mostCommon(xs: string[]): string {
  const n = new Map<string, number>()
  for (const x of xs) n.set(x, (n.get(x) ?? 0) + 1)
  return [...n.entries()].sort((a, b) => b[1] - a[1])[0][0]
}

/**
 * Folders to suggest for a volume mounted at target by app (the new
 * container's name), best first. stacksDir is where TainerHub keeps stacks
 * on the host; host is its name, for the explanation.
 */
export function recommendPaths(binds: BindMount[], o: { app: string; target: string; stacksDir?: string; host?: string }): PathRecommendation[] {
  const app = cleanSeg(o.app)
  const target = o.target.split(':')[0]
  const stacksDir = (o.stacksDir || DEFAULT_STACKS_DIR).replace(/\/+$/, '')
  const used = new Map(binds.map((b) => [b.source.replace(/\/+$/, ''), b.container]))
  const on = o.host ? ` on ${o.host}` : ''
  const out: PathRecommendation[] = []

  // The host's own layout: a root folder holding one folder per app.
  const byRoot = new Map<string, Layout[]>()
  for (const b of binds) {
    const l = layoutOf(b)
    if (!l || l.app === app || l.root === stacksDir || l.root.startsWith(stacksDir + '/')) continue
    byRoot.set(l.root, [...(byRoot.get(l.root) ?? []), l])
  }
  const roots = [...byRoot.entries()]
    .map(([root, ls]) => ({ root, ls, apps: [...new Set(ls.map((l) => l.app))] }))
    .sort((a, b) => b.apps.length - a.apps.length || b.ls.length - a.ls.length || a.root.localeCompare(b.root))
  const best = roots[0]
  if (best) {
    const same = best.ls.filter((l) => l.target === target)
    const suffix = same.length ? mostCommon(same.map((l) => l.suffix)) : '/' + cleanSeg(last(target))
    const path = `${best.root}/${app}${suffix}`
    const like = best.apps.slice(0, 3).join(', ') + (best.apps.length > 3 ? ` and ${best.apps.length - 3} more` : '')
    const taken = used.get(path)
    if (!taken)
      out.push({
        value: path,
        path,
        kind: 'convention',
        why: `Follows how ${like} keep${best.apps.length === 1 ? 's' : ''} ${same.length ? `their ${target}` : 'their data'}${on} (${best.root}/<app>${suffix}).`,
      })
  }

  // The stack's own folder.
  const seg = cleanSeg(last(target))
  out.push({
    value: './' + seg,
    path: `${stacksDir}/${app}/${seg}`,
    kind: 'stack',
    why: `In TainerHub's folder for this stack${on}: TainerHub creates it, and it moves with the stack.`,
  })
  return out
}
