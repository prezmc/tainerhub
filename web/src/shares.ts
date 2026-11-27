// Network share volumes: Docker volumes that mount an SMB/CIFS share or an
// NFS export (Resources → Volumes → New network share). Pure, so
// scripts/shares.test.mjs can run it under Node.

export type ShareType = 'cifs' | 'nfs'

/** What the volume list says about a share volume (never its options). */
export type ShareInfo = { type: ShareType; server: string; path: string }

/** The form, as sent to POST /api/hosts/{id}/volumes/share. */
export type ShareForm = {
  name: string
  type: ShareType
  server: string
  path: string
  username: string
  password: string
  domain: string
  uid: string
  gid: string
  fileMode: string
  dirMode: string
  version: string
  readOnly: boolean
}

export const CIFS_VERSIONS = ['3.1.1', '3.02', '3.0', '2.1', '2.0', '1.0', 'default']
export const NFS_VERSIONS = ['4.2', '4.1', '4', '3']

export function blankShare(type: ShareType = 'cifs'): ShareForm {
  return { name: '', type, server: '', path: '', username: '', password: '', domain: '', uid: '1000', gid: '1000', fileMode: '', dirMode: '', version: type === 'cifs' ? '3.0' : '4', readOnly: false }
}

/** "SMB //nas/media" or "NFS nas:/volume1/media". */
export function shareText(s: ShareInfo): string {
  return s.type === 'nfs' ? `NFS ${s.server}:${s.path}` : `SMB //${s.server}/${s.path}`
}

const VOLUME_NAME = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/
const HOST_LABEL = /^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/
const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/

function serverOK(s: string) {
  if (IPV4.test(s) || (/^[0-9a-fA-F:]+$/.test(s) && s.includes(':'))) return true
  return s.length <= 253 && s.split('.').every((l) => HOST_LABEL.test(l))
}

/**
 * The first thing wrong with the form, or null. The hub checks again; this
 * only says it sooner. Values end up in Docker's comma-separated mount
 * options, so commas and quotes are refused.
 */
export function shareProblem(f: ShareForm): string | null {
  if (!f.name) return 'Name the volume.'
  if (!VOLUME_NAME.test(f.name)) return 'The volume name may use letters, digits, _ . and - (not first).'
  if (!f.server) return "Enter the server's IP address or host name."
  if (f.server.startsWith('//') || f.server.includes('/')) return 'Enter just the server here (like 192.168.1.10); the share name goes below.'
  if (!serverOK(f.server)) return `"${f.server}" isn't an IP address or host name.`
  // eslint-disable-next-line no-control-regex
  const control = /[\x00-\x1f\x7f]/
  for (const [label, v] of [
    ['share', f.path],
    ['username', f.username],
    ['domain', f.domain],
    ['password', f.password],
  ] as const) {
    if (control.test(v)) return `The ${label} can't contain line breaks or control characters.`
    if (label === 'password' ? /[,"']/.test(v) : /[,"'\\=]/.test(v))
      return label === 'password'
        ? "The password can't contain commas or quotes: Docker hands it to the mount in a comma-separated list. Change it on the NAS, or write the volume in a compose file yourself."
        : `The ${label} can't contain commas, quotes, backslashes or =.`
  }
  if (f.type === 'cifs') {
    const p = f.path.replace(/^\/+|\/+$/g, '')
    if (!p) return 'Which share? Enter its name, as in //server/share.'
    if (p.includes('..')) return "The share path can't contain .."
    if (f.password && !f.username) return 'A password needs a username.'
    for (const [label, v] of [
      ['UID', f.uid],
      ['GID', f.gid],
    ] as const)
      if (v && !/^\d{1,10}$/.test(v)) return `The ${label} must be a number.`
    for (const [label, v] of [
      ['file mode', f.fileMode],
      ['folder mode', f.dirMode],
    ] as const)
      if (v && !/^[0-7]{3,4}$/.test(v)) return `The ${label} must be octal, like 0775.`
    if (f.version && !CIFS_VERSIONS.includes(f.version)) return `Unknown SMB version ${f.version}.`
  } else {
    if (!f.path.startsWith('/')) return 'The NFS export path starts with /, as in /volume1/media.'
    if (f.path.includes('..') || f.path.includes(' ')) return "The export path can't contain .. or spaces."
    if (f.version && !NFS_VERSIONS.includes(f.version)) return `Unknown NFS version ${f.version}.`
  }
  return null
}

/** The request body: only the fields that apply to the share's type. */
export function shareBody(f: ShareForm) {
  const t = (s: string) => s.trim()
  const base = { name: t(f.name), type: f.type, server: t(f.server), path: t(f.path), version: f.version, readOnly: f.readOnly || undefined }
  if (f.type === 'nfs') return base
  return {
    ...base,
    username: t(f.username) || undefined,
    password: f.password || undefined,
    domain: t(f.domain) || undefined,
    uid: t(f.uid) || undefined,
    gid: t(f.gid) || undefined,
    fileMode: t(f.fileMode) || undefined,
    dirMode: t(f.dirMode) || undefined,
  }
}

/** The line a service adds to mount the volume, for hints. */
export const mountLine = (name: string, target = '/data') => `- ${name}:${target}`
