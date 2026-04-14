import { promises as fs } from 'node:fs'
import path from 'node:path'
import type { BlobStore } from './interface.ts'

/**
 * Filesystem-backed BlobStore. Writes blobs under `baseDir` using the
 * key as a relative path. Keys may contain forward slashes (e.g. `uid/blobId`)
 * — each path segment becomes a directory.
 */
export class FsBlobStore implements BlobStore {
  constructor(private readonly baseDir: string) {}

  private resolve(key: string): string {
    // Safety: reject path traversal. Keys are server-generated from uuids,
    // but defense in depth is cheap.
    if (key.includes('..') || path.isAbsolute(key)) {
      throw new Error(`Invalid blob key: ${key}`)
    }
    return path.join(this.baseDir, key)
  }

  async put(key: string, data: Uint8Array): Promise<void> {
    const full = this.resolve(key)
    await fs.mkdir(path.dirname(full), { recursive: true })
    await fs.writeFile(full, data)
  }

  async get(key: string): Promise<Uint8Array | null> {
    try {
      const buf = await fs.readFile(this.resolve(key))
      return new Uint8Array(buf)
    } catch (err: unknown) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') return null
      throw err
    }
  }

  async delete(key: string): Promise<void> {
    try {
      await fs.unlink(this.resolve(key))
    } catch (err: unknown) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') return
      throw err
    }
  }

  async list(prefix: string): Promise<string[]> {
    const base = this.resolve(prefix)
    try {
      const entries = await fs.readdir(base, { withFileTypes: true, recursive: true })
      return entries
        .filter((e) => e.isFile())
        .map((e) => path.relative(this.baseDir, path.join(e.parentPath, e.name)))
    } catch (err: unknown) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') return []
      throw err
    }
  }
}
