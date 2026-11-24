// Tests for src/autoupdates.ts. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { containerAutoProblem, daysText, durationText, findTarget, resultText, windowText } from '../src/autoupdates.ts'

test('daysText names common sets', () => {
  assert.equal(daysText(['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun']), 'every day')
  assert.equal(daysText(['fri', 'mon', 'tue', 'wed', 'thu']), 'weekdays')
  assert.equal(daysText(['sun', 'sat']), 'weekends')
  assert.equal(daysText(['fri', 'mon', 'wed']), 'Mon, Wed, Fri')
})

test('windowText reads as one line', () => {
  const w = { settings: { enabled: true, days: ['sat', 'sun'], time: '04:30', timeZone: 'Europe/Berlin', maxMinutes: 90 }, zone: 'Europe/Berlin', next: null }
  assert.equal(windowText(w), 'weekends at 04:30 (Europe/Berlin), up to 90 min')
  assert.equal(durationText(120), '2 h')
})

test('resultText explains skips', () => {
  assert.equal(resultText('ok'), 'updated')
  assert.equal(resultText('skipped', 'host offline'), 'skipped: host offline')
  assert.equal(resultText(''), 'not run yet')
})

test('findTarget matches stacks by ID and containers by host and name', () => {
  const targets = [
    { id: '1', kind: 'stack', stackId: 's1', hostId: 'h1' },
    { id: '2', kind: 'container', containerName: 'web', hostId: 'h1' },
  ]
  assert.equal(findTarget(targets, { stackId: 's1' })?.id, '1')
  assert.equal(findTarget(targets, { hostId: 'h1', name: 'web' })?.id, '2')
  assert.equal(findTarget(targets, { hostId: 'h2', name: 'web' }), undefined)
})

test('containerAutoProblem mirrors the hub', () => {
  assert.equal(containerAutoProblem({ name: 'web', image: 'nginx:1' }), null)
  assert.match(containerAutoProblem({ name: 'app-1', project: 'app', image: 'x' }), /stack/)
  assert.match(containerAutoProblem({ name: 'tainerhub-agent', image: 'x' }), /TainerHub/)
  assert.match(containerAutoProblem({ name: 'r', image: 'redis@sha256:ab' }), /pinned/)
})
