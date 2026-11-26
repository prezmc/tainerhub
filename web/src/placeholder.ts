// Sample values from project READMEs that are meant to be replaced:
// "/path/to/data", "/PATH_TO_YOUR_CONFIG", "changeme", "<your-domain>".
// Same rules as internal/wire/placeholder.go. Dependency-free for the tests.

const ANGLE = /<[^<>]{1,60}>/
const PATH_TO = /(^|[^a-z])path[ _/-]?to([^a-z]|$)/i
const YOUR =
  /(^|[^a-z])your([^a-z]|$)|(^|[^a-z])your(password|pass|domain|email|mail|token|key|secret|user|username|name|host|server|ip|path|dir|folder|config|data|media|timezone|api)/i
const WORDS = /(^|[^a-z])(change[_-]?me|change[_-]?this|replace[_-]?me|replace[_-]?with|replace[_-]?this|fill[_-]?me|insert[_-]?here|todo|xxxx+)([^a-z]|$)/i

/** Whether s looks like a sample value from documentation rather than a real one. */
export function looksLikePlaceholder(s: string): boolean {
  const t = s.trim()
  if (!t || t.length > 500) return false
  return ANGLE.test(t) || PATH_TO.test(t) || YOUR.test(t) || WORDS.test(t)
}

/** A folder inside the stack's own folder for a volume mounted at target: "/config:ro" -> "./config". */
export function suggestSource(target: string): string {
  const path = target.split(':')[0]
  const last = path.split('/').filter(Boolean).pop() ?? ''
  const clean = last.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[-.]+/, '')
  return './' + (clean || 'data')
}

/** Whether an environment variable holds a secret, so a random value is a good default. */
export const isSecretKey = (key: string) => /pass(word)?|secret|token|salt|api[_-]?key|private[_-]?key|(^|_)key$/i.test(key)

/** A random value for a password or secret: 32 letters and digits. */
export function generateSecret(length = 32): string {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'
  const out: string[] = []
  const buf = new Uint32Array(length * 2)
  while (out.length < length) {
    globalThis.crypto.getRandomValues(buf)
    for (const n of buf) {
      // reject the top of the range so every character is equally likely
      if (n < Math.floor(0x100000000 / chars.length) * chars.length) out.push(chars[n % chars.length])
      if (out.length === length) break
    }
  }
  return out.join('')
}
