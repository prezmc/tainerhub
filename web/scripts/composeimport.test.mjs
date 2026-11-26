// Tests for src/composeimport.ts (paste a compose file into New container). Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { importService, interpolate, joinArgs, parseCompose, parseSize, splitImage, stackDraft, variablesOf } from '../src/composeimport.ts'
import { allProblems, composeFor, splitArgs } from '../src/compose.ts'

const ok = (text) => {
  const p = parseCompose(text)
  assert.equal(p.ok, true, p.error)
  return p
}
const one = (text, values) => {
  const p = ok(text)
  return importService(p, p.services[0], values)
}
const warns = (r) => r.notes.filter((n) => n.level === 'warn').map((n) => n.msg)
const spec = (r) => ({ name: r.name, image: r.image.includes('@') ? r.image : `${r.image}:${r.tag}`, restart: r.restart, ports: r.ports, env: r.env, vols: r.vols, adv: r.adv })

test('a typical README snippet fills the basic fields', () => {
  const r = one(`
services:
  uptime-kuma:
    image: louislam/uptime-kuma:2
    container_name: uptime-kuma
    volumes:
      - ./data:/app/data
      - /var/run/docker.sock:/var/run/docker.sock:ro
    ports:
      - "3001:3001"
    restart: always
`)
  assert.equal(r.image, 'louislam/uptime-kuma')
  assert.equal(r.tag, '2')
  assert.equal(r.name, 'uptime-kuma')
  assert.equal(r.restart, 'always')
  assert.deepEqual(r.ports, [{ host: '3001', container: '3001', proto: 'tcp' }])
  assert.deepEqual(r.vols, [
    { source: './data', target: '/app/data' },
    { source: '/var/run/docker.sock', target: '/var/run/docker.sock:ro' },
  ])
  assert.deepEqual(warns(r), [])
  assert.deepEqual(allProblems(spec(r)), [])
  // and the form writes the read-only flag back out
  assert.match(composeFor(spec(r)), /- "\/var\/run\/docker.sock:\/var\/run\/docker.sock:ro"/)
})

test('home assistant: host network, privileged, env map, no ports', () => {
  const r = one(`
services:
  homeassistant:
    container_name: homeassistant
    image: "ghcr.io/home-assistant/home-assistant:stable"
    volumes:
      - /PATH_TO_YOUR_CONFIG:/config
      - /etc/localtime:/etc/localtime:ro
      - /run/dbus:/run/dbus:ro
    restart: unless-stopped
    privileged: true
    network_mode: host
    environment:
      TZ: Europe/London
`)
  assert.equal(r.image, 'ghcr.io/home-assistant/home-assistant')
  assert.equal(r.tag, 'stable')
  assert.equal(r.adv.networkMode, 'host')
  assert.equal(r.adv.privileged, true)
  assert.deepEqual(r.env, [{ key: 'TZ', value: 'Europe/London' }])
  assert.ok(r.open.includes('network') && r.open.includes('security'))
  assert.ok(warns(r).some((w) => w.includes('privileged')))
})

test('jellyfin: user, group_add and devices; unknown keys are reported, not dropped', () => {
  const r = one(`
services:
  jellyfin:
    image: jellyfin/jellyfin
    user: 1000:1000
    group_add:
      - "122"
    network_mode: 'host'
    volumes:
      - /path/to/config:/config
      - type: bind
        source: /path/to/media
        target: /media
        read_only: true
    devices:
      - /dev/dri/renderD128:/dev/dri/renderD128
    restart: 'unless-stopped'
    extra_hosts:
      - 'host.docker.internal:host-gateway'
`)
  assert.equal(r.tag, 'latest')
  assert.equal(r.adv.user, '1000:1000')
  assert.deepEqual(r.adv.devices, ['/dev/dri/renderD128:/dev/dri/renderD128'])
  assert.deepEqual(r.vols[1], { source: '/path/to/media', target: '/media:ro' })
  assert.deepEqual(r.adv.extraHosts, [{ key: 'host.docker.internal', value: 'host-gateway' }])
  assert.ok(warns(r).some((w) => w.startsWith('group_add:')))
})

