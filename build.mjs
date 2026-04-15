import { build } from 'esbuild'

await build({
  entryPoints: ['src/index.ts'],
  bundle: true,
  platform: 'node',
  target: 'node24',
  format: 'esm',
  outfile: 'dist/index.js',
  external: ['pg', 'pg-native', 'dotenv'],
  // createRequire shim so bundled CommonJS deps can still call require() under ESM.
  banner: {
    js: `import { createRequire as __createRequire } from 'node:module';
const require = __createRequire(import.meta.url);`,
  },
})
