// Tests for src/sharednet.ts. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { networkOwner, networkUsers, sharedNetConsequence, usesNetworkOf } from '../src/sharednet.ts'

const id = 'f9d22246d25b8b3634bf355dc6a600a126f2efe761b2444e3efef783862cb213'
const gw = { id, name: 'gluetun', hostId: 'h1' }
const all = [
  gw,
  { id: 'b', name: 'qbit', hostId: 'h1', networkOf: id },
  { id: 'c', name: 'arr', hostId: 'h1', networkOf: 'gluetun' },
  { id: 'd', name: 'short', hostId: 'h1', networkOf: id.slice(0, 12) },
  { id: 'e', name: 'other-host', hostId: 'h2', networkOf: 'gluetun' },
  { id: 'f', name: 'too-short', hostId: 'h1', networkOf: id.slice(0, 4) },
  { id: 'g', name: 'plain', hostId: 'h1' },
]

test('finds the containers in a gateway network by ID or name, on the same host', () => {
  assert.deepEqual(networkUsers(gw, all), ['arr', 'qbit', 'short'])
  assert.equal(usesNetworkOf(gw, gw), false)
  assert.equal(networkOwner(all[1], all)?.name, 'gluetun')
  assert.equal(networkOwner(all[4], all), undefined)
  assert.equal(networkOwner(all[6], all), undefined)
})

test('says what breaks', () => {
  assert.match(sharedNetConsequence(['qbit'], 'stop'), /qbit runs in this container's network: it loses all networking .* restart it too/)
  assert.match(sharedNetConsequence(['a', 'b'], 'restart'), /a, b run .* Restart them afterwards/)
  assert.match(sharedNetConsequence(['a'], 'remove'), /can't start again until it's recreated/)
})
