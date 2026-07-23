import { readFileSync } from 'node:fs'

const compose = readFileSync('docker-compose.production.yml', 'utf8')
const example = readFileSync('.env.production.example', 'utf8')

const composeOnly = new Set([
  'POSTGRES_PASSWORD',
  'FUTO_NOTES_DATA_DIR',
  'FUTO_NOTES_PORT',
  'FUTO_NOTES_IMAGE',
])
const documented = [...example.matchAll(/^#?([A-Z][A-Z0-9_]+)=/gm)].map((match) => match[1])
const forwarded = documented.filter((name) => !composeOnly.has(name))

const missing = forwarded.filter((name) => !new RegExp(`^\\s{6}${name}:`, 'm').test(compose))
if (missing.length > 0) {
  console.error(`Production Compose does not forward: ${missing.join(', ')}`)
  process.exit(1)
}

if (!/^\s{6}DATABASE_URL:\s*postgres:\/\/futo_notes:\$\{POSTGRES_PASSWORD:\?/m.test(compose)) {
  console.error('Production Compose must use its bundled Postgres service')
  process.exit(1)
}

console.log(`Production Compose forwards all ${forwarded.length} supported settings.`)
