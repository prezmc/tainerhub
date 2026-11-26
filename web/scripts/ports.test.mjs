// Tests for src/ports.ts (published ports per container and per host). Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { addressText, anyAddress, bindingsOf, checkPort, hostPorts, loopback, parsePublished, portMappings } from '../src/ports.ts'

test('addresses', () => {
  for (const ip of [undefined, '', '0.0.0.0', '::', '[::]']) assert.ok(anyAddress(ip), String(ip))
  for (const ip of ['127.0.0.1', '::1', '192.168.1.5']) assert.ok(!anyAddress(ip), ip)
  for (const ip of ['127.0.0.1', '127.1.2.3', '::1', '[::1]', 'localhost']) assert.ok(loopback(ip), ip)
  for (const ip of [undefined, '', '0.0.0.0', '10.0.0.1', '::']) assert.ok(!loopback(ip), String(ip))
  assert.equal(addressText([]), 'all addresses')
  assert.equal(addressText(['127.0.0.1']), '127.0.0.1 (this host only)')
  assert.equal(addressText(['10.0.0.2', '127.0.0.1']), '10.0.0.2, 127.0.0.1')
})

test('parsing the compact form', () => {
  assert.deepEqual(parsePublished('8080->80/tcp'), { ip: '', hostPort: 8080, port: 80, proto: 'tcp' })
  assert.deepEqual(parsePublished('0.0.0.0:7000->7000/udp'), { ip: '0.0.0.0', hostPort: 7000, port: 7000, proto: 'udp' })
  assert.deepEqual(parsePublished('[::1]:53->53/udp'), { ip: '::1', hostPort: 53, port: 53, proto: 'udp' })
  assert.deepEqual(parsePublished('8080->80'), { ip: '', hostPort: 8080, port: 80, proto: 'tcp' })
  assert.equal(parsePublished('80/tcp'), null)
  assert.equal(parsePublished('x->80/tcp'), null)
  // Addresses from current agents win; older agents only send the compact form.
  const published = [{ ip: '127.0.0.1', hostPort: 9000, port: 9000, proto: 'tcp' }]
  assert.deepEqual(bindingsOf({ ports: ['9000->9000/tcp'], published }), published)
  assert.deepEqual(bindingsOf({ ports: ['9000->9000/tcp', '443/tcp'] }), [{ ip: '', hostPort: 9000, port: 9000, proto: 'tcp' }])
  assert.deepEqual(bindingsOf({}), [])
})

test('ports in use on a host', () => {
  const cs = [
    {
      name: 'web', hostId: 'h1', ports: ['443/tcp', '8080->80/tcp', '9000->9000/udp'],
      published: [
        { ip: '0.0.0.0', hostPort: 8080, port: 80, proto: 'tcp' },
        { ip: '::', hostPort: 8080, port: 80, proto: 'tcp' },
        { ip: '127.0.0.1', hostPort: 9000, port: 9000, proto: 'udp' },
        { ip: '::1', hostPort: 9000, port: 9000, proto: 'udp' },
      ],
    },
    { name: 'old-agent', hostId: 'h1', ports: ['53->53/udp', '53->53/tcp', '8443->443/tcp'] },
    { name: 'admin', hostId: 'h1', published: [{ ip: '10.0.0.2', hostPort: 8080, port: 8080, proto: 'udp' }] },
    { name: 'pihole', hostId: 'h1', hostNetwork: true },
    { name: 'elsewhere', hostId: 'h2', ports: ['22->22/tcp'], hostNetwork: true },
  ]
  const { ports, hostNetwork } = hostPorts(cs, 'h1')
  assert.deepEqual(hostNetwork, ['pihole'])
  assert.deepEqual(ports, [
    { hostPort: 53, proto: 'tcp', addresses: [], container: 'old-agent', port: 53 },
    { hostPort: 53, proto: 'udp', addresses: [], container: 'old-agent', port: 53 },
    { hostPort: 8080, proto: 'tcp', addresses: [], container: 'web', port: 80 },
    { hostPort: 8080, proto: 'udp', addresses: ['10.0.0.2'], container: 'admin', port: 8080 },
    { hostPort: 8443, proto: 'tcp', addresses: [], container: 'old-agent', port: 443 },
    { hostPort: 9000, proto: 'udp', addresses: ['127.0.0.1', '::1'], container: 'web', port: 9000 },
  ])
  assert.deepEqual(hostPorts(cs, 'nope'), { ports: [], hostNetwork: [] })

  // Is a port free?
  assert.equal(checkPort(ports, 'abc'), null)
  assert.equal(checkPort(ports, '0'), null)
  assert.equal(checkPort(ports, '70000'), null)
  assert.deepEqual(checkPort(ports, ' 3000 '), { port: 3000, used: [], next: {} })
  const c = checkPort(ports, '8080')
  assert.deepEqual(c.used.map((p) => p.container), ['web', 'admin'])
  assert.deepEqual(c.next, { tcp: '8081', udp: '8081' })
  assert.deepEqual(checkPort(ports, '53').next, { tcp: '54', udp: '54' })
})

test('a container\'s port mappings', () => {
  assert.deepEqual(
    portMappings([
      { container: '443/tcp' },
      { container: '53/udp', host: '[::1]:5353', ip: '::1' },
      { container: '80/tcp', host: '8080', ip: '0.0.0.0' },
      { container: '80/tcp', host: '8080', ip: '::' },
      { container: '9000/udp', host: '127.0.0.1:9000', ip: '127.0.0.1' },
      { container: '9000/udp', host: '192.168.1.5:9000', ip: '192.168.1.5' },
      { container: '10000/tcp', host: '10000' },
    ]),
    [
      { container: '53/udp', hostPort: '5353', addresses: ['::1'] },
      { container: '80/tcp', hostPort: '8080', addresses: [] },
      { container: '9000/udp', hostPort: '9000', addresses: ['127.0.0.1', '192.168.1.5'] },
      { container: '10000/tcp', hostPort: '10000', addresses: [] },
      { container: '443/tcp', hostPort: undefined, addresses: [] },
    ],
  )
  // Older agents put a single address in host, without ip.
  assert.deepEqual(portMappings([{ container: '80/tcp', host: '127.0.0.1:8080' }]), [{ container: '80/tcp', hostPort: '8080', addresses: ['127.0.0.1'] }])
  assert.deepEqual(portMappings([]), [])
})
