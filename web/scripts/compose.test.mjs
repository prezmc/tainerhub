// Tests for src/compose.ts. Run with `npm test` (Node 23.6+ strips the TypeScript types).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { allProblems, blankAdvanced, composeFor, problems, setCounts, splitArgs } from '../src/compose.ts'

const base = (adv = {}) => ({
  name: 'web',
  image: 'nginx:latest',
  restart: 'unless-stopped',
  ports: [{ host: '8080', container: '80', proto: 'tcp' }],
  env: [],
  vols: [],
  adv: { ...blankAdvanced(), ...adv },
})

// Every line composeFor writes must be one of these shapes, with user text
// only ever inside a JSON (= YAML double-quoted) string. That's what stops
// a value from breaking out into new keys.
const KEY = String.raw`(?:[a-z_][a-z_-]*|"(?:[^"\\]|\\.)*")`
const STR = String.raw`"(?:[^"\\]|\\.)*"`
const SCALAR = String.raw`(?:${STR}|\d+|true|false|all|nvidia|json-file|\{\}|\[gpu\]|\["CMD-SHELL", ${STR}\])`
const LINE = new RegExp(String.raw`^( {2})*(?:${KEY}:(?: ${SCALAR})?|- ${SCALAR}|- path: ${STR}|- driver: nvidia)$`)

function wellFormed(yaml) {
  for (const line of yaml.trimEnd().split('\n')) assert.match(line, LINE, `unexpected line: ${JSON.stringify(line)}`)
  // Every quoted string must parse as JSON (and so as YAML).
  for (const m of yaml.matchAll(new RegExp(STR, 'g'))) JSON.parse(m[0])
}

/** The decoded value of `key: "..."` lines and `- "..."` items, in order. */
const strings = (yaml) => [...yaml.matchAll(new RegExp(STR, 'g'))].map((m) => JSON.parse(m[0]))

test('the simple path is unchanged in shape', () => {
  const y = composeFor(base())
  assert.equal(y, 'services:\n  web:\n    image: "nginx:latest"\n    container_name: "web"\n    restart: "unless-stopped"\n    ports:\n      - "8080:80/tcp"\n')
  assert.equal(problems(base()), null)
})

test('hostile values stay inside quoted scalars', () => {
  const evil = 'x"\n    privileged: true\n#: - &a *a !tag $HOME ${X} \\ \u0000 \u0085 \u2028 \ufeff \ud800'
  const spec = base({
    labels: [{ key: 'traefik.http.routers.web.rule', value: evil }],
    command: `sh -c ${JSON.stringify(evil)}`,
    hcTest: evil,
    health: 'custom',
    workingDir: '/app: #x',
    user: '1000:1000',
  })
  spec.env = [{ key: 'A', value: evil }]
  spec.vols = [{ source: './data', target: '/data # x' }]
  const y = composeFor(spec)
  wellFormed(y)
  assert.ok(!/^\s*privileged:/m.test(y), 'no injected key')
  assert.ok(!/[\u0085\u2028\u2029\ufeff]/.test(y), 'YAML line breaks and BOMs are escaped')
  const s = strings(y)
  // "$" is doubled so compose doesn't interpolate it; the lone surrogate is dropped.
  const want = evil.replaceAll('$', '$$$$').replace('\ud800', '')
  assert.ok(s.includes(want), 'label/env value round-trips')
  assert.ok(s.includes('/app: #x'))
})

