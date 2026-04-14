import { Command } from 'commander'
import { randomBytes } from 'node:crypto'
import { saveConfig, type CliConfig } from '../lib/config.ts'
import { checkDocker, composePull, composeUp, writeCompose } from '../lib/docker.ts'
import { setupServer, waitForHealthy } from '../lib/api.ts'
import { askPassword, askPort, readPasswordStdin } from '../lib/prompt.ts'

const DEFAULT_PORT = 3000

export const setup = new Command('setup')
  .description('Set up a self-hosted Stonefruit server')
  .option('--port <number>', 'server port', String(DEFAULT_PORT))
  .option('--password <string>', 'initial server password (min 8 chars)')
  .option('--password-stdin', 'read password from stdin')
  .option('--yes', 'skip interactive prompts (requires --password or --password-stdin)')
  .action(async (opts) => {
    const nonInteractive = opts.yes || opts.password || opts.passwordStdin

    let port: number
    let password: string

    if (nonInteractive) {
      port = Number(opts.port)
      if (opts.passwordStdin) {
        password = await readPasswordStdin()
      } else if (opts.password) {
        password = opts.password
      } else {
        console.error('Error: --yes requires --password or --password-stdin')
        process.exit(1)
      }
    } else {
      console.log()
      console.log('  Stonefruit Server Setup')
      console.log('  -----------------------')
      console.log()
      port = await askPort(DEFAULT_PORT)
      password = await askPassword()
    }

    if (password.length < 8) {
      console.error('Error: password must be at least 8 characters.')
      process.exit(1)
    }

    // Check Docker
    process.stdout.write('  Checking Docker... ')
    const dockerVersion = await checkDocker()
    console.log(`Docker ${dockerVersion}`)

    const workDir = process.cwd()
    const postgresPassword = randomBytes(16).toString('hex')

    const config: CliConfig = {
      version: 1,
      port,
      data_path: './stonefruit-data',
      postgres_password: postgresPassword,
    }

    // Write compose + config
    await writeCompose(workDir, config)
    await saveConfig(workDir, config)
    console.log('  Wrote docker-compose.yml')

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

    // Set password
    process.stdout.write('  Setting password... ')
    const { admin_token } = await setupServer(baseUrl, password)
    console.log('done')

    // Save admin token
    config.data_path = workDir
    await saveConfig(workDir, config)

    console.log()
    console.log('  Stonefruit server is running!')
    console.log(`    Server:  ${baseUrl}`)
    console.log(`    Admin token: ${admin_token}`)
    console.log()
    console.log('  Next steps:')
    console.log('    1. Open Stonefruit on your phone or computer')
    console.log('    2. Go to Settings > Sync')
    console.log('    3. Enter the server URL and your password')
    console.log()
    console.log('  Save your admin token — you need it to reset the password.')
    console.log()
  })
