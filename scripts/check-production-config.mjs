import { readFileSync } from 'node:fs'

const compose = readFileSync('docker-compose.production.yml', 'utf8')
const example = readFileSync('.env.production.example', 'utf8')

const composeOnly = new Set([
  'POSTGRES_PASSWORD',
  'FUTO_NOTES_DATA_DIR',
  'FUTO_NOTES_CONFIG_DIR',
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

if (!/^\s{6}DATABASE_URL:\s*\$\{DATABASE_URL:-/m.test(compose)) {
  console.error('Production Compose DATABASE_URL must be overridable with a local default')
  process.exit(1)
}

if (!compose.includes('${FUTO_NOTES_CONFIG_DIR:-./config}:/config:ro')) {
  console.error('Production Compose must mount the documented config directory at /config')
  process.exit(1)
}

console.log(`Production Compose forwards all ${forwarded.length} supported settings.`)
