import { chmod, mkdtemp, mkdir, readFile, rm, stat, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const root = await mkdtemp(join(tmpdir(), 'futo-notes-installer-'))
const bin = join(root, 'bin')
const installDir = join(root, 'install')
await mkdir(bin)

const docker = `#!/bin/sh
case "$*" in
  "compose version"|"info"|"pull "*|"compose up -d") exit 0 ;;
  *) echo "unexpected docker invocation: $*" >&2; exit 70 ;;
esac
`
const curl = `#!/bin/sh
case "$*" in
  *" -o docker-compose.yml") printf 'services: {}\\n' > docker-compose.yml ;;
esac
exit 0
`
await writeFile(join(bin, 'docker'), docker)
await writeFile(join(bin, 'curl'), curl)
await chmod(join(bin, 'docker'), 0o755)
await chmod(join(bin, 'curl'), 0o755)

const password = `dollar$ double" single' backslash\\ combo\\' space `
const result = Bun.spawnSync(['sh', 'install.sh'], {
  cwd: process.cwd(),
  env: {
    ...process.env,
    PATH: `${bin}:${process.env.PATH}`,
    FUTO_NOTES_DIR: installDir,
    FUTO_ADMIN_PASSWORD: password,
    POSTGRES_PASSWORD: 'database-secret',
  },
  stdout: 'pipe',
  stderr: 'pipe',
})

if (!result.success) {
  process.stderr.write(result.stderr.toString())
  process.exit(result.exitCode ?? 1)
}

const envPath = join(installDir, '.env')
const contents = await readFile(envPath, 'utf8')
const expected = String.raw`FUTO_NOTES_PASSWORD="dollar$$ double\" single' backslash\\ combo\\' space "`
if (!contents.split('\n').includes(expected)) {
  console.error(`Installer did not safely serialize the plaintext password.\nExpected: ${expected}\nActual:\n${contents}`)
  process.exit(1)
}
if (contents.includes('FUTO_NOTES_PASSWORD_HASH=')) {
  console.error('Installer should not require a container round-trip to hash the password')
  process.exit(1)
}

const mode = (await stat(envPath)).mode & 0o777
if (mode !== 0o600) {
  console.error(`Generated .env mode is ${mode.toString(8)}, expected 600`)
  process.exit(1)
}

for (const unsupported of ['line one\nline two', 'line one\rline two']) {
  const rejected = Bun.spawnSync(['sh', 'install.sh'], {
    cwd: process.cwd(),
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      FUTO_NOTES_DIR: join(root, 'rejected'),
      FUTO_ADMIN_PASSWORD: unsupported,
      POSTGRES_PASSWORD: 'database-secret',
    },
    stdout: 'pipe',
    stderr: 'pipe',
  })
  if (rejected.success || !rejected.stderr.toString().includes('must not contain newline')) {
    console.error('Installer did not explicitly reject a CR/LF password')
    process.exit(1)
  }
}

await rm(root, { recursive: true })
console.log('Installer writes a mode-0600, Compose-safe plaintext password and rejects CR/LF.')
