// Hands a compose file from New container ("deploy the whole file as a
// stack") to the New stack page. Kept in memory only: it lives as long as
// the tab, and a reload simply starts the stack editor empty.

export type StackDraft = { name: string; compose: string; env: string }

let draft: StackDraft | null = null

export const setStackDraft = (d: StackDraft) => {
  draft = d
}
export const peekStackDraft = () => draft
export const clearStackDraft = () => {
  draft = null
}
