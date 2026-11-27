// Tests for src/shares.ts. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { blankShare, shareBody, shareProblem, shareText } from '../src/shares.ts'
import { addExternalVolume } from '../src/sharecompose.ts'

const ok = () => ({ ...blankShare('cifs'), name: 'media', server: '192.0.2.10', path: 'media', username: 'u', password: 'p=w!' })

test('labels share volumes without any secret', () => {
  assert.equal(shareText({ type: 'cifs', server: '192.0.2.10', path: 'media' }), 'SMB //192.0.2.10/media')
  assert.equal(shareText({ type: 'nfs', server: 'nas', path: '/volume1/media' }), 'NFS nas:/volume1/media')
})

test('checks the form like the hub does', () => {
  assert.equal(shareProblem(ok()), null)
  assert.equal(shareProblem({ ...blankShare('nfs'), name: 'm', server: 'nas.lan', path: '/v1/media' }), null)
  const bad = {
    comma: { password: 'a,b' },
    quote: { password: 'a"b' },
    newline: { password: 'a\nb' },
    userComma: { username: 'u,uid=0' },
    unc: { server: '//nas/media' },
    noServer: { server: '' },
    noShare: { path: '/' },
    uid: { uid: 'root' },
    mode: { fileMode: '999' },
    name: { name: '-x' },
    noUser: { username: '' },
  }
  for (const [k, v] of Object.entries(bad)) assert.notEqual(shareProblem({ ...ok(), ...v }), null, k)
  assert.match(shareProblem({ ...blankShare('nfs'), name: 'm', server: 'nas', path: 'v1' }), /starts with \//)
})

test('sends only what the share type uses', () => {
  const nfs = shareBody({ ...ok(), type: 'nfs', path: '/x', version: '4' })
  assert.equal(nfs.password, undefined)
  assert.equal(nfs.username, undefined)
  const cifs = shareBody({ ...ok(), name: ' media ' })
  assert.equal(cifs.name, 'media')
  assert.equal(cifs.password, 'p=w!')
})

test('adds an external volume to a compose file', () => {
  const c = 'services:\n  qbit:\n    image: qbit # the client\n    volumes:\n      - media:/data\n'
  const out = addExternalVolume(c, 'media')
  assert.match(out, /# the client/)
  assert.match(out, /\nvolumes:\n {2}media:\n {4}external: true\n/)
  assert.equal(addExternalVolume(out, 'media'), out, 'already there')
  const two = addExternalVolume('services: {}\nvolumes:\n  db: {}\n', 'media')
  assert.match(two, /db: \{\}/)
  assert.match(two, /media:\n {4}external: true/)
  assert.equal(addExternalVolume('services: [', 'media'), null)
  assert.equal(addExternalVolume('volumes: [a]\n', 'media'), null)
})

test('splits a pasted share address', async () => {
  const { parseShareAddress } = await import('../src/shares.ts')
  assert.deepEqual(parseShareAddress('\\\\192.0.2.10\\media'), { type: 'cifs', server: '192.0.2.10', path: 'media' })
  assert.deepEqual(parseShareAddress('\\\\nas\\media\\movies\\'), { type: 'cifs', server: 'nas', path: 'media/movies' })
  assert.deepEqual(parseShareAddress('//nas/media'), { type: 'cifs', server: 'nas', path: 'media' })
  assert.deepEqual(parseShareAddress('smb://nas.lan/media'), { type: 'cifs', server: 'nas.lan', path: 'media' })
  assert.deepEqual(parseShareAddress('nas:/volume1/media'), { type: 'nfs', server: 'nas', path: '/volume1/media' })
  assert.deepEqual(parseShareAddress('nfs://10.0.0.5/export/x'), { type: 'nfs', server: '10.0.0.5', path: '/export/x' })
  for (const s of ['media', 'media/movies', '192.0.2.10', '/volume1/media', '']) assert.equal(parseShareAddress(s), null, s)
})
