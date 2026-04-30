import { buildApp } from '../app.ts'
import { log } from '../logger.ts'
import { runCliSubcommand, runServer } from '../server.ts'

// Hosted entrypoint. Starts the same app as OSS; hosted-only middleware
// (billing, analytics, etc.) will wire in here as it's added.
function buildHostedApp() {
  const app = buildApp()
  // e.g. app.use('/api/billing/*', billingMiddleware)
  return app
}

if (import.meta.url === `file://${process.argv[1]}`) {
  runCliSubcommand()
    .then(() => runServer(buildHostedApp(), 'futo-notes (hosted)'))
    .catch((err) => {
      log.error('fatal startup error', { error: String(err) })
      process.exit(1)
    })
}
