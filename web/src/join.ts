// Helpers for the Add host dialog's one-line installer command.

/** The command a new server runs to download and start its agent. */
export function joinCommand(base: string, token: string, agentAddr: string): string {
  return `curl -fsSL "${base.replace(/\/+$/, '')}/join/${token}?hub=${encodeURIComponent(agentAddr)}" | sh`
}

/**
 * The hub's direct web address, when it differs from the page's own: the page
 * is then usually served through a reverse proxy, whose access list may answer
 * a server on another subnet with 403. Returns null when there is no choice.
 */
export function directAlternative(pageOrigin: string, directURL: string | undefined): string | null {
  if (!directURL) return null
  try {
    return new URL(directURL).origin === new URL(pageOrigin).origin ? null : new URL(directURL).origin
  } catch {
    return null
  }
}