test('variables: defaults are used, typed values win, missing ones are reported', () => {
  const text = `
services:
  app:
    image: nginx:\${TAG:-1.27}
    environment:
      - TZ=\${TZ:-UTC}
      - PUID=\${PUID}
      - PRICE=$$5
      - LITERAL=\${NOT CLOSED
    volumes:
      - \${DATA_DIR:-./data}:/data
`
  const p = ok(text)
  const vars = variablesOf(p, 'app')
  assert.deepEqual(
    vars.map((v) => [v.name, v.def]),
    [
      ['TAG', '1.27'],
      ['TZ', 'UTC'],
      ['PUID', undefined],
      ['DATA_DIR', './data'],
    ],
  )
  const r = importService(p, 'app', { TZ: '', TAG: '1.29' })
  assert.equal(r.tag, '1.29')
  assert.deepEqual(r.env, [
    { key: 'TZ', value: 'UTC' },
    { key: 'PUID', value: '' },
    { key: 'PRICE', value: '$5' },
    { key: 'LITERAL', value: '${NOT CLOSED' },
  ])
  assert.deepEqual(r.vols, [{ source: './data', target: '/data' }])
  assert.ok(warns(r).some((w) => w.includes('${PUID} has no value')))
  // a literal $ goes back out escaped, so compose doesn't expand it
  assert.match(composeFor(spec(r)), /"PRICE": "\$\$5"/)
})

test('interpolate follows compose rules', () => {
  const run = (s, values = {}) => {
    const missing = new Set()
    return [interpolate(s, values, new Map(), missing), [...missing]]
  }
  assert.deepEqual(run('${A:-x}', { A: '' }), ['x', []])
  assert.deepEqual(run('${A-x}', {}), ['x', []])
  assert.deepEqual(run('${A:+set}', { A: '1' }), ['set', []])
  assert.deepEqual(run('${A:+set}', {}), ['', []])
  assert.deepEqual(run('${A:?need A}', {}), ['', ['A']])
  assert.deepEqual(run('$A/$B', { A: 'a' }), ['a/', ['B']])
  assert.deepEqual(run('${A:-${B:-deep}}', {}), ['deep', []])
  assert.deepEqual(run('$$HOME and $', {}), ['$HOME and $', []])
  assert.deepEqual(run('${1BAD}', {}), ['${1BAD}', []])
})

