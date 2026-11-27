// Builds and checks the one-service compose file behind "New container".
// Pure and dependency-free so scripts/compose.test.mjs can run it under Node.

export type Port = { host: string; container: string; proto: 'tcp' | 'udp' }
export type Env = { key: string; value: string }
export type Vol = { source: string; target: string }
export type KV = { key: string; value: string }

export type NetworkMode = '' | 'host' | 'none'
export type HealthMode = 'image' | 'custom' | 'disable'

export const CAPS = ['NET_ADMIN', 'SYS_ADMIN', 'NET_RAW', 'SYS_PTRACE', 'SYS_TIME', 'MKNOD', 'CHOWN', 'DAC_OVERRIDE', 'SETUID', 'SETGID'] as const
export const DROP_CAPS = ['ALL', ...CAPS] as const

export type Advanced = {
  // networking
  networkMode: NetworkMode
  networks: string[] // existing networks on the host (external)
  newNetwork: string // a network this stack creates
  hostname: string
  extraHosts: KV[] // key = host name, value = IP
  dns: string // space or comma separated
  // labels
  labels: KV[]
  // command & runtime
  command: string
  entrypoint: string
  workingDir: string
  user: string
  init: boolean
  readOnly: boolean
  stopGrace: string
  tz: string
  // resources
  memory: string
  memoryUnit: 'm' | 'g'
  cpus: string
  shm: string
  shmUnit: 'm' | 'g'
  devices: string[]
  gpu: boolean
  // health check
  health: HealthMode
  hcTest: string
  hcInterval: string
  hcTimeout: string
  hcRetries: string
  hcStart: string
  // security
  capAdd: string[]
  capDrop: string[]
  privileged: boolean
  noNewPrivileges: boolean
  // logging
  logLimit: boolean
  logMaxSize: string
  logMaxFile: string
  // .env
  envFile: string
}

export const blankAdvanced = (): Advanced => ({
  networkMode: '',
  networks: [],
  newNetwork: '',
  hostname: '',
  extraHosts: [],
  dns: '',
  labels: [],
  command: '',
  entrypoint: '',
  workingDir: '',
  user: '',
  init: false,
  readOnly: false,
  stopGrace: '',
  tz: '',
  memory: '',
  memoryUnit: 'm',
  cpus: '',
  shm: '',
  shmUnit: 'm',
  devices: [],
  gpu: false,
  health: 'image',
  hcTest: '',
  hcInterval: '',
  hcTimeout: '',
  hcRetries: '',
  hcStart: '',
  capAdd: [],
  capDrop: [],
  privileged: false,
  noNewPrivileges: false,
  logLimit: false,
  logMaxSize: '10m',
  logMaxFile: '3',
  envFile: '',
})

export type Spec = {
  name: string
  image: string
  restart: string
  ports: Port[]
  env: Env[]
  vols: Vol[]
  adv?: Advanced
  /** Volumes that already exist on the host (network shares): used as external, not created per stack. */
  externalVols?: string[]
}

/**
 * Quotes any user value as a YAML double-quoted scalar. JSON escapes quotes,
 * backslashes, newlines and control characters, so a value can't end the
 * scalar or start a new key, and '#', ':' or a leading '-', '*', '&', '!'
 * are just text inside the quotes. On top of JSON, characters YAML treats
 * as line breaks or non-printable (NEL, U+2028/9, DEL, C1, BOM) are
 * \u-escaped, and lone surrogates (which YAML parsers reject) are dropped.
 */
export const q = (s: string) =>
  JSON.stringify(s.replace(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/g, '')).replace(
    /[\u007f-\u009f\u2028\u2029\ufeff\ufffe\uffff]/g,
    (c) => '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0'),
  )
/** Quotes a value compose would otherwise interpolate: "$" becomes "$$". */
export const qv = (s: string) => q(s.replaceAll('$', '$$$$'))

export const isNamedVolume = (src: string) => /^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(src)
const BUILTIN_NETWORKS = new Set(['bridge', 'host', 'none'])
export const isUserNetwork = (name: string) => !BUILTIN_NETWORKS.has(name)

/**
 * Splits a shell-like command line into arguments: whitespace separates,
 * '...' is literal, "..." allows \" \\ \$ \` escapes, and a backslash
 * outside quotes escapes the next character. No globbing or expansion.
 */
