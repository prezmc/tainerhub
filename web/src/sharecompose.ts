// Adding a network share volume to a stack's compose file. Separate from
// shares.ts so pages that only list shares don't load the YAML parser.
import { isMap, parseDocument } from 'yaml'

/**
 * Adds `name: {external: true}` under the compose file's top-level
 * volumes (creating the section if needed), keeping the rest of the file
 * and its comments. Returns the file unchanged if the entry is there, and
 * null if the file can't be parsed.
 */
export function addExternalVolume(compose: string, name: string): string | null {
  const doc = parseDocument(compose || '')
  if (doc.errors.length) return null
  if (doc.contents !== null && !isMap(doc.contents)) return null
  const vols = doc.get('volumes')
  if (vols !== undefined && vols !== null && !isMap(vols)) return null
  if (isMap(vols) && vols.has(name)) return compose
  doc.setIn(['volumes', name], doc.createNode({ external: true }))
  return doc.toString()
}
