import { buildApp } from './app.ts'
import { log } from './logger.ts'
import { runCliSubcommand, runServer } from './server.ts'

if (import.meta.url === `file://${process.argv[1]}`) {
  runCliSubcommand()
    .then(() => runServer(buildApp()))
    .catch((err) => {
      log.error('fatal startup error', { error: String(err) })
      process.exit(1)
    })
}
