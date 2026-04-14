import { Command } from 'commander'
import { loadConfig } from '../lib/config.ts'
import { checkHealth } from '../lib/api.ts'

export const status = new Command('status')
  .description('Check server health and status')
  .option('--base-url <url>', 'server URL')
  .option('--json', 'output JSON')
  .action(async (opts) => {
    let baseUrl = opts.baseUrl
    if (!baseUrl) {
      const config = await loadConfig(process.cwd())
      baseUrl = config ? `http://localhost:${config.port}` : 'http://localhost:3000'
    }

    const health = await checkHealth(baseUrl)

    if (opts.json) {
      console.log(JSON.stringify({ url: baseUrl, health }, null, 2))
      return
    }

    if (!health) {
      console.log(`  Stonefruit server at ${baseUrl}`)
      console.log('  Status: unreachable')
      process.exit(1)
    }

    console.log()
    console.log('  Stonefruit server status')
    console.log(`    URL:            ${baseUrl}`)
    console.log(`    Health:         ${health.status}`)
    console.log(`    Database:       ${health.db}`)
    console.log(`    Setup complete: ${health.setup_complete ? 'yes' : 'no'}`)
    console.log()
  })