export function splitArgs(line: string): { args: string[]; error?: string } {
  const args: string[] = []
  let cur = ''
  let has = false
  let i = 0
  while (i < line.length) {
    const c = line[i]
    if (c === "'") {
      const end = line.indexOf("'", i + 1)
      if (end < 0) return { args, error: 'A single quote (\') is never closed.' }
      cur += line.slice(i + 1, end)
      has = true
      i = end + 1
    } else if (c === '"') {
      i++
      let closed = false
      while (i < line.length) {
        const d = line[i]
        if (d === '"') {
          closed = true
          i++
          break
        }
        if (d === '\\' && i + 1 < line.length && '"\\$`'.includes(line[i + 1])) {
          cur += line[i + 1]
          i += 2
          continue
        }
        cur += d
        i++
      }
      if (!closed) return { args, error: 'A double quote (") is never closed.' }
      has = true
    } else if (c === '\\') {
      if (i + 1 < line.length) cur += line[i + 1]
      has = true
      i += 2
    } else if (/\s/.test(c)) {
      if (has) args.push(cur)
      cur = ''
      has = false
      i++
    } else {
      cur += c
      has = true
      i++
    }
  }
  if (has) args.push(cur)
  return { args }
}

export const splitList = (s: string) => s.split(/[\s,]+/).filter(Boolean)

/** The KEY=VALUE lines of a .env text; blank lines and # comments are skipped. */
export function envFileProblems(text: string): string[] {
  const out: string[] = []
  text.split(/\r?\n/).forEach((line, i) => {
    const t = line.trim()
    if (!t || t.startsWith('#')) return
    if (!/^(export\s+)?[A-Za-z_][A-Za-z0-9_]*=/.test(t)) out.push(`.env line ${i + 1}: use NAME=value (names use letters, digits and _).`)
  })
  return out
}
const envFileHasVars = (text: string) => text.split(/\r?\n/).some((l) => /^\s*(export\s+)?[A-Za-z_][A-Za-z0-9_]*=/.test(l))

const block = (lines: string[], key: string, items: string[], indent = '    ') => {
  if (!items.length) return
  lines.push(`${indent}${key}:`)
  for (const it of items) lines.push(`${indent}  - ${it}`)
}

/** The environment as composeFor writes it, with TZ added from the time zone shortcut when not set already. */
export function effectiveEnv(env: Env[], adv?: Advanced): Env[] {
  const list = env.filter((e) => e.key)
  if (adv?.tz.trim() && !list.some((e) => e.key === 'TZ')) list.push({ key: 'TZ', value: adv.tz.trim() })
  return list
}

