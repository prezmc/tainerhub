// Tests for src/join.ts. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { directAlternative, joinCommand } from '../src/join.ts'

test('joinCommand builds the pasted line', () => {
  assert.equal(
    joinCommand('https://tainerhub.example.com/', 'thj1_abc', '192.168.1.10:7443'),
    'curl -fsSL "https://tainerhub.example.com/join/thj1_abc?hub=192.168.1.10%3A7443" | sh',
  )
})

test('directAlternative offers the hub only when the page is elsewhere', () => {
  assert.equal(directAlternative('https://tainerhub.example.com', 'http://192.168.1.10:8585'), 'http://192.168.1.10:8585')
  assert.equal(directAlternative('http://192.168.1.10:8585', 'http://192.168.1.10:8585'), null)
  assert.equal(directAlternative('http://192.168.1.10:8585', undefined), null)
  assert.equal(directAlternative('http://192.168.1.10:8585', 'not a url'), null)
})