test('several services: each can be picked, the rest are named in a warning', () => {
  const p = ok(`
name: Photo Stack
services:
  web:
    image: example/web:1
    depends_on: [db]
    ports: ["8080:80"]
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: \${DB_PASSWORD}
volumes:
  pg: {}
`)
  assert.deepEqual(p.services, ['web', 'db'])
  const web = importService(p, 'web')
  assert.ok(warns(web).some((w) => w.includes('db was left out')))
  assert.ok(warns(web).some((w) => w.startsWith('depends_on (db)')))
  const db = importService(p, 'db')
  assert.equal(db.image, 'postgres')
  assert.equal(db.tag, '16')

  const d = stackDraft('services: {}', p, { DB_PASSWORD: "it's a secret" })
  assert.equal(d.name, 'photo-stack')
  assert.equal(d.compose, 'services: {}\n')
  assert.equal(d.env, `DB_PASSWORD="it's a secret"\n`)
  assert.match(stackDraft('x', p, {}).env, /^# DB_PASSWORD=/)
})

test('a bare service or a services map without services: still imports', () => {
  const a = one('image: lscr.io/linuxserver/plex:latest\nports: ["32400:32400"]\n')
  assert.equal(a.name, 'plex')
  assert.equal(a.image, 'lscr.io/linuxserver/plex')
  const b = one('whoami:\n  image: traefik/whoami\n')
  assert.equal(b.service, 'whoami')
})

test('ports: ranges, udp, long syntax, and local-only binds', () => {
  const r = one(`
services:
  a:
    image: x
    ports:
      - 53:53/udp
      - "127.0.0.1:9000:9000"
      - "[::1]:9001:9001"
      - "0.0.0.0:8443:443"
      - "7000-7002:7000-7002"
      - "6000-6005:80"
      - 9999
      - target: 80
        published: "8081"
        protocol: tcp
      - "5000:5000/sctp"
`)
  assert.deepEqual(r.ports, [
    { host: '53', container: '53', proto: 'udp' },
    { host: '8443', container: '443', proto: 'tcp' },
    { host: '7000', container: '7000', proto: 'tcp' },
    { host: '7001', container: '7001', proto: 'tcp' },
    { host: '7002', container: '7002', proto: 'tcp' },
    { host: '', container: '9999', proto: 'tcp' },
    { host: '8081', container: '80', proto: 'tcp' },
  ])
  const w = warns(r)
  assert.ok(w.some((m) => m.includes('127.0.0.1 only')))
  assert.ok(w.some((m) => m.includes('[::1] only')))
  assert.ok(w.some((m) => m.includes('different number')))
  assert.ok(w.some((m) => m.includes('sctp')))
})

test('advanced options map onto the form', () => {
  const r = one(`
services:
  app:
    image: example/app@sha256:${'a'.repeat(64)}
    container_name: My_App
    restart: on-failure:5
    hostname: app
    dns: 1.1.1.1
    networks:
      - proxy
      - backend
      - default
    labels:
      traefik.enable: "true"
      com.example.empty:
    command: ["sh", "-c", "echo 'hi there' && sleep 1"]
    entrypoint: /init
    working_dir: /app
    init: true
    read_only: true
    stop_grace_period: 1m
    mem_limit: 512M
    cpus: 1.5
    shm_size: 268435456
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: 1
              capabilities: [gpu]
      replicas: 2
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/"]
      interval: 30s
      retries: 3
      start_interval: 1s
    cap_add: [CAP_NET_ADMIN, SYS_MODULE]
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true, apparmor:unconfined]
    logging:
      driver: json-file
      options:
        max-size: 10M
    env_file: .env
    tty: true
networks:
  proxy:
    external: true
  backend:
    driver: bridge
`)
  const a = r.adv
  assert.equal(r.image, `example/app@sha256:${'a'.repeat(64)}`)
  assert.equal(r.name, 'my_app')
  assert.equal(r.restart, 'on-failure')
  assert.deepEqual(a.networks, ['proxy'])
  assert.equal(a.newNetwork, 'backend')
  assert.equal(a.dns, '1.1.1.1')
  assert.deepEqual(a.labels, [
    { key: 'traefik.enable', value: 'true' },
    { key: 'com.example.empty', value: '' },
  ])
  assert.deepEqual(splitArgs(a.command).args, ['sh', '-c', "echo 'hi there' && sleep 1"])
  assert.equal(a.entrypoint, '/init')
  assert.equal(a.init && a.readOnly, true)
  assert.deepEqual([a.memory, a.memoryUnit, a.cpus, a.shm, a.shmUnit], ['512', 'm', '1.5', '256', 'm'])
  assert.equal(a.gpu, true)
  assert.equal(a.health, 'custom')
  assert.equal(a.hcTest, 'curl -f http://localhost/')
  assert.deepEqual([a.hcInterval, a.hcRetries], ['30s', '3'])
  assert.deepEqual(a.capAdd, ['NET_ADMIN'])
  assert.deepEqual(a.capDrop, ['ALL'])
  assert.equal(a.noNewPrivileges, true)
  assert.deepEqual([a.logLimit, a.logMaxSize, a.logMaxFile], [true, '10m', '3'])
  assert.deepEqual(r.open.sort(), ['envfile', 'health', 'labels', 'logging', 'network', 'resources', 'runtime', 'security'])
  const w = warns(r).join('\n')
  for (const s of ['particular GPUs', 'deploy.replicas', 'start_interval', 'SYS_MODULE', 'apparmor', 'env_file', 'tty:'])
    assert.ok(w.includes(s), `warns about ${s}`)
  assert.deepEqual(allProblems(spec(r)), [])
  const ch = Object.fromEntries(r.changes.map((c) => [c.id, c]))
  assert.deepEqual([ch.restart.from, ch.restart.to], ['on-failure:5', 'on-failure'])
  assert.deepEqual([ch.name.from, ch.name.to], ['My_App', 'my_app'])
  assert.equal(ch.health.to, 'curl -f http://localhost/')
  assert.equal(ch['log-files'].to, '3')
  assert.equal(ch.memory, undefined) // 512M needs no conversion
})

test('changes TainerHub makes are listed and can be adjusted', () => {
  const p = parseCompose(`
services:
  app:
    image: example/app
    environment:
      TZ: \${TZ:-UTC}
    mem_limit: 524288k
    network_mode: bridge
    runtime: nvidia
`)
  const r = importService(p, 'app')
  assert.deepEqual(
    r.changes.map((c) => [c.id, c.from, c.to, c.input]),
    [
      ['var:TZ', '', 'UTC', 'variable'],
      ['tag', '', 'latest', 'text'],
      ['restart', '', 'unless-stopped', 'restart'],
      ['network-mode', 'bridge', 'the stack’s own network', undefined],
      ['memory', '524288k', '512 MB', undefined],
      ['gpu', 'runtime: nvidia', 'all NVIDIA GPUs (a GPU reservation)', undefined],
    ],
  )
  // Edits win; an unknown restart policy falls back; a set variable isn't a change.
  const e = importService(p, 'app', { TZ: 'Europe/London' }, { 'change:tag': ' 1.4 ', 'change:restart': 'bogus' })
  assert.equal(e.tag, '1.4')
  assert.equal(e.changes.find((c) => c.id === 'tag').to, '1.4') // what the form gets, for the notice
  assert.equal(e.restart, 'unless-stopped')
  assert.equal(e.changes.find((c) => c.id === 'restart').to, 'unless-stopped')
  assert.equal(e.env[0].value, 'Europe/London')
  assert.equal(e.changes.find((c) => c.id === 'var:TZ').to, 'Europe/London')
  assert.equal(importService(p, 'app', {}, { 'change:restart': 'always' }).restart, 'always')
})

test('build-only services and other things the form can’t do are reported', () => {
  const r = one('services:\n  app:\n    build: .\n    volumes: [/data]\n    x-note: hi\n')
  assert.equal(r.image, '')
  const w = warns(r)
  assert.ok(w.some((m) => m.includes('built from source')))
  assert.ok(w.some((m) => m.includes('has no source')))
  assert.deepEqual(r.vols, [{ source: '', target: '/data' }]) // the form then asks for a source
  assert.ok(r.notes.some((n) => n.msg.startsWith('x-note')))
})

test('external volumes and env without values warn', () => {
  const r = one('services:\n  a:\n    image: x\n    environment: [HOME_URL, "A=1"]\n    volumes: [media:/media]\nvolumes:\n  media:\n    external: true\n')
  assert.deepEqual(r.env, [
    { key: 'HOME_URL', value: '' },
    { key: 'A', value: '1' },
  ])
  const w = warns(r)
  assert.ok(w.some((m) => m.startsWith('HOME_URL takes its value from the shell')))
  assert.ok(w.some((m) => m.includes('media is external')))
})

test('bad or hostile input is refused cleanly', () => {
  assert.equal(parseCompose('').ok, false)
  assert.match(parseCompose('docker run -d -p 80:80 nginx').error, /docker run/)
  assert.match(parseCompose('services:\n  a: [').error, /valid YAML/)
  assert.match(parseCompose('services:\n  a:\n    image: x\n    image: y\n').error, /valid YAML/) // duplicate keys
  assert.equal(parseCompose('just text').ok, false)
  assert.equal(parseCompose('x'.repeat(300 << 10)).ok, false)
  // alias bomb
  const bomb = ['a: &a [x,x,x,x,x,x,x,x,x,x]']
  for (let i = 1; i < 10; i++) bomb.push(`${'b'.repeat(i)}: &${'b'.repeat(i)} [${Array(10).fill(i === 1 ? '*a' : '*' + 'b'.repeat(i - 1)).join(',')}]`)
  assert.equal(parseCompose(bomb.join('\n') + '\nservices:\n  s:\n    image: x\n').ok, false)
  // __proto__ keys stay plain data
  const r = one('services:\n  a:\n    image: x\n    labels:\n      __proto__: polluted\n    environment:\n      __proto__: y\n')
  assert.equal({}.polluted, undefined)
  assert.deepEqual(r.adv.labels, [{ key: '__proto__', value: 'polluted' }])
  // values that try to inject YAML stay inside quotes when the form writes them out
  const evil = one('services:\n  a:\n    image: x\n    environment:\n      A: "x\\"\\n    privileged: true"\n')
  assert.ok(!/^\s*privileged:/m.test(composeFor(spec(evil))))
  // merge keys work
  const m = one('x-common: &c\n  restart: always\nservices:\n  a:\n    <<: *c\n    image: x\n')
  assert.equal(m.restart, 'always')
})

test('helpers', () => {
  assert.deepEqual(splitImage('localhost:5000/app'), { image: 'localhost:5000/app', tag: 'latest' })
  assert.deepEqual(splitImage('localhost:5000/app:v2'), { image: 'localhost:5000/app', tag: 'v2' })
  assert.deepEqual(parseSize('1.5g'), { value: '1.5', unit: 'g' })
  assert.deepEqual(parseSize('2GB'), { value: '2', unit: 'g' })
  assert.deepEqual(parseSize('524288k'), { value: '512', unit: 'm' })
  assert.equal(parseSize('lots'), null)
  for (const args of [['a b', "it's", '$x', 'plain', ''], ['--flag=1', 'a"b', 'back\\slash']]) assert.deepEqual(splitArgs(joinArgs(args)).args, args)
})

test('recommends this computer’s time zone and free host ports', () => {
  const p = parseCompose(`
services:
  jellyfin:
    image: jellyfin/jellyfin:10.11
    environment:
      - TZ=Etc/UTC
    ports:
      - 8096:8096
      - 8920:8920
      - 7359:7359/udp
`)
  const ctx = { timeZone: 'Europe/London', usedPorts: new Map([['8096/tcp', 'old-media'], ['7359/udp', 'other']]), hostName: 'nas' }
  const r = importService(p, 'jellyfin', {}, {}, ctx)
  assert.equal(r.env[0].value, 'Europe/London')
  assert.deepEqual(r.ports.map((x) => x.host), ['8097', '8920', '7360'])
  assert.deepEqual(r.ports.map((x) => x.container), ['8096', '8920', '7359'])
  const ch = Object.fromEntries(r.changes.map((c) => [c.id, c]))
  assert.deepEqual([ch.tz.from, ch.tz.to, ch.tz.keepable], ['Etc/UTC', 'Europe/London', true])
  assert.match(ch['port:0'].why, /already used by old-media on nas/)
  // Keeping the file's values
  const kept = importService(p, 'jellyfin', {}, { 'change:tz': 'Etc/UTC', 'change:port:0': '8096' }, ctx)
  assert.equal(kept.env[0].value, 'Etc/UTC')
  assert.equal(kept.ports[0].host, '8096')
  // A UTC computer, or no context: nothing changes
  const utc = importService(p, 'jellyfin', {}, {}, { timeZone: 'Etc/UTC' })
  assert.equal(utc.env[0].value, 'Etc/UTC')
  assert.ok(!utc.changes.some((c) => c.id === 'tz' || c.id.startsWith('port:')))
  // A real zone in the file is left alone
  const set = importService(parseCompose('services:\n  a:\n    image: x:1\n    environment: {TZ: America/Chicago}\n'), 'a', {}, {}, ctx)
  assert.equal(set.env[0].value, 'America/Chicago')
  // \${TZ:-UTC} becomes one TZ change, not a variable default too
  const v = importService(parseCompose('services:\n  a:\n    image: x:1\n    environment: ["TZ=\${TZ:-UTC}"]\n'), 'a', {}, {}, ctx)
  assert.deepEqual(v.changes.map((c) => c.id).filter((id) => id.includes('TZ') || id === 'tz'), ['tz'])
  assert.equal(v.env[0].value, 'Europe/London')
})