/** Builds a one-service compose file. Every user value is quoted, and "$" is escaped so compose doesn't interpolate it. */
export function composeFor(o: Spec) {
  const a = o.adv ?? blankAdvanced()
  const lines = ['services:', `  ${o.name}:`, `    image: ${q(o.image)}`, `    container_name: ${q(o.name)}`, `    restart: ${q(o.restart)}`]
  const ownNet = a.networkMode === ''
  if (!ownNet) lines.push(`    network_mode: ${q(a.networkMode)}`)

  // command & runtime
  const cmd = a.command.trim() ? splitArgs(a.command) : null
  if (cmd && !cmd.error) block(lines, 'command', cmd.args.map(qv))
  const ep = a.entrypoint.trim() ? splitArgs(a.entrypoint) : null
  if (ep && !ep.error) block(lines, 'entrypoint', ep.args.map(qv))
  if (a.workingDir.trim()) lines.push(`    working_dir: ${qv(a.workingDir.trim())}`)
  if (a.user.trim()) lines.push(`    user: ${qv(a.user.trim())}`)
  if (a.init) lines.push('    init: true')
  if (a.readOnly) lines.push('    read_only: true')
  if (a.stopGrace.trim()) lines.push(`    stop_grace_period: ${q(a.stopGrace.trim())}`)

  // networking
  if (ownNet && a.hostname.trim()) lines.push(`    hostname: ${qv(a.hostname.trim())}`)
  const hosts = a.extraHosts.filter((h) => h.key.trim() || h.value.trim())
  block(lines, 'extra_hosts', hosts.map((h) => q(`${h.key.trim()}:${h.value.trim()}`)))
  block(lines, 'dns', splitList(a.dns).map(q))
  const nets = ownNet ? [...new Set([...a.networks, ...(a.newNetwork.trim() ? [a.newNetwork.trim()] : [])])] : []
  block(lines, 'networks', nets.map(q))

  const ports = ownNet ? o.ports.filter((p) => p.container) : []
  block(lines, 'ports', ports.map((p) => q(`${p.host ? p.host + ':' : ''}${p.container}/${p.proto}`)))

  const env = effectiveEnv(o.env, a)
  const useEnvFile = envFileHasVars(a.envFile)
  if (useEnvFile) {
    // required: false: on a new stack the hub checks the compose file before .env is in place.
    lines.push('    env_file:', '      - path: ".env"', '        required: false')
  }
  if (env.length) {
    lines.push('    environment:')
    for (const e of env) lines.push(`      ${q(e.key)}: ${qv(e.value)}`)
  }
  const vols = o.vols.filter((v) => v.source && v.target)
  block(lines, 'volumes', vols.map((v) => qv(`${v.source}:${v.target}`)))

  // labels
  const labels = a.labels.filter((l) => l.key.trim())
  if (labels.length) {
    lines.push('    labels:')
    for (const l of labels) lines.push(`      ${q(l.key.trim())}: ${qv(l.value)}`)
  }

  // resources
  if (a.memory.trim()) lines.push(`    mem_limit: ${q(a.memory.trim() + a.memoryUnit)}`)
  if (a.cpus.trim()) lines.push(`    cpus: ${q(a.cpus.trim())}`)
  if (a.shm.trim()) lines.push(`    shm_size: ${q(a.shm.trim() + a.shmUnit)}`)
  block(lines, 'devices', a.devices.map((d) => d.trim()).filter(Boolean).map(q))
  if (a.gpu) {
    lines.push(
      '    deploy:',
      '      resources:',
      '        reservations:',
      '          devices:',
      '            - driver: nvidia',
      '              count: all',
      '              capabilities: [gpu]',
    )
  }

  // health check
  if (a.health === 'disable') lines.push('    healthcheck:', '      disable: true')
  else if (a.health === 'custom' && a.hcTest.trim()) {
    lines.push('    healthcheck:', `      test: ["CMD-SHELL", ${qv(a.hcTest.trim())}]`)
    if (a.hcInterval.trim()) lines.push(`      interval: ${q(a.hcInterval.trim())}`)
    if (a.hcTimeout.trim()) lines.push(`      timeout: ${q(a.hcTimeout.trim())}`)
    if (a.hcRetries.trim()) lines.push(`      retries: ${Number(a.hcRetries.trim())}`)
    if (a.hcStart.trim()) lines.push(`      start_period: ${q(a.hcStart.trim())}`)
  }

  // security
  if (a.privileged) lines.push('    privileged: true')
  block(lines, 'cap_add', a.capAdd.map(q))
  block(lines, 'cap_drop', a.capDrop.map(q))
  if (a.noNewPrivileges) block(lines, 'security_opt', [q('no-new-privileges:true')])

  // logging
  if (a.logLimit) {
    lines.push('    logging:', '      driver: json-file', '      options:')
    lines.push(`        max-size: ${q(a.logMaxSize.trim())}`, `        max-file: ${q(a.logMaxFile.trim())}`)
  }

  // top-level
  if (nets.length) {
    lines.push('networks:')
    for (const n of nets) {
      if (a.networks.includes(n)) lines.push(`  ${q(n)}:`, '    external: true')
      else lines.push(`  ${q(n)}:`, `    name: ${q(n)}`)
    }
  }
  const named = [...new Set(vols.map((v) => v.source).filter(isNamedVolume))]
  if (named.length) {
    lines.push('volumes:')
    for (const n of named) {
      if (o.externalVols?.includes(n)) lines.push(`  ${q(n)}:`, '    external: true')
      else lines.push(`  ${q(n)}: {}`)
    }
  }
  return lines.join('\n') + '\n'
}

// ---------------- validation ----------------

export type Section = 'basic' | 'network' | 'labels' | 'runtime' | 'resources' | 'health' | 'security' | 'logging' | 'envfile'
export type Problem = { section: Section; field: string; msg: string }

