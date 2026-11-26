// Tests for src/placeholder.ts and the importer's use of it. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { generateSecret, isSecretKey, looksLikePlaceholder, suggestSource } from '../src/placeholder.ts'
import { importService, parseCompose } from '../src/composeimport.ts'

// Same cases as internal/wire/placeholder_test.go, so both sides agree.
const YES = ['/path/to/prowlarr/data', '/PATH_TO_YOUR_CONFIG', '/path_to/config', 'path/to/media', '/your/media', '/mnt/your-media', 'YOUR_DOMAIN', 'yourpassword', '<your-domain>', '<config dir>', 'changeme', 'CHANGE_ME', 'change-this', 'replace_with_a_secret', 'xxxxxx', 'todo']
const NO = ['', '/srv/media', './config', '/opt/tainerhub/stacks/app/data', '/var/run/docker.sock', 'Europe/London', '/mnt/yourself', 'pathtology', '1000', 'postgres://db:5432/app', '/data/mypath/to', 'secret-key-123', 'https://example.com/youri', '/home/user/todolist']

test('placeholder detection', () => {
  for (const s of YES) assert.ok(looksLikePlaceholder(s), s)
  for (const s of NO) assert.ok(!looksLikePlaceholder(s), s)
})

test('suggestions', () => {
  assert.equal(suggestSource('/config'), './config')
  assert.equal(suggestSource('/var/lib/App Data:ro'), './app-data')
  assert.equal(suggestSource('/'), './data')
  for (const k of ['DB_PASSWORD', 'SECRET_KEY', 'API_KEY', 'JWT_SECRET', 'TOKEN', 'PASS']) assert.ok(isSecretKey(k), k)
  for (const k of ['TZ', 'DOMAIN', 'PUID', 'KEYBOARD']) assert.ok(!isSecretKey(k), k)
  const a = generateSecret()
  assert.match(a, /^[A-Za-z0-9]{32}$/)
  assert.notEqual(a, generateSecret())
})

test('the importer asks about examples and applies replacements', () => {
  const p = parseCompose(`
services:
  prowlarr:
    image: lscr.io/linuxserver/prowlarr:latest
    environment:
      - TZ=Etc/UTC
      - API_KEY=changeme
    volumes:
      - /path/to/prowlarr/data:/config
      - /srv/downloads:/downloads
    labels:
      traefik.http.routers.p.rule: Host(\`prowlarr.<your-domain>\`)
`)
  assert.ok(p.ok)
  const r = importService(p, 'prowlarr')
  assert.deepEqual(
    r.placeholders.map((x) => [x.id, x.suggestion, x.secret]),
    [
      ['volume:0', './config', undefined],
      ['env:API_KEY', undefined, true],
      ['label:traefik.http.routers.p.rule', undefined, undefined],
    ],
  )
  assert.equal(r.notes.filter((n) => n.msg.includes('looks like an example')).length, 3)

  const fixed = importService(p, 'prowlarr', {}, { 'volume:0': './config', 'env:API_KEY': 'k3y', 'label:traefik.http.routers.p.rule': ' ' })
  assert.equal(fixed.vols[0].source, './config')
  assert.equal(fixed.env.find((e) => e.key === 'API_KEY').value, 'k3y')
  assert.equal(fixed.placeholders.length, 3) // still listed, so the inputs stay
  assert.equal(fixed.notes.filter((n) => n.msg.includes('looks like an example')).length, 1) // the blank one
})
