import { randomBytes, scrypt, timingSafeEqual } from 'node:crypto'

const SCRYPT_N = 16384
const SCRYPT_R = 8
const SCRYPT_P = 1
const KEY_LEN = 64
const SALT_LEN = 32

/**
 * Hash a password using Node's built-in scrypt.
 * Returns a self-describing string: `scrypt:N=...,r=...,p=...:salt_hex:hash_hex`
 */
export async function hashPassword(plain: string): Promise<string> {
  const salt = randomBytes(SALT_LEN)
  const derived = await new Promise<Buffer>((resolve, reject) => {
    scrypt(plain, salt, KEY_LEN, { N: SCRYPT_N, r: SCRYPT_R, p: SCRYPT_P }, (err, key) => {
      if (err) reject(err)
      else resolve(key)
    })
  })
  return `scrypt:N=${SCRYPT_N},r=${SCRYPT_R},p=${SCRYPT_P}:${salt.toString('hex')}:${derived.toString('hex')}`
}

/**
 * Verify a password against a hash string produced by `hashPassword`.
 * Uses timing-safe comparison.
 */
export async function verifyPassword(plain: string, stored: string): Promise<boolean> {
  const parts = stored.split(':')
  if (parts[0] !== 'scrypt' || parts.length !== 4) return false

  const salt = Buffer.from(parts[2], 'hex')
  const expected = Buffer.from(parts[3], 'hex')

  const derived = await new Promise<Buffer>((resolve, reject) => {
    scrypt(plain, salt, expected.length, { N: SCRYPT_N, r: SCRYPT_R, p: SCRYPT_P }, (err, key) => {
      if (err) reject(err)
      else resolve(key)
    })
  })

  return timingSafeEqual(derived, expected)
}
