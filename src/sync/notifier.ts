import pg from 'pg'
import { sql, type Transaction } from 'kysely'
import type { Database } from '../db/types.ts'
import { env } from '../env.ts'
import { log } from '../logger.ts'

const CHANNEL = 'collection_changes'
const APP_NAME = 'futo-notes-notifier'

// Reconnect backoff bounds for the dedicated LISTEN connection.
const INITIAL_RECONNECT_MS = 1_000
const MAX_RECONNECT_MS = 30_000

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
  /**
   * Register a callback fired when the upstream LISTEN connection drops. While
   * it is down no NOTIFYs are delivered, so open streams should treat this as
   * "you are now stale" — close and let the client reconnect + re-pull. Returns
   * an unsubscribe function.
   */
  onListenerLost(handler: () => void): () => void
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
  private lostHandlers = new Set<() => void>()
  private stopped = false
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private reconnectDelayMs = INITIAL_RECONNECT_MS

  async start(): Promise<void> {
    this.stopped = false
    if (this.client) return
    await this.connect()
  }

  private dispatch(msg: pg.Notification): void {
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
  }

  private async connect(): Promise<void> {
    const client = new pg.Client({
      connectionString: env.DATABASE_URL,
      ssl: env.DB_SSL_OPTIONS,
      application_name: APP_NAME,
    })
    client.on('notification', (msg) => this.dispatch(msg))
    // 'error' and 'end' both signal a dead connection; handleLoss is made
    // idempotent by removeAllListeners so whichever fires first wins.
    client.on('error', (err) => {
      log.error('sync notifier: listener error', { err: String(err) })
      this.handleLoss(client)
    })
    client.on('end', () => this.handleLoss(client))

    try {
      await client.connect()
      await client.query(`LISTEN ${CHANNEL}`)
    } catch (err) {
      log.error('sync notifier: connect failed', { err: String(err) })
      this.handleLoss(client)
      return
    }
    this.client = client
    this.reconnectDelayMs = INITIAL_RECONNECT_MS
    log.info('sync notifier listening', { channel: CHANNEL })
  }

  /** Tear down a dead connection and schedule a reconnect. Idempotent. */
  private handleLoss(client: pg.Client): void {
    client.removeAllListeners()
    client.end().catch(() => {})
    if (this.client === client) this.client = null
    for (const handler of this.lostHandlers) {
      try {
        handler()
      } catch (err) {
        log.warn('sync notifier: lost-handler threw', { err: String(err) })
      }
    }
    this.scheduleReconnect()
  }

  private scheduleReconnect(): void {
    if (this.stopped || this.reconnectTimer || this.client) return
    const delay = this.reconnectDelayMs
    this.reconnectDelayMs = Math.min(this.reconnectDelayMs * 2, MAX_RECONNECT_MS)
    log.warn('sync notifier: scheduling reconnect', { delayMs: delay })
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      if (this.stopped) return
      void this.connect()
    }, delay)
  }

  async stop(): Promise<void> {
    this.stopped = true
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.handlers.clear()
    this.lostHandlers.clear()
    const client = this.client
    this.client = null
    if (!client) return
    client.removeAllListeners()
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

  onListenerLost(handler: () => void): () => void {
    this.lostHandlers.add(handler)
    return () => {
      this.lostHandlers.delete(handler)
    }
  }
}

export const notifier: Notifier = new PostgresNotifier()
