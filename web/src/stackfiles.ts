// A stack's extra files (Dockerfile, scripts, config): the rules the hub
// checks, mirrored here so the editor can say what's wrong before saving.

export interface StackFile {
  path: string
  content: string
  executable?: boolean
}

export const MAX_FILES = 30
export const MAX_FILE_BYTES = 256 << 10
export const MAX_TOTAL_BYTES = 2 << 20

const RESERVED = new Set(['compose.yaml', 'compose.yml', 'docker-compose.yaml', 'docker-compose.yml', '.env'])
const SEGMENT = /^[A-Za-z0-9._-]+$/

/** Why a path can't be a stack file (null if it can). `others` are the stack's other files. */
export function pathProblem(path: string, others: string[] = []): string | null {
  if (path === '') return 'Enter a name.'
  if (path.length > 200) return 'Keep the path under 200 characters.'
  if (path.startsWith('/')) return "Use a path inside the stack's folder (no leading /)."
  if (RESERVED.has(path)) return `${path} has its own tab.`
  const segs = path.split('/')
  if (segs.length > 4) return 'Folders go at most 3 levels deep.'
  for (const s of segs) {
    if (s === '') return "The path has an empty part ('//' or a trailing '/')."
    if (s === '.' || s === '..') return "'.' and '..' aren't allowed."
    if (s.startsWith('.tainerhub')) return 'Names starting with .tainerhub are reserved.'
    if (!SEGMENT.test(s)) return "Use letters, digits, '.', '_' and '-', with '/' between folders."
  }
  for (const o of others) {
    if (o === path) return `The stack already has ${path}.`
    if (o.startsWith(path + '/')) return `${path} is a folder of ${o}.`
    if (path.startsWith(o + '/')) return `${o} is a file, so it can't hold ${path}.`
  }
  return null
}

const bytes = (s: string) => new TextEncoder().encode(s).length

/** What's wrong with the files as a whole (null if nothing). */
export function filesProblem(files: StackFile[]): string | null {
  if (files.length > MAX_FILES) return `A stack can have at most ${MAX_FILES} extra files.`
  let total = 0
  for (const f of files) {
    const n = bytes(f.content)
    if (n > MAX_FILE_BYTES) return `${f.path} is larger than 256 KB.`
    if (f.content.includes('\0')) return `${f.path} isn't text.`
    total += n
  }
  if (total > MAX_TOTAL_BYTES) return 'The extra files are larger than 2 MB together.'
  return null
}

export function filesEqual(a: StackFile[], b: StackFile[]): boolean {
  if (a.length !== b.length) return false
  return a.every((f, i) => f.path === b[i].path && f.content === b[i].content && !!f.executable === !!b[i].executable)
}

/** Whether the compose file has a build: section (a quick look, not a YAML parser). */
export function composeBuilds(compose: string): boolean {
  return /^\s*(-\s+)?build\s*:/m.test(compose)
}

/** Whether any build: points at a Git repository (it brings its own Dockerfile). */
export function composeBuildsFromGit(compose: string): boolean {
  return /^\s*(build|context)\s*:\s*["']?(https?:\/\/|git@|git:\/\/)/m.test(compose)
}

export function isDockerfile(path: string): boolean {
  const base = path.split('/').pop() ?? ''
  return base === 'Dockerfile' || base.endsWith('.Dockerfile') || base.startsWith('Dockerfile.')
}

/** Shell scripts are usually meant to run. */
export const looksExecutable = (path: string) => path.endsWith('.sh')

export interface FileTemplate {
  id: string
  label: string
  path: string
  content: string
  executable?: boolean
}

export const TEMPLATES: FileTemplate[] = [
  {
    id: 'dockerfile',
    label: 'Dockerfile',
    path: 'Dockerfile',
    content: `# Built for services with "build: ." (the stack's folder is the build context).
# Docker's classic builder runs it: plain Dockerfile syntax, no RUN --mount.
# Copy files by name: "COPY . ." would also copy .env into the image.
FROM alpine:3.20
WORKDIR /app
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
CMD ["/usr/local/bin/entrypoint.sh"]
`,
  },
  {
    id: 'entrypoint',
    label: 'entrypoint.sh',
    path: 'entrypoint.sh',
    executable: true,
    content: `#!/bin/sh
# Runs when the container starts.
set -e
echo "Starting on $(hostname)"
# Replace with your program. exec lets it receive the stop signal.
exec sleep infinity
`,
  },
  { id: 'empty', label: 'Empty file', path: '', content: '' },
]
