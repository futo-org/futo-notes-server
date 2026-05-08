import { createHash, randomBytes } from 'node:crypto'
import { uuidv7 } from 'uuidv7'
import { db } from '../db/connection.ts'

export const SESSION_TTL_MS = 7 * 24 * 60 * 60 * 1000

/**
 * Slide the expiry forward when less than half the TTL remains. An active
 * client causes at most one renewal per ~3.5 days; a fully idle session still
 * dies after 7 days.
 */
const SESSION_RENEW_THRESHOLD_MS = SESSION_TTL_MS / 2

/** 32 random bytes, hex-encoded. The raw token only lives in the client cookie. */
export function generateToken(): string {
  return randomBytes(32).toString('hex')
}

/** SHA-256 of the raw token, returned as a Buffer for Postgres bytea. */
export function hashToken(raw: string): Buffer {
  return createHash('sha256').update(raw).digest()
}

export interface CreatedSession {
  sessionId: string
  rawToken: string
  expiresAt: Date
}

export async function createSession(userId: string): Promise<CreatedSession> {
  const rawToken = generateToken()
  const hash = hashToken(rawToken)
  const expiresAt = new Date(Date.now() + SESSION_TTL_MS)
  const sessionId = uuidv7()

  await db
    .insertInto('sessions')
    .values({
      id: sessionId,
      user_id: userId,
      access_token_hash: hash,
      expires_at: expiresAt,
    })
    .execute()

  return { sessionId, rawToken, expiresAt }
}

export interface ValidatedSession {
  sessionId: string
  user: { id: string; email: string; name: string }
  /** True when this validation slid the expiry forward — caller should refresh the cookie. */
  renewed: boolean
}

/**
 * Looks up a session by hash(rawToken) and returns the attached user.
 * Returns null for missing, expired, or otherwise invalid tokens.
 */
export async function validateSession(rawToken: string): Promise<ValidatedSession | null> {
  const hash = hashToken(rawToken)
  const row = await db
    .selectFrom('sessions')
    .innerJoin('users', 'users.id', 'sessions.user_id')
    .select([
      'sessions.id as session_id',
      'sessions.expires_at',
      'users.id as user_id',
      'users.email',
      'users.name',
    ])
    .where('sessions.access_token_hash', '=', hash)
    .executeTakeFirst()

  if (!row) return null
  const now = Date.now()
  const expiresAt = new Date(row.expires_at).getTime()
  if (expiresAt <= now) {
    await db.deleteFrom('sessions').where('id', '=', row.session_id).execute()
    return null
  }

  let renewed = false
  if (expiresAt - now < SESSION_RENEW_THRESHOLD_MS) {
    await db
      .updateTable('sessions')
      .set({ expires_at: new Date(now + SESSION_TTL_MS) })
      .where('id', '=', row.session_id)
      .execute()
    renewed = true
  }

  return {
    sessionId: row.session_id,
    user: { id: row.user_id, email: row.email, name: row.name },
    renewed,
  }
}

export async function destroySession(sessionId: string): Promise<void> {
  await db.deleteFrom('sessions').where('id', '=', sessionId).execute()
}
