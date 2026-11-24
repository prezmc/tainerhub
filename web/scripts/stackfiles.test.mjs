// Tests for src/stackfiles.ts. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { composeBuilds, composeBuildsFromGit, filesEqual, filesProblem, isDockerfile, pathProblem, TEMPLATES } from '../src/stackfiles.ts'

test('pathProblem matches the hub rules', () => {
  for (const ok of ['Dockerfile', 'entrypoint.sh', 'conf/nginx.conf', 'a/b/c/d.txt', 'web/.env', 'x'.repeat(200)]) assert.equal(pathProblem(ok), null, ok)
  for (const bad of ['', '/etc/passwd', '../x', 'a/../b', './Dockerfile', 'a//b', 'a/', 'a\\b', 'my file', 'naïve', '.tainerhub-files', 'sub/.tainerhubx', 'compose.yaml', 'docker-compose.yml', '.env', 'a/b/c/d/e', 'x'.repeat(201)])
    assert.notEqual(pathProblem(bad), null, bad)
})

test('pathProblem catches clashes with other files', () => {
  assert.match(pathProblem('Dockerfile', ['Dockerfile']), /already has/)
  assert.match(pathProblem('conf', ['conf/a.ini']), /folder/)
  assert.match(pathProblem('conf/a.ini', ['conf']), /is a file/)
  assert.equal(pathProblem('conf/b.ini', ['conf/a.ini']), null)
})

test('filesProblem checks count, size and text', () => {
  assert.equal(filesProblem([{ path: 'a', content: 'x' }]), null)
  assert.match(filesProblem(Array.from({ length: 31 }, (_, i) => ({ path: `f${i}`, content: '' }))), /at most 30/)
  assert.match(filesProblem([{ path: 'big', content: 'x'.repeat((256 << 10) + 1) }]), /big is larger/)
  assert.match(filesProblem(Array.from({ length: 9 }, (_, i) => ({ path: `f${i}`, content: 'x'.repeat(256 << 10) }))), /2 MB/)
  assert.match(filesProblem([{ path: 'bin', content: 'a\0b' }]), /isn't text/)
})

test('build detection', () => {
  assert.ok(composeBuilds('services:\n  web:\n    build: .\n'))
  assert.ok(composeBuilds('services:\n  web:\n    build:\n      context: ./web\n'))
  assert.ok(!composeBuilds('services:\n  web:\n    image: nginx # build: no\n'))
  assert.ok(composeBuildsFromGit('    build: https://github.com/org/repo.git#main\n'))
  assert.ok(composeBuildsFromGit('    build:\n      context: "https://github.com/org/repo.git"\n'))
  assert.ok(!composeBuildsFromGit('    build: .\n'))
  assert.ok(isDockerfile('Dockerfile') && isDockerfile('api/worker.Dockerfile') && isDockerfile('Dockerfile.dev') && !isDockerfile('notes.txt'))
})

test('filesEqual and templates', () => {
  assert.ok(filesEqual([{ path: 'a', content: 'x' }], [{ path: 'a', content: 'x', executable: false }]))
  assert.ok(!filesEqual([{ path: 'a', content: 'x' }], [{ path: 'a', content: 'x', executable: true }]))
  for (const t of TEMPLATES) if (t.path) assert.equal(pathProblem(t.path), null, t.id)
  assert.ok(TEMPLATES.find((t) => t.id === 'entrypoint').executable)
})