const DURATION = /^(\d+(\.\d+)?(ns|us|ms|s|m|h))+$/
const portOK = (s: string) => /^\d{1,5}$/.test(s) && +s >= 1 && +s <= 65535
const positive = (s: string) => /^\d+(\.\d+)?$/.test(s) && +s > 0
const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/
const IPV6 = /^[0-9a-fA-F:]*:[0-9a-fA-F:.]*$/
const isIP = (s: string) => IPV4.test(s) || IPV6.test(s)
const HOSTNAME = /^(?=.{1,253}$)[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$/
export const NETWORK_NAME = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$/
export const LABEL_KEY = /^[A-Za-z0-9]([A-Za-z0-9._\/-]*[A-Za-z0-9])?$/

/** Every problem with the form, per field. existingNetworks lets it flag a new network that already exists. */
export function allProblems(o: Spec, existingNetworks: string[] = []): Problem[] {
  const a = o.adv ?? blankAdvanced()
  const out: Problem[] = []
  const add = (section: Section, field: string, msg: string) => out.push({ section, field, msg })

  if (!o.image) add('basic', 'image', 'Pick an image.')
  if (/\s/.test(o.image)) add('basic', 'image', 'The image name has a space in it.')
  if (!/^[a-z0-9][a-z0-9_-]{0,62}$/.test(o.name)) add('basic', 'name', 'Name: lowercase letters, digits, - and _, starting with a letter or digit.')
  if (a.networkMode === '') {
    for (const p of o.ports) {
      if (!p.container && !p.host) continue
      if (!portOK(p.container) || (p.host && !portOK(p.host))) {
        add('basic', 'ports', 'Ports must be numbers from 1 to 65535.')
        break
      }
    }
  }
  for (const e of o.env) if (e.key && !/^[A-Za-z_][A-Za-z0-9_]*$/.test(e.key)) add('basic', 'env', `Environment variable names use letters, digits and _ (not "${e.key}").`)
  for (const v of o.vols) {
    if (!v.source && !v.target) continue
    if (!v.source || !v.target.startsWith('/')) add('basic', 'vols', 'Volumes need a source and an absolute path inside the container, like /data.')
    else if (!isNamedVolume(v.source) && !/^(\/|\.\/|\.\.\/)/.test(v.source)) add('basic', 'vols', `Volume source "${v.source}" should be a volume name or a path starting with / or ./`)
  }

  // networking
  const ownNet = a.networkMode === ''
  if (!ownNet && (a.networks.length || a.newNetwork.trim())) add('network', 'networks', `Networks can't be joined with "${a.networkMode}" network mode.`)
  if (a.networkMode === 'host' && a.hostname.trim()) add('network', 'hostname', 'A container on the host network uses the host\'s name; leave Hostname empty.')
  if (a.hostname.trim() && !HOSTNAME.test(a.hostname.trim())) add('network', 'hostname', 'Hostname: letters, digits, - and dots, like app or app.lan.')
  const nn = a.newNetwork.trim()
  if (nn) {
    if (!NETWORK_NAME.test(nn)) add('network', 'newNetwork', 'New network name: letters, digits, _ . -, starting with a letter or digit.')
    else if (!isUserNetwork(nn)) add('network', 'newNetwork', `"${nn}" is a built-in network; pick it as the network mode instead.`)
    else if (existingNetworks.includes(nn)) add('network', 'newNetwork', `A network called "${nn}" already exists on this host: attach to it instead.`)
  }
  for (const h of a.extraHosts) {
    if (!h.key.trim() && !h.value.trim()) continue
    if (!HOSTNAME.test(h.key.trim())) add('network', 'extraHosts', `Extra host "${h.key}" isn't a valid host name.`)
    else if (!isIP(h.value.trim()) && h.value.trim() !== 'host-gateway') add('network', 'extraHosts', `Extra host ${h.key}: "${h.value}" isn't an IP address (or host-gateway).`)
  }
  for (const d of splitList(a.dns)) if (!isIP(d)) add('network', 'dns', `DNS server "${d}" isn't an IP address.`)

  // labels
  const seen = new Set<string>()
  for (const l of a.labels) {
    const k = l.key.trim()
    if (!k) {
      if (l.value) add('labels', 'labels', 'A label has a value but no key.')
      continue
    }
    if (!LABEL_KEY.test(k)) add('labels', 'labels', `Label key "${k}": letters, digits, . _ / -, starting and ending with a letter or digit.`)
    if (seen.has(k)) add('labels', 'labels', `Label "${k}" is set twice.`)
    seen.add(k)
  }

  // command & runtime
  if (a.command.trim()) {
    const r = splitArgs(a.command)
    if (r.error) add('runtime', 'command', `Command: ${r.error}`)
  }
  if (a.entrypoint.trim()) {
    const r = splitArgs(a.entrypoint)
    if (r.error) add('runtime', 'entrypoint', `Entrypoint: ${r.error}`)
  }
  if (a.workingDir.trim() && !a.workingDir.trim().startsWith('/')) add('runtime', 'workingDir', 'Working directory must be an absolute path, like /app.')
  if (a.user.trim() && !/^[A-Za-z0-9_][A-Za-z0-9_.-]*(:[A-Za-z0-9_][A-Za-z0-9_.-]*)?$/.test(a.user.trim())) add('runtime', 'user', 'User: a name or ID, optionally with a group, like 1000:1000.')
  if (a.stopGrace.trim() && !DURATION.test(a.stopGrace.trim())) add('runtime', 'stopGrace', 'Stop grace period: a duration like 10s or 1m30s.')
  if (a.tz.trim() && !/^[A-Za-z0-9_+\-]+(\/[A-Za-z0-9_+\-]+)*$/.test(a.tz.trim())) add('runtime', 'tz', 'Time zone: a name like Europe/London or America/New_York.')

  // resources
  if (a.memory.trim()) {
    if (!positive(a.memory.trim())) add('resources', 'memory', 'Memory limit: a number greater than 0, like 512 (MB) or 1.5 (GB).')
    else if ((a.memoryUnit === 'g' ? +a.memory * 1024 : +a.memory) < 6) add('resources', 'memory', 'Memory limit: Docker needs at least 6 MB.')
  }
  if (a.cpus.trim() && !positive(a.cpus.trim())) add('resources', 'cpus', 'CPU limit: a number of cores greater than 0, like 0.5 or 2.')
  if (a.shm.trim() && !positive(a.shm.trim())) add('resources', 'shm', 'Shared memory: a number greater than 0.')
  for (const d of a.devices) {
    const t = d.trim()
    if (!t) continue
    const [host, ctr, perms, ...rest] = t.split(':')
    if (!/^\/dev\/\S+$/.test(host)) add('resources', 'devices', `Device "${t}": the host path starts with /dev/, like /dev/dri.`)
    else if (rest.length || (ctr !== undefined && !/^\/\S*$/.test(ctr) && !/^[rwm]{1,3}$/.test(ctr))) add('resources', 'devices', `Device "${t}": use /dev/host[:/dev/container][:rwm].`)
    else if (perms !== undefined && !/^[rwm]{1,3}$/.test(perms)) add('resources', 'devices', `Device "${t}": permissions are some of r, w and m.`)
  }

  // health check
  if (a.health === 'custom') {
    if (!a.hcTest.trim()) add('health', 'hcTest', 'Health check: enter a command, like curl -f http://localhost/ || exit 1.')
    for (const [f, label] of [['hcInterval', 'Interval'], ['hcTimeout', 'Timeout'], ['hcStart', 'Start period']] as const) {
      const v = a[f].trim()
      if (v && !DURATION.test(v)) add('health', f, `${label}: a duration like 30s or 1m.`)
    }
    if (a.hcRetries.trim() && !(/^\d+$/.test(a.hcRetries.trim()) && +a.hcRetries >= 1)) add('health', 'hcRetries', 'Retries: a whole number, 1 or more.')
  }

  // security
  const both = a.capAdd.filter((c) => a.capDrop.includes(c))
  if (both.length) add('security', 'caps', `${both.join(', ')} is both added and dropped.`)

  // logging
  if (a.logLimit) {
    if (!/^\d+[kmg]$/.test(a.logMaxSize.trim())) add('logging', 'logMaxSize', 'Max size: a number and k, m or g, like 10m.')
    if (!(/^\d+$/.test(a.logMaxFile.trim()) && +a.logMaxFile >= 1)) add('logging', 'logMaxFile', 'Files to keep: a whole number, 1 or more.')
  }

  for (const m of envFileProblems(a.envFile)) add('envfile', 'envFile', m)
  return out
}

/** The first problem, or null. */
export function problems(o: Spec, existingNetworks: string[] = []): string | null {
  return allProblems(o, existingNetworks)[0]?.msg ?? null
}

/** How many options are set in each advanced section, shown when a section is collapsed. */
export function setCounts(a: Advanced): Record<Exclude<Section, 'basic'>, number> {
  const n = (...xs: unknown[]) => xs.filter((x) => (Array.isArray(x) ? x.length > 0 : typeof x === 'string' ? x.trim() !== '' : !!x)).length
  return {
    network: n(a.networkMode, a.networks, a.newNetwork, a.hostname, a.extraHosts.filter((h) => h.key.trim() || h.value.trim()), a.dns),
    labels: a.labels.filter((l) => l.key.trim()).length,
    runtime: n(a.command, a.entrypoint, a.workingDir, a.user, a.init, a.readOnly, a.stopGrace, a.tz),
    resources: n(a.memory, a.cpus, a.shm, a.devices.filter((d) => d.trim()), a.gpu),
    health: a.health === 'image' ? 0 : 1,
    security: a.capAdd.length + a.capDrop.length + n(a.privileged, a.noNewPrivileges),
    logging: a.logLimit ? 1 : 0,
    envfile: envFileHasVars(a.envFile) ? 1 : 0,
  }
}
