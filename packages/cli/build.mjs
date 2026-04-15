import { build } from 'esbuild'
import { chmodSync } from 'node:fs'

await build({
  entryPoints: ['src/cli.ts'],
  bundle: true,
  platform: 'node',
  target: 'node24',
  format: 'esm',
  outfile: 'dist/cli.js',
  // Shebang + createRequire shim so bundled CommonJS deps (commander et al)
  // can still call require() at runtime under ESM.
  banner: {
    js: `#!/usr/bin/env node
import { createRequire as __createRequire } from 'node:module';
const require = __createRequire(import.meta.url);`,
  },
})

chmodSync('dist/cli.js', 0o755)
