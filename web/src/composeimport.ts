// Turns a pasted compose file (the kind project READMEs publish) into the
// fields of the New container form. The text is untrusted: it's parsed with
// a bounded YAML parser, only known keys are read, and every value ends up in
// a form field that composeFor() quotes again before anything is deployed.
// Whatever can't be mapped becomes a note: nothing is dropped silently.
//
// Pure apart from the `yaml` package, so scripts/composeimport.test.mjs can
// run it under Node.

import { parseDocument } from 'yaml'
import { CAPS, DROP_CAPS, blankAdvanced, type Advanced, type Env, type KV, type Port, type Section, type Vol } from './compose.ts'
import { isSecretKey, looksLikePlaceholder, suggestSource } from './placeholder.ts'
import { isUtcLike, portConflicts } from './recommend.ts'

export const MAX_COMPOSE_BYTES = 256 << 10

export type Note = { level: 'warn' | 'info'; msg: string }
/** A variable the file uses. def is its default (from ${X:-d} or ${X-d}); required when ${X:?msg} or ${X?msg}. */
export type VarRef = { name: string; def?: string; required?: boolean }

type Obj = Record<string, unknown>
export type Parsed =
  | { ok: false; error: string }
  | {
      ok: true
      /** The top level as compose sees it, with a bare service wrapped in services:. */
      doc: Obj
      services: string[]
      /** The project's own name (top-level name:), if any. */
      project: string
      notes: Note[]
    }

export type Imported = {
  service: string
  image: string // repository, or the full reference when it has a digest
  tag: string
  name: string
  restart: string
  ports: Port[]
  env: Env[]
  vols: Vol[]
  adv: Advanced
  notes: Note[]
  /** Advanced sections worth opening: they were filled or need a look. */
  open: Section[]
  /** Values that look like examples from the docs, to ask the user about. */
  placeholders: Placeholder[]
  /** Values TainerHub filled in or changed, to show and let the user adjust. */
  changes: Change[]
}

/**
 * A value TainerHub chose rather than took from the file: a default it
 * filled in, or a value it had to change for the form. from is what the
 * file had ("" when nothing). input says how the user can adjust it (none:
 * only in the form afterwards); edits[id] holds their value.
 */
export type Change = { id: string; label: string; from: string; to: string; why: string; input?: 'text' | 'restart' | 'variable'; keepable?: boolean }

/**
 * What the importer knows about where the container will run, for its
 * recommendations: this computer's time zone, and the host ports already
 * in use on the chosen host ("8080/tcp" -> container).
 */
export type ImportContext = { timeZone?: string; usedPorts?: Map<string, string>; hostName?: string }

/**
 * A value that looks like an example from the docs ("/path/to/data",
 * "changeme"). id is how replacements refer to it: volume:<row>, env:<key>
 * or label:<key>. suggestion is a sensible replacement, when there is one.
 */
export type Placeholder = { id: string; kind: 'volume' | 'env' | 'label'; label: string; value: string; suggestion?: string; secret?: boolean; target?: string }

const isObj = (v: unknown): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v)
const has = (o: Obj, k: string) => Object.prototype.hasOwnProperty.call(o, k)
const str = (v: unknown) => (v === null || v === undefined ? '' : typeof v === 'object' ? '' : String(v))
const code = (s: string) => `“${s.length > 80 ? s.slice(0, 77) + '…' : s}”`

/** A stack/container name from any text: lowercase letters, digits, - and _. */
export function cleanName(s: string) {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, '-')
    .replace(/^[-_]+/, '')
    .replace(/-+$/, '')
    .slice(0, 63)
}

// ---------------- parsing ----------------

const looksLikeService = (v: unknown) => isObj(v) && (has(v, 'image') || has(v, 'build'))

