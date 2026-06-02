import pg from 'pg'
import { sql, type Transaction } from 'kysely'
import type { Database } from '../db/types.ts'
import { env } from '../env.ts'
import { log } from '../logger.ts'

const CHANNEL = 'collection_changes'

export interface ChangeEvent {
  userId: string
  collectionId: string
  currentVersion: number
}

export type ChangeHandler = (event: ChangeEvent) => void

export interface Notifier {
  /**
   * Fire a change event. Must run inside the same transaction that committed
   * the mutation — Postgres only dispatches NOTIFY on commit, so this avoids
   * the race where a subscriber pulls before the writing transaction commits.
   */
  publish(trx: Transaction<Database>, event: ChangeEvent): Promise<void>
  /** Returns an unsubscribe function. Idempotent. */
  subscribe(userId: string, handler: ChangeHandler): () => void
  start(): Promise<void>
  stop(): Promise<void>
}

/** Compact JSON payload to stay well under Postgres' 8000-byte NOTIFY limit. */
interface NotifyPayload {
  u: string
  c: string
  v: number
}

export class PostgresNotifier implements Notifier {
  private client: pg.Client | null = null
  private handlers = new Map<string, Set<ChangeHandler>>()

  async start(): Promise<void> {
    if (this.client) return
    const client = new pg.Client({
      connectionString: env.DATABASE_URL,
      ssl: env.DB_SSL ? { rejectUnauthorized: false } : undefined,
    })
    await client.connect()
    client.on('notification', (msg) => {
      if (msg.channel !== CHANNEL || !msg.payload) return
      let parsed: NotifyPayload
      try {
        parsed = JSON.parse(msg.payload) as NotifyPayload
      } catch {
        log.warn('sync notifier: invalid payload', { payload: msg.payload })
        return
      }
      const event: ChangeEvent = {
        userId: parsed.u,
        collectionId: parsed.c,
        currentVersion: parsed.v,
      }
      const set = this.handlers.get(event.userId)
      if (!set) return
      for (const handler of set) {
        try {
          handler(event)
        } catch (err) {
          log.warn('sync notifier: handler threw', { err: String(err) })
        }
      }
    })
    client.on('error', (err) => {
      log.error('sync notifier: listener error', { err: String(err) })
    })
    await client.query(`LISTEN ${CHANNEL}`)
    this.client = client
    log.info('sync notifier listening', { channel: CHANNEL })
  }

  async stop(): Promise<void> {
    this.handlers.clear()
    const client = this.client
    this.client = null
    if (!client) return
    try {
      await client.query(`UNLISTEN ${CHANNEL}`)
    } catch {}
    await client.end().catch(() => {})
  }

  async publish(trx: Transaction<Database>, event: ChangeEvent): Promise<void> {
    const payload: NotifyPayload = {
      u: event.userId,
      c: event.collectionId,
      v: event.currentVersion,
    }
    const json = JSON.stringify(payload)
    await sql`select pg_notify(${CHANNEL}, ${json})`.execute(trx)
  }

  subscribe(userId: string, handler: ChangeHandler): () => void {
    let set = this.handlers.get(userId)
    if (!set) {
      set = new Set()
      this.handlers.set(userId, set)
    }
    set.add(handler)
    return () => {
      const current = this.handlers.get(userId)
      if (!current) return
      current.delete(handler)
      if (current.size === 0) this.handlers.delete(userId)
    }
  }
}

export const notifier: Notifier = new PostgresNotifier()
