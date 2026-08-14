import { rmSync } from 'node:fs'
import { build } from 'esbuild'

rmSync('dist', { recursive: true, force: true })

await build({
  entryPoints: ['src/cli.ts'],
  outfile: 'dist/host-gateway.mjs',
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node20',
  sourcemap: true,
  legalComments: 'linked',
  external: ['node-pty'],
  banner: {
    js: `// Bundled Pie Relay adapter using the vendored mobile transport and E2EE stack.
import { createRequire as __nodeCreateRequire } from 'node:module';
const require = __nodeCreateRequire(import.meta.url);`
  }
})
