import { Command } from 'commander'
import { loadConfig } from '../lib/config.ts'
import { composePull, composeUpRecreate, getContainerImageId } from '../lib/docker.ts'
import { waitForHealthy } from '../lib/api.ts'

export const update = new Command('update')
  .description('Pull latest server image and restart')
  .option('--compose-dir <path>', 'directory with docker-compose.yml', '.')
  .action(async (opts) => {
    const workDir = opts.composeDir
    const config = await loadConfig(workDir)
    const port = config?.port ?? 3000
    const baseUrl = `http://localhost:${port}`

    const start = Date.now()

    // Snapshot current image
    const oldImage = await getContainerImageId('stonefruit')

    process.stdout.write('  Pulling latest image... ')
    await composePull(workDir)
    console.log('done')

    process.stdout.write('  Restarting container... ')
    await composeUpRecreate(workDir)
    console.log('done')

    process.stdout.write('  Waiting for server... ')
    await waitForHealthy(baseUrl, 60000)
    console.log('healthy')

    const newImage = await getContainerImageId('stonefruit')
    const elapsed = Math.round((Date.now() - start) / 1000)

    console.log()
    if (oldImage && newImage && oldImage !== newImage) {
      console.log('  Updated to a new version.')
      console.log(`    Image: ${oldImage.slice(0, 12)} -> ${newImage.slice(0, 12)}`)
    } else {
      console.log('  Already up to date.')
      if (newImage) console.log(`    Image: ${newImage.slice(0, 12)}`)
    }
    console.log(`  Done in ${elapsed}s.`)
    console.log()
  })
