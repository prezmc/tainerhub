// Tests for src/recommend.ts (time zone and free-port suggestions). Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { isUtcLike, localTimeZone, nextFreePort, portConflicts, usedHostPorts } from '../src/recommend.ts'

test('time zones', () => {
  for (const v of ['UTC', 'Etc/UTC', 'etc/utc', 'GMT', 'Etc/GMT', 'Etc/GMT+0', 'Zulu', ' UTC ']) assert.ok(isUtcLike(v), v)
  for (const v of ['Europe/London', 'America/New_York', 'Etc/GMT+1', '', 'UTC2']) assert.ok(!isUtcLike(v), v)
  assert.equal(typeof localTimeZone(), 'string')
})

test('ports in use on a host', () => {
  const cs = [
    { name: 'old-media', hostId: 'h1', ports: ['8096->8096/tcp', '1900->1900/udp', '0.0.0.0:7000->7000/tcp'] },
    { name: 'web', hostId: 'h1', project: 'site', ports: ['80->80/tcp'] },
    { name: 'elsewhere', hostId: 'h2', ports: ['8097->80/tcp'] },
    { name: 'internal', hostId: 'h1', ports: ['3000/tcp'] }, // not published
  ]
  const used = usedHostPorts(cs, 'h1')
  assert.deepEqual([...used.keys()].sort(), ['1900/udp', '7000/tcp', '80/tcp', '8096/tcp'])
  assert.ok(!usedHostPorts(cs, 'h1', 'site').has('80/tcp'))

  assert.equal(nextFreePort('8096', 'tcp', (k) => used.has(k)), '8097') // 8097 is only taken on h2
  assert.equal(nextFreePort('x', 'tcp', () => false), '')

  const rows = [
    { host: '8096', proto: 'tcp' },
    { host: '8097', proto: 'tcp' },
    { host: '', proto: 'tcp' },
    { host: '1900', proto: 'tcp' }, // only udp is taken
    { host: '8097', proto: 'tcp' }, // twice in the form
    { host: '1900', proto: 'udp' },
  ]
  assert.deepEqual(portConflicts(rows, used), [
    { index: 0, key: '8096/tcp', by: 'old-media', suggestion: '8098' },
    { index: 4, key: '8097/tcp', by: 'another port in this form', suggestion: '8099' },
    { index: 5, key: '1900/udp', by: 'old-media', suggestion: '1901' },
  ])
})
