// Tests for src/rollback.ts. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { changedServices, displayTag, imageText, isRollbackTag, shortId, triggerText } from '../src/rollback.ts'

const id = 'sha256:' + 'ab'.repeat(32)

test('imageText shows the name and short ID', () => {
  assert.equal(imageText({ ref: 'nginx:1.27', imageId: id }), 'nginx:1.27 · abababababab')
  assert.equal(imageText({ ref: 'nginx:1.27', imageId: '' }), 'nginx:1.27')
  assert.equal(imageText({ ref: '', imageId: id }), 'abababababab')
  assert.equal(imageText(null), 'not running')
  assert.equal(shortId(undefined), '')
})

test('triggerText names what the point was recorded for', () => {
  assert.equal(triggerText('auto'), 'before an automatic update')
  assert.equal(triggerText('pull'), 'before a Pull & redeploy')
})

test('changedServices skips services already on the old image', () => {
  const p = { services: [{ same: true }, { same: false }, { same: false }] }
  assert.equal(changedServices(p), 2)
})

test('displayTag prefers the image own name over a rollback tag', () => {
  assert.equal(displayTag(['tainerhub-rollback/app:web', 'nginx:1']), 'nginx:1')
  assert.equal(displayTag(['tainerhub-rollback/app:web']), 'tainerhub-rollback/app:web')
  assert.equal(displayTag([]), undefined)
  assert.ok(isRollbackTag('tainerhub-rollback/app/pinned:web'))
  assert.ok(!isRollbackTag('ghcr.io/tainerhub-rollback/x:1'))
})
