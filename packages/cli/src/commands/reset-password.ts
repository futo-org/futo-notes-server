import { Command } from 'commander'
import { loadConfig } from '../lib/config.ts'
import { resetPassword } from '../lib/api.ts'
import { askPassword, readPasswordStdin } from '../lib/prompt.ts'

export const resetPasswordCmd = new Command('reset-password')
  .description('Reset the server password (requires admin token)')
  .option('--base-url <url>', 'server URL')
  .option('--admin-token <token>', 'admin token from setup')
  .option('--password <string>', 'new password (min 8 chars)')
  .option('--password-stdin', 'read new password from stdin')
  .action(async (opts) => {
    const config = await loadConfig(process.cwd())

    let baseUrl = opts.baseUrl
    if (!baseUrl) {
      baseUrl = config ? `http://localhost:${config.port}` : 'http://localhost:3000'
    }

    const adminToken = opts.adminToken
    if (!adminToken) {
      console.error('Error: --admin-token is required. This was printed during setup.')
      process.exit(1)
    }

    let password: string
    if (opts.passwordStdin) {
      password = await readPasswordStdin()
    } else if (opts.password) {
      password = opts.password
    } else {
      password = await askPassword()
    }

    if (password.length < 8) {
      console.error('Error: password must be at least 8 characters.')
      process.exit(1)
    }

    await resetPassword(baseUrl, adminToken, password)
    console.log('  Password reset successfully. All existing sessions have been revoked.')
  })
