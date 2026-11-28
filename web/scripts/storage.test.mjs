// Tests for src/storage.ts (container disk use). Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { byStorage, bytes, storageDetail, storageOf } from '../src/storage.ts'

test('formatting sizes', () => {
  assert.equal(bytes(undefined), '–')
  assert.equal(bytes(0), '0 B')
  assert.equal(bytes(999), '999 B')
  assert.equal(bytes(12_345), '12 KB')
  assert.equal(bytes(40_000_000), '40 MB')
  assert.equal(bytes(1_234_000_000), '1.2 GB')
  assert.equal(bytes(3_000_000_000_000), '3.0 TB')
})

test('total and detail', () => {
  assert.equal(storageOf({}), undefined)
  assert.equal(storageOf({ size: { rw: 0, at: 1 } }), 0)
  assert.equal(storageOf({ size: { rw: 40_000_000, volumes: 1_200_000_000, at: 1 } }), 1_240_000_000)
  assert.equal(storageDetail({ rw: 40_000_000, at: 1 }), '40 MB written by the container')
  assert.equal(storageDetail({ rw: 40_000_000, volumes: 1_200_000_000, at: 1 }), '40 MB written by the container, 1.2 GB in volumes')
})

test('sorting: biggest first, unmeasured (older agents) last', () => {
  const cs = [
    { name: 'old-agent' },
    { name: 'small', size: { rw: 10, at: 1 } },
    { name: 'empty', size: { rw: 0, at: 1 } },
    { name: 'big', size: { rw: 10, volumes: 5000, at: 1 } },
    { name: 'also-old' },
  ]
  assert.deepEqual(cs.sort(byStorage).map((c) => c.name), ['big', 'small', 'empty', 'also-old', 'old-agent'])
})
