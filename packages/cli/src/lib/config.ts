import { readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'

export interface CliConfig {
  version: number
  port: number
  data_path: string
  postgres_password: string
}

const CONFIG_FILE = '.stonefruit-cli.json'

export function configPath(dir: string): string {
  return path.join(dir, CONFIG_FILE)
}

export async function loadConfig(dir: string): Promise<CliConfig | null> {
  try {
    const raw = await readFile(configPath(dir), 'utf-8')
    return JSON.parse(raw) as CliConfig
  } catch {
    return null
  }
}

export async function saveConfig(dir: string, config: CliConfig): Promise<void> {
  await writeFile(configPath(dir), JSON.stringify(config, null, 2) + '\n')
}