test('networking', () => {
  let y = composeFor(base({ networks: ['proxy'], newNetwork: 'backend', hostname: 'app', extraHosts: [{ key: 'db.lan', value: '10.0.0.5' }], dns: '1.1.1.1, 9.9.9.9' }))
  wellFormed(y)
  assert.match(y, /    networks:\n      - "proxy"\n      - "backend"\n/)
  assert.match(y, /\nnetworks:\n  "proxy":\n    external: true\n  "backend":\n    name: "backend"\n/)
  assert.match(y, /extra_hosts:\n      - "db.lan:10.0.0.5"/)
  assert.match(y, /dns:\n      - "1.1.1.1"\n      - "9.9.9.9"/)

  y = composeFor(base({ networkMode: 'host', networks: ['proxy'] }))
  assert.match(y, /network_mode: "host"/)
  assert.ok(!y.includes('ports:'), 'ports are dropped on the host network')
  assert.ok(!/networks:/.test(y))
  assert.match(problems(base({ networkMode: 'host', networks: ['proxy'] })), /can't be joined/)
  assert.match(problems(base({ newNetwork: 'proxy' }), ['proxy']), /already exists/)
  assert.match(problems(base({ dns: '1.1.1' })), /DNS server/)
  assert.match(problems(base({ extraHosts: [{ key: 'db', value: 'nope' }] })), /isn't an IP/)
  assert.equal(problems(base({ extraHosts: [{ key: 'host.docker.internal', value: 'host-gateway' }] })), null)
})

test('labels', () => {
  const y = composeFor(base({ labels: [{ key: 'traefik.enable', value: 'true' }, { key: 'traefik.http.routers.web.rule', value: 'Host(`app.example.com`)' }] }))
  wellFormed(y)
  assert.match(y, /labels:\n      "traefik.enable": "true"\n      "traefik.http.routers.web.rule": "Host\(`app.example.com`\)"/)
  assert.match(problems(base({ labels: [{ key: 'bad key', value: '' }] })), /Label key/)
  assert.match(problems(base({ labels: [{ key: 'a', value: '1' }, { key: 'a', value: '2' }] })), /twice/)
})

test('splitArgs', () => {
  assert.deepEqual(splitArgs(`sh -c 'echo "hi there"' x\\ y "a\\"b" ''`).args, ['sh', '-c', 'echo "hi there"', 'x y', 'a"b', ''])
  assert.deepEqual(splitArgs('  --port   8080 ').args, ['--port', '8080'])
  assert.ok(splitArgs(`echo 'oops`).error)
  assert.ok(splitArgs('echo "oops').error)
  assert.match(problems(base({ command: 'echo "oops' })), /never closed/)
})

test('command & runtime', () => {
  const spec = base({ command: 'serve --port 80', entrypoint: '/init', workingDir: '/app', user: '1000:1000', init: true, readOnly: true, stopGrace: '1m30s', tz: 'Europe/London' })
  const y = composeFor(spec)
  wellFormed(y)
  assert.match(y, /command:\n      - "serve"\n      - "--port"\n      - "80"\n/)
  assert.match(y, /entrypoint:\n      - "\/init"\n/)
  assert.match(y, /init: true\n    read_only: true\n    stop_grace_period: "1m30s"/)
  assert.match(y, /environment:\n      "TZ": "Europe\/London"/)
  // TZ from the shortcut never overrides one set by hand
  spec.env = [{ key: 'TZ', value: 'UTC' }]
  assert.ok(!composeFor(spec).includes('Europe/London'))
  assert.match(problems(base({ workingDir: 'app' })), /absolute/)
  assert.match(problems(base({ user: 'a b' })), /User/)
  assert.match(problems(base({ stopGrace: '10' })), /duration/)
})

test('resources', () => {
  const y = composeFor(base({ memory: '1.5', memoryUnit: 'g', cpus: '0.5', shm: '256', devices: ['/dev/dri', '/dev/ttyUSB0:/dev/ttyACM0:rw'], gpu: true }))
  wellFormed(y)
  assert.match(y, /mem_limit: "1.5g"\n    cpus: "0.5"\n    shm_size: "256m"/)
  assert.match(y, /devices:\n      - "\/dev\/dri"\n      - "\/dev\/ttyUSB0:\/dev\/ttyACM0:rw"/)
  assert.match(y, /deploy:\n      resources:\n        reservations:\n          devices:\n            - driver: nvidia\n              count: all\n              capabilities: \[gpu\]/)
  assert.match(problems(base({ memory: '0' })), /Memory/)
  assert.match(problems(base({ memory: '4' })), /6 MB/)
  assert.match(problems(base({ memory: '512MB' })), /Memory/)
  assert.match(problems(base({ cpus: '-1' })), /CPU/)
  assert.match(problems(base({ devices: ['/srv/x'] })), /\/dev\//)
  assert.match(problems(base({ devices: ['/dev/dri:/dev/dri:rwx'] })), /permissions/)
})

test('health check', () => {
  let y = composeFor(base({ health: 'custom', hcTest: 'curl -f http://localhost/ || exit 1', hcInterval: '30s', hcTimeout: '5s', hcRetries: '3', hcStart: '1m' }))
  wellFormed(y)
  assert.match(y, /healthcheck:\n      test: \["CMD-SHELL", "curl -f http:\/\/localhost\/ \|\| exit 1"\]\n      interval: "30s"\n      timeout: "5s"\n      retries: 3\n      start_period: "1m"/)
  y = composeFor(base({ health: 'disable' }))
  assert.match(y, /healthcheck:\n      disable: true/)
  assert.match(problems(base({ health: 'custom' })), /enter a command/)
  assert.match(problems(base({ health: 'custom', hcTest: 'true', hcInterval: '30' })), /Interval/)
  assert.match(problems(base({ health: 'custom', hcTest: 'true', hcRetries: '0' })), /Retries/)
})

test('security & logging', () => {
  const y = composeFor(base({ privileged: true, capAdd: ['NET_ADMIN'], capDrop: ['ALL'], noNewPrivileges: true, logLimit: true }))
  wellFormed(y)
  assert.match(y, /privileged: true\n    cap_add:\n      - "NET_ADMIN"\n    cap_drop:\n      - "ALL"\n    security_opt:\n      - "no-new-privileges:true"/)
  assert.match(y, /logging:\n      driver: json-file\n      options:\n        max-size: "10m"\n        max-file: "3"/)
  assert.match(problems(base({ capAdd: ['NET_RAW'], capDrop: ['NET_RAW'] })), /both added and dropped/)
  assert.match(problems(base({ logLimit: true, logMaxSize: '10MB' })), /Max size/)
})

test('env file', () => {
  const y = composeFor(base({ envFile: '# secrets\nDB_PASSWORD=s3cret\n\nexport API_KEY=x' }))
  wellFormed(y)
  assert.match(y, /env_file:\n      - path: ".env"\n        required: false/)
  assert.ok(!y.includes('s3cret'), 'secrets stay out of the compose file')
  assert.ok(!composeFor(base({ envFile: '# only a comment' })).includes('env_file'))
  const p = allProblems(base({ envFile: 'OK=1\nnot a var\n9X=1' })).map((x) => x.msg)
  assert.deepEqual(p, ['.env line 2: use NAME=value (names use letters, digits and _).', '.env line 3: use NAME=value (names use letters, digits and _).'])
})

test('basic validation', () => {
  assert.match(problems({ ...base(), image: '' }), /Pick an image/)
  assert.match(problems({ ...base(), name: 'Bad Name' }), /Name/)
  assert.match(problems({ ...base(), ports: [{ host: '70000', container: '80', proto: 'tcp' }] }), /65535/)
  assert.match(problems({ ...base(), env: [{ key: '1X', value: '' }] }), /Environment/)
  assert.match(problems({ ...base(), vols: [{ source: 'data', target: 'data' }] }), /absolute/)
  // ports aren't checked when the host network makes them meaningless
  assert.equal(problems({ ...base({ networkMode: 'host' }), ports: [{ host: 'x', container: '', proto: 'tcp' }] }), null)
})

test('set counts', () => {
  assert.deepEqual(setCounts(blankAdvanced()), { network: 0, labels: 0, runtime: 0, resources: 0, health: 0, security: 0, logging: 0, envfile: 0 })
  const c = setCounts({ ...blankAdvanced(), dns: '1.1.1.1', networks: ['a', 'b'], capAdd: ['NET_ADMIN', 'SYS_TIME'], privileged: true, init: true })
  assert.equal(c.network, 2)
  assert.equal(c.security, 3)
  assert.equal(c.runtime, 1)
})

test('a network share volume on the host is used as external', () => {
  const spec = { ...base(), vols: [{ source: 'media', target: '/data' }, { source: 'config', target: '/config' }], externalVols: ['media', 'other'] }
  const y = composeFor(spec)
  wellFormed(y)
  assert.match(y, /\nvolumes:\n {2}"media":\n {4}external: true\n {2}"config": \{\}\n$/)
  assert.ok(!y.includes('other'), 'only volumes the container mounts')
})
