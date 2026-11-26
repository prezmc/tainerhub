// Tests for src/hostscope.ts. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { allHosts, hubAdmin, sameScope, scopeBody, scopeOf, scopeProblem, scopeText, toggleHost } from '../src/hostscope.ts'

const hosts = [
  { id: 'b', name: 'beta' },
  { id: 'a', name: 'alpha' },
  { id: 'hub', name: 'nas', hubHost: true },
]

test('scopeOf reads the API shape', () => {
  assert.deepEqual(scopeOf({ scoped: false }), allHosts)
  assert.deepEqual(scopeOf({ scoped: true, hosts: ['a'] }), { scoped: true, hosts: ['a'] })
  assert.deepEqual(scopeOf({ scoped: true }), { scoped: true, hosts: [] })
  assert.deepEqual(scopeOf({}), allHosts)
})

test('hubAdmin needs admin and every host', () => {
  assert.equal(hubAdmin({ role: 'owner' }), true)
  assert.equal(hubAdmin({ role: 'admin', scoped: false }), true)
  assert.equal(hubAdmin({ role: 'admin', scoped: true }), false)
  assert.equal(hubAdmin({ role: 'operator' }), false)
  assert.equal(hubAdmin(undefined), false)
})

test('scopeText names the hosts', () => {
  assert.equal(scopeText(allHosts, hosts), 'All hosts')
  assert.equal(scopeText({ scoped: true, hosts: ['b', 'a'] }, hosts), 'alpha, beta')
  assert.equal(scopeText({ scoped: true, hosts: ['a', 'gone'] }, hosts), 'alpha, 1 removed host')
  assert.equal(scopeText({ scoped: true, hosts: [] }, hosts), 'No hosts')
})

test('toggleHost', () => {
  assert.deepEqual(toggleHost(['a'], 'b'), ['a', 'b'])
  assert.deepEqual(toggleHost(['a', 'b'], 'a'), ['b'])
})

test('scopeProblem mirrors the hub', () => {
  assert.equal(scopeProblem('admin', allHosts, hosts), null)
  assert.match(scopeProblem('viewer', { scoped: true, hosts: [] }, hosts), /at least one/)
  assert.match(scopeProblem('viewer', { scoped: true, hosts: ['gone'] }, hosts), /at least one/)
  assert.equal(scopeProblem('operator', { scoped: true, hosts: ['hub'] }, hosts), null)
  assert.match(scopeProblem('admin', { scoped: true, hosts: ['a', 'hub'] }, hosts), /hub's own server/)
  assert.equal(scopeProblem('admin', { scoped: true, hosts: ['a'] }, hosts), null)
})

test('scopeBody sends null for every host and drops removed ones', () => {
  assert.equal(scopeBody(allHosts, hosts), null)
  assert.deepEqual(scopeBody({ scoped: true, hosts: ['a', 'gone'] }, hosts), ['a'])
})

test('sameScope ignores order', () => {
  assert.equal(sameScope(allHosts, { scoped: false, hosts: ['a'] }), true)
  assert.equal(sameScope({ scoped: true, hosts: ['a', 'b'] }, { scoped: true, hosts: ['b', 'a'] }), true)
  assert.equal(sameScope({ scoped: true, hosts: ['a'] }, { scoped: true, hosts: ['a', 'b'] }), false)
  assert.equal(sameScope(allHosts, { scoped: true, hosts: [] }), false)
})
