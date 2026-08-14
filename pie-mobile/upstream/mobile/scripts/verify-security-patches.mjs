import { createRequire } from 'node:module'
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

const root = join(import.meta.dirname, '..')
const virtualStore = join(root, 'node_modules', '.pnpm')
const candidates = readdirSync(virtualStore).filter((name) =>
  name.startsWith('brace-expansion@1.1.16_patch_hash='),
)
const active = candidates.find((name) => {
  const source = join(virtualStore, name, 'node_modules', 'brace-expansion', 'index.js')
  return readFileSync(source, 'utf8').includes('MAX_BRACE_GROUPS')
})

if (!active) {
  throw new Error('The brace-expansion 1.x resource-limit backport is not installed')
}

const packageRoot = join(virtualStore, active, 'node_modules', 'brace-expansion')
const require = createRequire(import.meta.url)
const expand = require(packageRoot)
const normal = expand('{a,b}')
if (normal.join(',') !== 'a,b') {
  throw new Error(`The brace-expansion compatibility contract changed: ${normal}`)
}

const pathological = '{a,b}'.repeat(65)
const limited = expand(pathological)
if (limited.length !== 1 || limited[0] !== pathological) {
  throw new Error('The brace-expansion recursion limit is not active')
}

const bounded = expand('{a,b}'.repeat(20))
if (bounded.length > 10_000) {
  throw new Error(`The brace-expansion result limit was bypassed: ${bounded.length}`)
}
