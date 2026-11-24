// Stack rollback: small helpers for the preview and the banner (kept free
// of React so `npm test` can check them).

import type { RollbackImage, RollbackPreview, RollbackTrigger } from './api'

const triggers: Record<RollbackTrigger, string> = {
  deploy: 'before a deploy',
  pull: 'before a Pull & redeploy',
  auto: 'before an automatic update',
  rollback: 'before a rollback',
}

/** "before a Pull & redeploy" */
export const triggerText = (t: RollbackTrigger | string): string => triggers[t as RollbackTrigger] ?? `before a ${t}`

/** The short image ID ("3f2a9c1b0d4e"), or '' when unknown. */
export const shortId = (id?: string): string => (id ? id.replace(/^sha256:/, '').slice(0, 12) : '')

/** One line for an image: "nginx:1.27 · 3f2a9c1b0d4e". */
export function imageText(im: RollbackImage | null | undefined): string {
  if (!im) return 'not running'
  const id = shortId(im.imageId)
  if (!im.ref) return id || 'unknown'
  return id ? `${im.ref} · ${id}` : im.ref
}

/** How many services a rollback would change (those not already on the old image). */
export const changedServices = (p: RollbackPreview): number => p.services.filter((s) => !s.same).length

/** Tags under tainerhub-rollback/ keep images for rollbacks; they are not names to pull. */
export const isRollbackTag = (tag: string): boolean => tag.startsWith('tainerhub-rollback/')

/** The tag to show for an image: its own name before a rollback tag. */
export const displayTag = (tags: string[]): string | undefined => tags.find((t) => !isRollbackTag(t)) ?? tags[0]
