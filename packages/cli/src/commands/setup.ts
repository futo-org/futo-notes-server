import { Command } from 'commander'
import { randomBytes } from 'node:crypto'
import { loadConfig, saveConfig, type CliConfig } from '../lib/config.ts'
import { checkDocker, composePull, composeUp, removeStaleStonefruitContainers, writeCompose } from '../lib/docker.ts'
import { waitForHealthy } from '../lib/api.ts'
import { askPort } from '../lib/prompt.ts'

const DEFAULT_PORT = 3005

export const setup = new Command('setup')
  .description('Set up a self-hosted Stonefruit server')
  .option('--port <number>', 'server port', String(DEFAULT_PORT))
  .option('--yes', 'skip interactive prompts (use defaults or provided flags)')
  .action(async (opts) => {
    const workDir = process.cwd()
    const existing = await loadConfig(workDir)

    let port: number
    if (existing) {
      // Re-running setup — reuse existing config, just upgrade + restart.
      port = existing.port
      console.log()
      console.log(`  Existing Stonefruit install found in ${workDir}`)
      console.log(`  Upgrading and restarting on port ${port}...`)
      console.log()
    } else if (opts.yes) {
      port = Number(opts.port)
    } else {
      console.log()
      console.log('  Stonefruit Server Setup')
      console.log('  -----------------------')
      console.log()
      port = await askPort(DEFAULT_PORT)
    }

    // Check Docker
    process.stdout.write('  Checking Docker... ')
    const dockerVersion = await checkDocker()
    console.log(`Docker ${dockerVersion}`)

    const config: CliConfig = existing ?? {
      version: 1,
      port,
      data_path: workDir,
      postgres_password: randomBytes(16).toString('hex'),
    }

    if (!existing) {
      await writeCompose(workDir, config)
      await saveConfig(workDir, config)
      console.log('  Wrote docker-compose.yml')
    }

    // Remove containers from any other compose project with the same names
    // so `docker compose up` doesn't fail with "container name in use".
    await removeStaleStonefruitContainers(workDir)

    // Pull + start
    process.stdout.write('  Pulling images... ')
    await composePull(workDir)
    console.log('done')

    process.stdout.write('  Starting containers... ')
    await composeUp(workDir)
    console.log('done')

    // Wait for health
    const baseUrl = `http://localhost:${port}`
    process.stdout.write('  Waiting for server... ')
    await waitForHealthy(baseUrl, 60000)
    console.log('healthy')

    console.log()
    console.log('  Stonefruit server is running!')
    console.log()
    console.log(`  Create your account at: ${baseUrl}/start`)
    console.log()
    console.log('  Then open Stonefruit, go to Settings > Sync, and enter:')
    console.log(`    Server URL: ${baseUrl}`)
    console.log('    Email + password you just signed up with')
    console.log()
  })
