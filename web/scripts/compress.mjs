// Writes .br and .gz copies of built assets; the hub serves them directly.
import { readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { brotliCompressSync, gzipSync, constants } from 'node:zlib'

const dir = new URL('../dist/assets/', import.meta.url).pathname
for (const f of readdirSync(dir)) {
  if (!/\.(js|css|svg|json)$/.test(f)) continue
  const buf = readFileSync(join(dir, f))
  writeFileSync(join(dir, f + '.br'), brotliCompressSync(buf, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } }))
  writeFileSync(join(dir, f + '.gz'), gzipSync(buf, { level: 9 }))
}
