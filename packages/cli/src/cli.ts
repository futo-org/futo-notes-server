import { program } from 'commander'
import { setup } from './commands/setup.ts'
import { status } from './commands/status.ts'
import { update } from './commands/update.ts'
import { resetPasswordCmd } from './commands/reset-password.ts'

program
  .name('stonefruit')
  .description('Self-hosted Stonefruit server management CLI')
  .version('0.0.1')

program.addCommand(setup)
program.addCommand(status)
program.addCommand(update)
program.addCommand(resetPasswordCmd)

program.parse()
