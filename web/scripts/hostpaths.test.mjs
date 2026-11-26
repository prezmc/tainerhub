// Tests for src/hostpaths.ts (recommended folders for a volume). Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { appNames, layoutOf, recommendPaths } from '../src/hostpaths.ts'

const b = (container, image, source, target) => ({ container, image, source, target })

test('app names and layouts', () => {
  assert.deepEqual(appNames({ container: 'media-sonarr-1', image: 'lscr.io/linuxserver/sonarr:4' }), ['media-sonarr-1', 'sonarr'])
  assert.deepEqual(layoutOf(b('radarr', 'linuxserver/radarr', '/srv/appdata/radarr/config', '/config')), { root: '/srv/appdata', app: 'radarr', suffix: '/config', target: '/config', container: 'radarr' })
  assert.equal(layoutOf(b('web', 'nginx', '/srv/www', '/usr/share/nginx/html')), null)
  assert.equal(layoutOf(b('x', 'x', '/x', '/data')), null) // no root above it
})

test('follows the host’s layout for the same mount point', () => {
  const binds = [
    b('sonarr', 'lscr.io/linuxserver/sonarr:latest', '/srv/appdata/sonarr/config', '/config'),
    b('sonarr', 'lscr.io/linuxserver/sonarr:latest', '/mnt/media/tv', '/tv'),
    b('radarr', 'lscr.io/linuxserver/radarr:latest', '/srv/appdata/radarr/config', '/config'),
    b('web', 'nginx', '/home/me/site', '/usr/share/nginx/html'),
    b('old', 'lscr.io/linuxserver/bazarr', '/data/bazarr', '/config'),
  ]
  const [first, second] = recommendPaths(binds, { app: 'prowlarr', target: '/config', stacksDir: '/opt/tainerhub/stacks', host: 'nas' })
  assert.equal(first.kind, 'convention')
  assert.equal(first.value, '/srv/appdata/prowlarr/config')
  assert.match(first.why, /sonarr, radarr keep their \/config on nas/)
  assert.deepEqual([second.kind, second.value, second.path], ['stack', './config', '/opt/tainerhub/stacks/prowlarr/config'])
})

test('a whole-app folder, a different mount point, and no pattern at all', () => {
  const flat = [b('sonarr', 'linuxserver/sonarr', '/docker/sonarr', '/config'), b('radarr', 'linuxserver/radarr', '/docker/radarr', '/config')]
  assert.equal(recommendPaths(flat, { app: 'prowlarr', target: '/config' })[0].value, '/docker/prowlarr')
  // other apps only show /config; a /data volume gets its own subfolder
  assert.equal(recommendPaths(flat, { app: 'prowlarr', target: '/data:ro' })[0].value, '/docker/prowlarr/data')
  const none = recommendPaths([], { app: 'Prowlarr', target: '/config' })
  assert.equal(none.length, 1)
  assert.deepEqual([none[0].value, none[0].path], ['./config', '/opt/tainerhub/stacks/prowlarr/config'])
})

test('never the folder of an app that already exists, nor TainerHub’s own stacks as a pattern', () => {
  const binds = [
    b('sonarr', 'linuxserver/sonarr', '/srv/appdata/sonarr', '/config'),
    b('prowlarr', 'linuxserver/prowlarr', '/srv/appdata/prowlarr', '/config'), // the same app, already running
    b('stack-radarr-1', 'linuxserver/radarr', '/opt/tainerhub/stacks/radarr/config', '/config'),
    b('stack-lidarr-1', 'linuxserver/lidarr', '/opt/tainerhub/stacks/lidarr/config', '/config'),
  ]
  const recs = recommendPaths(binds, { app: 'prowlarr', target: '/config', stacksDir: '/opt/tainerhub/stacks' })
  // /srv/appdata/prowlarr is in use by the running prowlarr, so only the stack folder is left
  assert.deepEqual(recs.map((r) => r.kind), ['stack'])
})