/** Parses the pasted text. Never throws. */
export function parseCompose(text: string): Parsed {
  if (!text.trim()) return { ok: false, error: 'Paste a compose file first.' }
  if (new TextEncoder().encode(text).length > MAX_COMPOSE_BYTES) return { ok: false, error: `That's over ${MAX_COMPOSE_BYTES >> 10} KB; a compose file is usually a few KB.` }
  if (/^\s*(sudo\s+)?docker\s+(container\s+)?(run|create)\b/.test(text)) return { ok: false, error: 'That looks like a docker run command. Paste the compose (YAML) version from the project’s docs instead.' }

  let data: unknown
  try {
    // Core schema (YAML 1.2): "no", "on" and "yes" stay strings, as compose reads them.
    // merge: compose files often share settings with <<: *anchor.
    const doc = parseDocument(text, { schema: 'core', merge: true, uniqueKeys: true, prettyErrors: true, strict: true })
    if (doc.errors.length) return { ok: false, error: `This isn't valid YAML: ${doc.errors[0].message.split('\n')[0]}` }
    // maxAliasCount stops "billion laughs" alias bombs.
    data = doc.toJS({ maxAliasCount: 200 })
  } catch (e) {
    return { ok: false, error: `This isn't valid YAML: ${e instanceof Error ? e.message.split('\n')[0] : String(e)}` }
  }

  const notes: Note[] = []
  if (!isObj(data)) return { ok: false, error: 'This isn’t a compose file: expected keys like services: and image:.' }
  let doc = data
  if (!has(doc, 'services')) {
    if (looksLikeService(doc)) {
      // Just a service's settings: image:, ports:, …
      const name = cleanName(str(doc.container_name) || imageBase(str(doc.image))) || 'app'
      doc = { services: { [name]: doc } }
      notes.push({ level: 'info', msg: 'The text had no services: section, so it was read as one service.' })
    } else if (Object.keys(doc).length && Object.values(doc).every(looksLikeService)) {
      // Services without the services: line above them.
      doc = { services: doc }
      notes.push({ level: 'info', msg: 'The text had no services: line, so its top-level entries were read as services.' })
    } else return { ok: false, error: 'No services: section found. Paste the whole compose file, starting at services:.' }
  }
  if (!isObj(doc.services) || !Object.keys(doc.services).length) return { ok: false, error: 'services: is empty.' }
  const services = Object.keys(doc.services).filter((k) => isObj((doc.services as Obj)[k]))
  if (!services.length) return { ok: false, error: 'None of the services has any settings.' }

  for (const k of Object.keys(doc)) {
    if (k === 'version') notes.push({ level: 'info', msg: 'version: is obsolete in compose and was ignored.' })
    else if (k === 'secrets' || k === 'configs') notes.push({ level: 'warn', msg: `Top-level ${k}: can’t be set up from this form. Deploy as a stack to keep them.` })
    else if (k === 'include') notes.push({ level: 'warn', msg: 'include: pulls in other files, which the form can’t read. Deploy as a stack and add them there.' })
    else if (!['services', 'networks', 'volumes', 'name'].includes(k) && !k.startsWith('x-')) notes.push({ level: 'warn', msg: `Top-level ${k}: isn’t a compose key the form understands; it was left out.` })
  }
  return { ok: true, doc, services, project: str(doc.name), notes }
}

// ---------------- variables ----------------

/**
 * Expands compose variables in one string: $$ is a literal $, and $X, ${X},
 * ${X:-default}, ${X-default}, ${X:?error}, ${X?error}, ${X:+alt}, ${X+alt}
 * work as in compose, with nested defaults. values holds what the user typed;
 * an empty value counts as unset. Variables used without a value are added
 * to missing. With values null it only collects the variables (into refs).
 */
export function interpolate(s: string, values: Record<string, string> | null, refs: Map<string, VarRef>, missing: Set<string>): string {
  let out = ''
  let i = 0
  const get = (n: string) => (values && Object.prototype.hasOwnProperty.call(values, n) && values[n] !== '' ? values[n] : undefined)
  const note = (r: VarRef) => {
    const old = refs.get(r.name)
    if (!old) refs.set(r.name, r)
    else {
      if (old.def === undefined && r.def !== undefined) old.def = r.def
      if (r.required) old.required = true
    }
  }
  while (i < s.length) {
    const c = s[i]
    if (c !== '$') {
      out += c
      i++
      continue
    }
    const n = s[i + 1]
    if (n === '$') {
      out += '$'
      i += 2
      continue
    }
    const bare = /^[A-Za-z_][A-Za-z0-9_]*/.exec(s.slice(i + 1))
    if (bare) {
      note({ name: bare[0] })
      const v = get(bare[0])
      if (v === undefined) missing.add(bare[0])
      out += v ?? ''
      i += 1 + bare[0].length
      continue
    }
    if (n !== '{') {
      out += '$'
      i++
      continue
    }
    // ${...}: find the matching brace (defaults may nest ${...}).
    let depth = 0
    let j = i + 1
    for (; j < s.length; j++) {
      if (s[j] === '{') depth++
      else if (s[j] === '}' && --depth === 0) break
    }
    if (j >= s.length) {
      out += s.slice(i) // unclosed: keep it as text
      break
    }
    const inner = s.slice(i + 2, j)
    i = j + 1
    const m = /^([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])([\s\S]*))?$/.exec(inner)
    if (!m) {
      out += '${' + inner + '}'
      continue
    }
    const [, name, op = '', arg = ''] = m
    const v = get(name)
    if (op === ':-' || op === '-') {
      // the default may itself use variables
      const def = interpolate(arg, values, refs, missing)
      note({ name, def })
      out += v ?? def
    } else if (op === ':+' || op === '+') {
      note({ name })
      out += v !== undefined ? interpolate(arg, values, refs, missing) : ''
    } else {
      note({ name, required: op !== '' || undefined })
      if (v === undefined) missing.add(name)
      out += v ?? ''
    }
  }
  return out
}

/** Expands every string (not keys) in a value. */
function expand(v: unknown, values: Record<string, string> | null, refs: Map<string, VarRef>, missing: Set<string>, depth = 0): unknown {
  if (depth > 40) return v
  if (typeof v === 'string') return interpolate(v, values, refs, missing)
  if (Array.isArray(v)) return v.map((x) => expand(x, values, refs, missing, depth + 1))
  if (isObj(v)) {
    const o: Obj = {}
    for (const [k, x] of Object.entries(v)) Object.defineProperty(o, k, { value: expand(x, values, refs, missing, depth + 1), enumerable: true, writable: true, configurable: true })
    return o
  }
  return v
}

