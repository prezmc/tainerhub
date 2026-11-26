// Tests for src/own.ts. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { imageName, ownConsequence, ownContainer, ownResource, ownServerCommands } from '../src/own.ts'

const c = (name, image, project) => ({ name, image, project, hostName: 'nas' })

test('imageName drops registry, owner, tag and digest', () => {
  assert.equal(imageName('ghcr.io/prezmc/tainerhub-agent:latest'), 'tainerhub-agent')
  assert.equal(imageName('ghcr.io/prezmc/tainerhub@sha256:abc'), 'tainerhub')
  assert.equal(imageName('localhost:5000/tainerhub'), 'tainerhub')
  assert.equal(imageName('postgres:17-alpine'), 'postgres')
})

test('finds the hub, its database and the agent from the install files', () => {
  const own = 'tainerhub'
  assert.equal(ownContainer(c('tainerhub', 'ghcr.io/prezmc/tainerhub:latest', own), own)?.kind, 'hub')
  assert.equal(ownContainer(c('tainerhub-postgres', 'postgres:17-alpine', own), own)?.kind, 'database')
  assert.equal(ownContainer(c('tainerhub-agent', 'ghcr.io/prezmc/tainerhub-agent:latest', own), own)?.kind, 'agent')
  assert.match(ownContainer(c('tainerhub-agent', 'x', own), own).what, /connects nas to TainerHub/)
})

test('finds renamed ones by image or by the agent’s compose project', () => {
  assert.equal(ownContainer(c('th', 'ghcr.io/prezmc/tainerhub:1.4'))?.kind, 'hub')
  assert.equal(ownContainer(c('agent', 'ghcr.io/prezmc/tainerhub-agent:1.4'))?.kind, 'agent')
  assert.equal(ownContainer(c('db', 'postgres:17', 'th'), 'th')?.kind, 'database')
  assert.equal(ownContainer(c('proxy', 'caddy:2', 'th'), 'th')?.kind, 'other')
})

test('leaves other containers alone', () => {
  assert.equal(ownContainer(c('postgres', 'postgres:17', 'app'), 'tainerhub-agent'), null)
  assert.equal(ownContainer(c('web', 'nginx', undefined), undefined), null)
  assert.equal(ownContainer(c('web', 'nginx', ''), ''), null)
})

test('refused only when the agent guards it: in the agent’s own compose project', () => {
  // On the hub's server everything is one project: the agent refuses.
  assert.equal(ownContainer(c('tainerhub', 'ghcr.io/prezmc/tainerhub', 'tainerhub'), 'tainerhub').refused, true)
  // The hub seen from an agent installed separately: it would go through.
  assert.equal(ownContainer(c('tainerhub', 'ghcr.io/prezmc/tainerhub', 'tainerhub'), 'tainerhub-agent').refused, false)
  // The agent didn't say which project it runs in.
  assert.equal(ownContainer(c('tainerhub-agent', 'ghcr.io/prezmc/tainerhub-agent', 'tainerhub-agent'), undefined).refused, false)
  assert.equal(ownContainer(c('tainerhub-agent', 'ghcr.io/prezmc/tainerhub-agent'), '').refused, false)
})

test('consequences say what breaks', () => {
  const agent = ownContainer(c('tainerhub-agent', 'x'))
  assert.match(ownConsequence(agent, 'nas', true), /nas is cut off.*new join token/)
  assert.doesNotMatch(ownConsequence(agent, 'nas', false), /join token/)
  const hub = ownContainer(c('tainerhub', 'x'))
  assert.match(ownConsequence(hub, 'nas', false), /every host/)
  const db = ownContainer(c('tainerhub-postgres', 'x'))
  assert.match(ownConsequence(db, 'nas', true), /every user, host and stack/)
})

test('server commands do it on purpose and bring it back', () => {
  const o = ownContainer(c('tainerhub', 'x', 'tainerhub'), 'tainerhub')
  assert.deepEqual(ownServerCommands({ name: 'tainerhub' }, 'stop', o), { run: 'docker stop tainerhub', back: 'docker compose -p tainerhub up -d' })
  assert.equal(ownServerCommands({ name: 'tainerhub' }, 'remove', o).run, 'docker rm -f tainerhub')
  const loose = ownContainer(c('tainerhub-agent', 'x'))
  assert.equal(ownServerCommands({ name: 'tainerhub-agent' }, 'kill', loose).back, 'docker start tainerhub-agent')
})

test('ownResource spots TainerHub’s volumes and networks', () => {
  assert.equal(ownResource({ name: 'tainerhub_tainerhub-data', usedBy: [] }), true)
  assert.equal(ownResource({ name: 'tainerhub-agent_tainerhub-agent', usedBy: [] }), true)
  assert.equal(ownResource({ name: 'data', project: 'th', usedBy: [] }, 'th'), true)
  assert.equal(ownResource({ name: 'th_db', usedBy: ['tainerhub-postgres'] }), true)
  assert.equal(ownResource({ name: 'app_data', project: 'app', usedBy: ['web'] }, 'th'), false)
  assert.equal(ownResource({ name: 'mytainerhubish', usedBy: [] }), false)
})
