import { build } from 'esbuild'

const hosted = process.argv.includes('--hosted')
const entryPoint = hosted ? 'src/hosted/index.ts' : 'src/index.ts'
const outfile = hosted ? 'dist/hosted.js' : 'dist/index.js'

await build({
  entryPoints: [entryPoint],
  bundle: true,
  platform: 'node',
  target: 'node24',
  format: 'esm',
  outfile,
  external: ['pg', 'pg-native'],
  // createRequire shim so bundled CommonJS deps can still call require() under ESM.
  banner: {
    js: `import { createRequire as __createRequire } from 'node:module';
const require = __createRequire(import.meta.url);`,
  },
})