/** The variables a service (and the top-level networks and volumes it can use) refers to; every service when service is omitted. */
export function variablesOf(p: Extract<Parsed, { ok: true }>, service?: string): VarRef[] {
  const refs = new Map<string, VarRef>()
  const svcs = p.doc.services as Obj
  const parts = service ? [svcs[service]] : Object.values(svcs)
  expand([parts, p.doc.networks, p.doc.volumes], null, refs, new Set())
  return [...refs.values()]
}

// ---------------- mapping helpers ----------------

function imageBase(ref: string) {
  return (ref.split('@')[0].split('/').pop() ?? '').split(':')[0]
}

/** "lscr.io/org/app:1.2" -> { image: "lscr.io/org/app", tag: "1.2" }. A digest stays in image. */
export function splitImage(ref: string): { image: string; tag: string } {
  if (ref.includes('@')) return { image: ref, tag: 'latest' }
  const colon = ref.lastIndexOf(':')
  if (colon > ref.lastIndexOf('/')) return { image: ref.slice(0, colon), tag: ref.slice(colon + 1) || 'latest' }
  return { image: ref, tag: 'latest' }
}

/** Joins arguments into one line that splitArgs() splits back into the same arguments. */
export function joinArgs(args: string[]) {
  return args.map((a) => (/^[A-Za-z0-9_@%+=:,./-]+$/.test(a) ? a : `'${a.replaceAll("'", `'\\''`)}'`)).join(' ')
}

/** "512m", "1.5GB", 268435456 -> a number and m/g for the form. */
export function parseSize(v: unknown): { value: string; unit: 'm' | 'g' } | null {
  const m = /^\s*(\d+(?:\.\d+)?)\s*([bkmg]?)b?\s*$/i.exec(str(v))
  if (!m) return null
  const n = +m[1]
  const u = m[2].toLowerCase()
  const fmt = (x: number) => String(Math.round(x * 1000) / 1000)
  if (u === 'g') return { value: fmt(n), unit: 'g' }
  if (u === 'm') return { value: fmt(n), unit: 'm' }
  if (u === 'k') return { value: fmt(n / 1024), unit: 'm' }
  return { value: fmt(n / 1048576), unit: 'm' }
}

const list = (v: unknown): unknown[] => (Array.isArray(v) ? v : v === undefined || v === null ? [] : [v])

/** KEY=VALUE list items or a mapping, as pairs. A bare KEY (or KEY: with no value) has value null. */
function pairs(v: unknown, sep = '='): { key: string; value: string | null }[] {
  if (isObj(v)) return Object.entries(v).map(([key, x]) => ({ key, value: x === null || x === undefined ? null : str(x) }))
  return list(v).map((x) => {
    const s = str(x)
    const i = s.indexOf(sep)
    return i < 0 ? { key: s, value: null } : { key: s.slice(0, i), value: s.slice(i + 1) }
  })
}

const isLocalBind = (ip: string) => ip !== '' && ip !== '0.0.0.0' && ip !== '::' && ip !== '[::]'

function range(s: string): number[] | null {
  const m = /^(\d+)(?:-(\d+))?$/.exec(s)
  if (!m) return null
  const a = +m[1]
  const b = m[2] ? +m[2] : a
  if (b < a || b - a > 99) return null
  return Array.from({ length: b - a + 1 }, (_, i) => a + i)
}

/** One ports: entry as form rows, or a note why it can't be. */
function mapPort(entry: unknown): { rows: Port[]; note?: Note } {
  let ip = ''
  let host = ''
  let ctr = ''
  let proto = 'tcp'
  const shown = isObj(entry) ? JSON.stringify(entry) : str(entry)
  if (isObj(entry)) {
    ctr = str(entry.target)
    host = str(entry.published)
    ip = str(entry.host_ip)
    proto = str(entry.protocol) || 'tcp'
    const extra = Object.keys(entry).filter((k) => !['target', 'published', 'host_ip', 'protocol', 'mode', 'name', 'app_protocol'].includes(k))
    if (extra.length) return { rows: [], note: { level: 'warn', msg: `Port ${code(shown)} uses ${extra.join(', ')}, which the form can’t set; it was left out.` } }
  } else {
    let s = str(entry).trim()
    const slash = s.lastIndexOf('/')
    if (slash >= 0) {
      proto = s.slice(slash + 1)
      s = s.slice(0, slash)
    }
    if (s.startsWith('[')) {
      const end = s.indexOf(']:')
      if (end < 0) return { rows: [], note: { level: 'warn', msg: `Port ${code(shown)} couldn’t be read; it was left out.` } }
      ip = s.slice(0, end + 1)
      s = s.slice(end + 2)
    }
    const parts = s.split(':')
    if (parts.length === 1) ctr = parts[0]
    else if (parts.length === 2) [host, ctr] = parts
    else if (parts.length === 3 && !ip) [ip, host, ctr] = parts
    else return { rows: [], note: { level: 'warn', msg: `Port ${code(shown)} couldn’t be read; it was left out.` } }
  }
  proto = proto.toLowerCase()
  if (proto !== 'tcp' && proto !== 'udp') return { rows: [], note: { level: 'warn', msg: `Port ${code(shown)}: the form supports tcp and udp, not ${proto}; it was left out.` } }
  if (isLocalBind(ip))
    return {
      rows: [],
      note: { level: 'warn', msg: `Port ${code(shown)} is published on ${ip} only. The form publishes on every address, so it was left out: add it back by hand if that’s OK, or deploy as a stack.` },
    }
  const cs = range(ctr)
  const hs = host ? range(host) : null
  if (!cs || (host && !hs)) return { rows: [], note: { level: 'warn', msg: `Port ${code(shown)} couldn’t be read; it was left out.` } }
  if (hs && hs.length !== cs.length) return { rows: [], note: { level: 'warn', msg: `Port ${code(shown)} maps a range of host ports to a different number of container ports, which the form can’t do; it was left out.` } }
  return { rows: cs.map((c, i) => ({ host: hs ? String(hs[i]) : '', container: String(c), proto: proto as Port['proto'] })) }
}

/** One volumes: entry as a form row, or a note why it can't be. */
function mapVolume(entry: unknown, notes: Note[]): Vol | null {
  if (isObj(entry)) {
    const type = str(entry.type) || 'volume'
    const shown = JSON.stringify(entry)
    if (type !== 'bind' && type !== 'volume') {
      notes.push({ level: 'warn', msg: `Volume ${code(shown)} is a ${type} mount, which the form can’t set; it was left out.` })
      return null
    }
    const extra = Object.keys(entry).filter((k) => !['type', 'source', 'target', 'read_only'].includes(k))
    if (extra.length) notes.push({ level: 'warn', msg: `Volume ${code(shown)}: ${extra.join(', ')} can’t be set from the form and was left out.` })
    return { source: str(entry.source), target: str(entry.target) + (entry.read_only === true ? ':ro' : '') }
  }
  const s = str(entry).trim()
  const parts = s.split(':')
  if (parts.length === 1) {
    notes.push({ level: 'warn', msg: `Volume ${code(s)} has no source, so its data would be lost on updates. Give it a volume name (like data) or a folder.` })
    return { source: '', target: s }
  }
  const [source, target, ...mode] = parts
  const opts = mode.join(':')
  return { source, target: target + (opts && opts !== 'rw' ? `:${opts}` : '') }
}

const KNOWN_SERVICE_KEYS = new Set([
  'image',
  'container_name',
  'restart',
  'ports',
  'expose',
  'volumes',
  'environment',
  'env_file',
  'networks',
  'network_mode',
  'hostname',
  'extra_hosts',
  'dns',
  'labels',
  'command',
  'entrypoint',
  'working_dir',
  'user',
  'init',
  'read_only',
  'stop_grace_period',
  'mem_limit',
  'cpus',
  'shm_size',
  'devices',
  'deploy',
  'runtime',
  'gpus',
  'healthcheck',
  'cap_add',
  'cap_drop',
  'privileged',
  'security_opt',
  'logging',
])

const STACK_HINT = 'Deploy the whole file as a stack to keep it.'

/**
 * Maps one service onto the form. values are the variables the user filled
 * in (an empty string means "no value": defaults apply, else it's empty).
 */
export function importService(
  p: Extract<Parsed, { ok: true }>,
  service: string,
  values: Record<string, string> = {},
  edits: Record<string, string> = {},
  ctx: ImportContext = {},
): Imported {
  // The TZ environment variable gets this computer's zone instead of UTC (see below).
  const myZone = ctx.timeZone && !isUtcLike(ctx.timeZone) ? ctx.timeZone : ''
  const notes: Note[] = [...p.notes]
  const warn = (msg: string) => notes.push({ level: 'warn', msg })
  const info = (msg: string) => notes.push({ level: 'info', msg })
  const changes: Change[] = []
  /** Records a change and returns the value to use: the user's edit, if any. to is then that value. */
  const change = (c: Change) => {
    const e = c.input && c.input !== 'variable' ? edits[`change:${c.id}`]?.trim() : undefined
    const to = e || c.to
    changes.push({ ...c, to })
    return to
  }
  const refs = new Map<string, VarRef>()
  const missing = new Set<string>()
  const svcRaw = (p.doc.services as Obj)[service]
  const s = expand(svcRaw, values, refs, missing) as Obj
  const topNets = (isObj(p.doc.networks) ? expand(p.doc.networks, values, refs, missing) : {}) as Obj
  const topVols = (isObj(p.doc.volumes) ? expand(p.doc.volumes, values, refs, missing) : {}) as Obj
  for (const v of refs.values()) {
    // Listed even once the user has typed a value, so the row stays put while they edit it.
    if (v.def !== undefined) {
      const own = values[v.name]
      if (v.name === 'TZ' && !own && myZone && isUtcLike(v.def)) continue // listed as the TZ change instead
      changes.push({ id: `var:${v.name}`, label: '${' + v.name + '}', from: '', to: own || v.def, why: own ? `Your value (the file’s default is ${v.def === '' ? 'empty' : v.def}).` : 'Not set, so the file’s own default is used.', input: 'variable' })
    }
  }
  for (const name of missing) warn(`\${${name}} has no value, so it was left empty where the file uses it. Enter a value for it before filling the form, or fix that field afterwards.`)

  const adv = blankAdvanced()
  const others = p.services.filter((x) => x !== service)
  if (others.length) warn(`Only ${service} was imported; ${others.join(', ')} ${others.length === 1 ? 'was' : 'were'} left out. ${STACK_HINT}`)

  // image
  const ref = str(s.image).trim()
  const split = ref ? splitImage(ref) : { image: '', tag: 'latest' }
  const image = split.image
  let tag = split.tag
  if (ref && !ref.includes('@') && !(ref.split('/').pop() ?? '').includes(':'))
    tag = change({ id: 'tag', label: 'Version (tag)', from: '', to: 'latest', why: 'The file names no version, so Docker would use latest. Pinning a version makes updates deliberate.', input: 'text' })
  if (!ref) warn(has(s, 'build') ? `${service} is built from source (build:), and the form can only run a ready-made image. ${STACK_HINT}` : `${service} has no image:.`)
  else if (has(s, 'build')) warn(`build: was left out: the form runs the image ${code(ref)} instead of building it. ${STACK_HINT}`)

  // name
  const wanted = str(s.container_name) || service
  let name = cleanName(wanted) || cleanName(imageBase(ref)) || 'app'
  if (name !== wanted)
    name = change({ id: 'name', label: 'Name', from: wanted, to: name, why: 'Names use lowercase letters, digits, - and _.', input: 'text' })

  // restart
  let restart = 'unless-stopped'
  const r = s.restart === false ? 'no' : str(s.restart)
  if (!has(s, 'restart'))
    restart = change({ id: 'restart', label: 'Restart', from: '', to: 'unless-stopped', why: 'The file sets none (Docker’s default is never), so the app would stay down after a reboot.', input: 'restart' })
  else if (['no', 'always', 'unless-stopped', 'on-failure'].includes(r)) restart = r
  else if (/^on-failure:\d+$/.test(r))
    restart = change({ id: 'restart', label: 'Restart', from: r, to: 'on-failure', why: 'The form can’t limit the number of retries.', input: 'restart' })
  else restart = change({ id: 'restart', label: 'Restart', from: r, to: 'unless-stopped', why: 'That isn’t a restart policy.', input: 'restart' })
  if (!['no', 'always', 'unless-stopped', 'on-failure'].includes(restart)) {
    restart = 'unless-stopped'
    const c = changes.find((x) => x.id === 'restart')
    if (c) c.to = restart
  }

  // networking
  const nm = str(s.network_mode)
  if (nm === 'host' || nm === 'none') adv.networkMode = nm
  else if (nm === 'bridge')
    change({ id: 'network-mode', label: 'Network mode', from: 'bridge', to: 'the stack’s own network', why: 'Works the same for most apps, and lets it join other networks. Change it under Advanced → Networking.' })
  else if (nm) warn(`network_mode: ${code(nm)} can’t be set from the form; it was left out. ${STACK_HINT}`)

  const ports: Port[] = []
  for (const e of list(s.ports)) {
    const { rows, note } = mapPort(e)
    ports.push(...rows)
    if (note) notes.push(note)
  }
  const on = ctx.hostName ? ` on ${ctx.hostName}` : ''
  if (ctx.usedPorts && !adv.networkMode) {
    for (const c of portConflicts(ports, ctx.usedPorts)) {
      if (!c.suggestion) continue
      const row = ports[c.index]
      row.host = change({
        id: `port:${c.index}`,
        label: `Host port ${c.key}`,
        from: row.host,
        to: c.suggestion,
        why: `${c.key} is already used by ${c.by}${on}, so the container wouldn't start. The app still listens on ${row.container} inside it; only the port you open it on changes.`,
        input: 'text',
        keepable: true,
      })
    }
  }
  if (adv.networkMode && ports.length) info(`ports: are ignored with network_mode: ${adv.networkMode}: the app uses the host’s ports directly.`)
  if (has(s, 'expose')) info('expose: was left out: containers on the same network reach each other’s ports without it.')

  if (isObj(s.networks) || Array.isArray(s.networks)) {
    const names = isObj(s.networks) ? Object.keys(s.networks) : list(s.networks).map(str)
    for (const n of names) {
      const opts = isObj(s.networks) ? (s.networks as Obj)[n] : null
      if (isObj(opts) && Object.keys(opts).length) warn(`Network ${n}: ${Object.keys(opts).join(', ')} can’t be set from the form and ${Object.keys(opts).length === 1 ? 'was' : 'were'} left out.`)
      if (n === 'default') continue
      const def = isObj(topNets[n]) ? (topNets[n] as Obj) : {}
      const real = str(def.name) || n
      const extra = Object.keys(def).filter((k) => !['name', 'external'].includes(k))
      if (def.external === true || (isObj(def.external) && has(def.external, 'name'))) {
        const ext = isObj(def.external) ? str((def.external as Obj).name) || real : real
        if (!adv.networks.includes(ext)) adv.networks.push(ext)
      } else if (!adv.newNetwork) {
        adv.newNetwork = real
        if (extra.length) warn(`Network ${n}: ${extra.join(', ')} can’t be set from the form; it’s created as a plain network.`)
      } else warn(`The form can create one network; ${real} was left out (${adv.newNetwork} is created). ${STACK_HINT}`)
    }
    if (adv.networkMode && names.some((n) => n !== 'default')) warn(`networks: can’t be joined with network_mode: ${adv.networkMode}. Clear one of them under Advanced → Networking.`)
  }
  if (adv.networks.length) info(`${adv.networks.join(', ')} ${adv.networks.length === 1 ? 'is an existing network' : 'are existing networks'}: make sure ${adv.networks.length === 1 ? 'it exists' : 'they exist'} on the host you pick.`)

  adv.hostname = str(s.hostname)
  for (const h of isObj(s.extra_hosts) ? pairs(s.extra_hosts) : list(s.extra_hosts).map((x) => {
    const t = str(x)
    const i = t.search(/[=:]/)
    return i < 0 ? { key: t, value: '' } : { key: t.slice(0, i), value: t.slice(i + 1) }
  }))
    adv.extraHosts.push({ key: h.key, value: (h.value ?? '').replace(/^\[(.*)\]$/, '$1') } as KV)
  adv.dns = list(s.dns).map(str).join(' ')

  // environment
  const env: Env[] = []
  for (const e of pairs(s.environment)) {
    if (!e.key) continue
    if (e.value === null) warn(`${e.key} takes its value from the shell that runs compose, which TainerHub doesn’t have. Enter a value under Environment variables.`)
    env.push({ key: e.key, value: e.value ?? '' })
  }
  const tz = env.find((e) => e.key === 'TZ')
  if (tz && myZone && isUtcLike(tz.value))
    tz.value = change({ id: 'tz', label: 'TZ', from: tz.value, to: myZone, why: `This computer’s time zone, so the app’s times match yours. ${tz.value} is the usual example in READMEs.`, input: 'text', keepable: true })
  if (has(s, 'env_file')) {
    const files = list(s.env_file).map((f) => (isObj(f) ? str(f.path) : str(f)))
    warn(`env_file: ${files.join(', ')} can’t be read from here. Paste its KEY=value lines under Advanced → Env file.`)
  }

  // volumes
  const vols: Vol[] = []
  for (const e of list(s.volumes)) {
    const v = mapVolume(e, notes)
    if (!v) continue
    vols.push(v)
    if (v.source.startsWith('~')) warn(`Volume ${code(v.source)}: ~ isn’t expanded on the host. Use a full path like /srv/app, or ./config in the stack’s folder.`)
    const def = topVols[v.source]
    if (isObj(def)) {
      const keys = Object.keys(def)
      if (def.external === true) warn(`Volume ${v.source} is external (an existing volume), but the form creates a new one for the stack. ${STACK_HINT}`)
      else if (keys.length) warn(`Volume ${v.source}: ${keys.join(', ')} can’t be set from the form; it’s created as a plain volume.`)
    }
  }

  // labels
  for (const l of pairs(s.labels)) if (l.key) adv.labels.push({ key: l.key, value: l.value ?? '' })

  // command & runtime
  const argLine = (v: unknown, key: string) => {
    if (Array.isArray(v)) {
      if (!v.length) warn(`${key}: [] (clearing the image’s ${key}) can’t be set from the form; it was left out.`)
      return joinArgs(v.map(str))
    }
    if (v === '' && key === 'entrypoint') warn('entrypoint: "" (clearing the image’s entrypoint) can’t be set from the form; it was left out.')
    return str(v)
  }
  adv.command = argLine(s.command, 'command')
  adv.entrypoint = argLine(s.entrypoint, 'entrypoint')
  adv.workingDir = str(s.working_dir)
  adv.user = str(s.user)
  adv.init = s.init === true
  adv.readOnly = s.read_only === true
  adv.stopGrace = str(s.stop_grace_period)

  // resources
  const deploy = isObj(s.deploy) ? s.deploy : {}
  const res = isObj(deploy.resources) ? deploy.resources : {}
  const limits = isObj(res.limits) ? res.limits : {}
  const mem = has(s, 'mem_limit') ? s.mem_limit : limits.memory
  if (mem !== undefined) {
    const m = parseSize(mem)
    if (m) {
      ;[adv.memory, adv.memoryUnit] = [m.value, m.unit]
      if (!/^\s*[\d.]+\s*[mg]b?\s*$/i.test(str(mem))) change({ id: 'memory', label: 'Memory limit', from: str(mem), to: `${m.value} ${m.unit === 'g' ? 'GB' : 'MB'}`, why: 'The form sets limits in MB or GB.' })
    } else warn(`Memory limit ${code(str(mem))} couldn’t be read; it was left out.`)
  }
  adv.cpus = str(has(s, 'cpus') ? s.cpus : limits.cpus)
  if (has(s, 'shm_size')) {
    const m = parseSize(s.shm_size)
    if (m) [adv.shm, adv.shmUnit] = [m.value, m.unit]
    else warn(`shm_size ${code(str(s.shm_size))} couldn’t be read; it was left out.`)
  }
  for (const d of list(s.devices)) {
    if (isObj(d)) adv.devices.push([str(d.source), str(d.target), str(d.permissions)].filter(Boolean).join(':'))
    else adv.devices.push(str(d))
  }
  // GPUs: deploy.resources.reservations.devices, runtime: nvidia or gpus:
  const reservations = isObj(res.reservations) ? res.reservations : {}
  for (const d of list(reservations.devices)) {
    if (!isObj(d)) continue
    const caps = list(d.capabilities).map(str)
    if (str(d.driver) === 'nvidia' || caps.includes('gpu')) {
      adv.gpu = true
      if ((has(d, 'count') && str(d.count) !== 'all') || has(d, 'device_ids')) warn('The file picks particular GPUs; the form gives the container all NVIDIA GPUs.')
    } else warn(`A device reservation (${code(JSON.stringify(d))}) can’t be set from the form; it was left out.`)
  }
  if (has(s, 'gpus')) {
    adv.gpu = true
    if (str(s.gpus) !== 'all') warn('gpus: picks particular GPUs; the form gives the container all NVIDIA GPUs.')
  }
  if (has(s, 'runtime')) {
    if (str(s.runtime) === 'nvidia') {
      adv.gpu = true
      change({ id: 'gpu', label: 'GPU', from: 'runtime: nvidia', to: 'all NVIDIA GPUs (a GPU reservation)', why: 'Does the same with current Docker. Change it under Advanced → Resources.' })
    } else warn(`runtime: ${code(str(s.runtime))} can’t be set from the form; it was left out.`)
  }
  for (const k of Object.keys(deploy)) if (k !== 'resources') warn(`deploy.${k} is for Docker Swarm or can’t be set from the form; it was left out.`)
  for (const k of Object.keys(res)) if (k !== 'limits' && k !== 'reservations') warn(`deploy.resources.${k} can’t be set from the form; it was left out.`)
  for (const k of Object.keys(limits)) if (k !== 'memory' && k !== 'cpus') warn(`deploy.resources.limits.${k} can’t be set from the form; it was left out.`)
  for (const k of Object.keys(reservations)) if (k !== 'devices') warn(`deploy.resources.reservations.${k} can’t be set from the form; it was left out.`)

  // health check
  if (isObj(s.healthcheck)) {
    const h = s.healthcheck
    const test = h.test
    if (h.disable === true || (Array.isArray(test) && str(test[0]) === 'NONE')) adv.health = 'disable'
    else if (test !== undefined) {
      adv.health = 'custom'
      if (Array.isArray(test)) {
        const [kind, ...rest] = test.map(str)
        if (kind === 'CMD-SHELL') adv.hcTest = rest.join(' ')
        else if (kind === 'CMD')
          adv.hcTest = change({ id: 'health', label: 'Health check', from: JSON.stringify(test), to: joinArgs(rest), why: 'The form runs health checks through the shell (CMD-SHELL). Images without /bin/sh need the file as a stack instead.', input: 'text' })
        else adv.hcTest = joinArgs([kind, ...rest])
      } else adv.hcTest = str(test)
      adv.hcInterval = str(h.interval)
      adv.hcTimeout = str(h.timeout)
      adv.hcRetries = str(h.retries)
      adv.hcStart = str(h.start_period)
    }
    const extra = Object.keys(h).filter((k) => !['test', 'disable', 'interval', 'timeout', 'retries', 'start_period'].includes(k))
    if (extra.length) warn(`healthcheck: ${extra.join(', ')} can’t be set from the form; it was left out.`)
  }

  // security
  const capList = (v: unknown, allowed: readonly string[], key: string) => {
    const out: string[] = []
    for (const c of list(v).map((x) => str(x).toUpperCase().replace(/^CAP_/, ''))) {
      if (allowed.includes(c)) out.push(c)
      else warn(`${key}: ${c} isn’t one of the capabilities the form offers; it was left out. ${STACK_HINT}`)
    }
    return [...new Set(out)]
  }
  adv.capAdd = capList(s.cap_add, CAPS, 'cap_add')
  adv.capDrop = capList(s.cap_drop, DROP_CAPS, 'cap_drop')
  adv.privileged = s.privileged === true
  if (adv.privileged) warn('privileged: true gives the container full root access to the host. You’ll be asked to confirm before it deploys.')
  for (const o of list(s.security_opt).map(str)) {
    if (/^no-new-privileges([:=]true)?$/.test(o)) adv.noNewPrivileges = true
    else if (!/^no-new-privileges[:=]false$/.test(o)) warn(`security_opt: ${code(o)} can’t be set from the form; it was left out.`)
  }

  // logging
  if (isObj(s.logging)) {
    const driver = str(s.logging.driver) || 'json-file'
    const opts = isObj(s.logging.options) ? s.logging.options : {}
    if (driver !== 'json-file') warn(`The ${driver} logging driver can’t be set from the form; Docker’s default is used.`)
    else {
      if (has(opts, 'max-size') || has(opts, 'max-file')) {
        adv.logLimit = true
        if (has(opts, 'max-size')) adv.logMaxSize = str(opts['max-size']).toLowerCase()
        else adv.logMaxSize = change({ id: 'log-size', label: 'Log max size', from: '', to: adv.logMaxSize, why: 'The file only sets the number of files; the form needs both.', input: 'text' })
        if (has(opts, 'max-file')) adv.logMaxFile = str(opts['max-file'])
        else adv.logMaxFile = change({ id: 'log-files', label: 'Log files to keep', from: '', to: adv.logMaxFile, why: 'The file only sets the size; the form needs both.', input: 'text' })
      }
      const extra = Object.keys(opts).filter((k) => k !== 'max-size' && k !== 'max-file')
      if (extra.length) warn(`Logging options ${extra.join(', ')} can’t be set from the form; they were left out.`)
    }
  }

  // everything else
  for (const k of Object.keys(s)) {
    if (KNOWN_SERVICE_KEYS.has(k) || k === 'build') continue
    if (k === 'depends_on') {
      const deps = isObj(s.depends_on) ? Object.keys(s.depends_on) : list(s.depends_on).map(str)
      warn(`depends_on (${deps.join(', ')}) was left out: the form runs one container. ${STACK_HINT}`)
    } else if (k === 'links') warn(`links: was left out: the form runs one container. ${STACK_HINT}`)
    else if (k.startsWith('x-')) info(`${k} is an extension field, which compose ignores; it was left out.`)
    else warn(`${k}: can’t be set from the New container form, so it was left out. ${STACK_HINT}`)
  }

  const open = new Set<Section>()
  if (adv.networkMode || adv.networks.length || adv.newNetwork || adv.hostname || adv.extraHosts.length || adv.dns) open.add('network')
  if (adv.labels.length) open.add('labels')
  if (adv.command || adv.entrypoint || adv.workingDir || adv.user || adv.init || adv.readOnly || adv.stopGrace) open.add('runtime')
  if (adv.memory || adv.cpus || adv.shm || adv.devices.length || adv.gpu) open.add('resources')
  if (adv.health !== 'image') open.add('health')
  if (adv.capAdd.length || adv.capDrop.length || adv.privileged || adv.noNewPrivileges) open.add('security')
  if (adv.logLimit) open.add('logging')
  if (has(s, 'env_file')) open.add('envfile')

  // Examples from the docs: replaced with what the user entered, else flagged.
  const placeholders: Placeholder[] = []
  const check = (ph: Placeholder, set: (v: string) => void) => {
    placeholders.push(ph)
    const r = edits[ph.id]?.trim()
    if (r) set(r)
    else warn(`${ph.label}: ${code(ph.value)} looks like an example from the docs. Replace it with your own ${ph.kind === 'volume' ? 'folder' : 'value'}.`)
  }
  vols.forEach((v, i) => {
    if (looksLikePlaceholder(v.source))
      check({ id: `volume:${i}`, kind: 'volume', label: `Volume for ${v.target.split(':')[0]}`, value: v.source, suggestion: suggestSource(v.target), target: v.target }, (r) => (v.source = r))
  })
  for (const e of env)
    if (looksLikePlaceholder(e.value)) check({ id: `env:${e.key}`, kind: 'env', label: e.key, value: e.value, secret: isSecretKey(e.key) }, (r) => (e.value = r))
  for (const l of adv.labels) if (looksLikePlaceholder(l.value)) check({ id: `label:${l.key}`, kind: 'label', label: `Label ${l.key}`, value: l.value }, (r) => (l.value = r))

  return { service, image, tag, name, restart, ports, env, vols, adv, notes, open: [...open], placeholders, changes }
}

// ---------------- whole file as a stack ----------------

/** A .env value that dotenv and compose read back unchanged. */
function envValue(v: string) {
  const one = v.replace(/[\r\n]+/g, ' ')
  if (/^[A-Za-z0-9_@%+=:,./-]*$/.test(one)) return one
  if (!one.includes("'")) return `'${one}'`
  return `"${one.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\$/g, '\\$')}"`
}

/** What the new stack editor is filled with: the file as pasted, a name, and a .env with the values typed in. */
export function stackDraft(text: string, p: Extract<Parsed, { ok: true }>, values: Record<string, string>) {
  const name = cleanName(p.project) || cleanName(p.services[0]) || 'app'
  const vars = variablesOf(p)
  const lines: string[] = []
  for (const v of vars) {
    const val = values[v.name] ?? ''
    if (val !== '') lines.push(`${v.name}=${envValue(val)}`)
    else if (v.def === undefined) lines.push(`# ${v.name}=   (no value yet; the file needs one)`)
  }
  return { name, compose: text.endsWith('\n') ? text : text + '\n', env: lines.length ? lines.join('\n') + '\n' : '' }
}
