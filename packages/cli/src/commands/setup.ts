import { Command } from 'commander'
import { randomBytes } from 'node:crypto'
import { loadConfig, saveConfig, type CliConfig } from '../lib/config.ts'
import { checkDocker, composePull, composeUp, writeCompose } from '../lib/docker.ts'
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
    console.log(`    Server: ${baseUrl}`)
    console.log()
    console.log('  Next steps:')
    console.log(`    1. Open ${baseUrl}/start in your browser to create your account`)
    console.log('    2. Open Stonefruit on your phone or computer')
    console.log('    3. Go to Settings > Sync and enter the server URL, email, and password')
    console.log()
  })
