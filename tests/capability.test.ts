import assert from 'node:assert/strict'
import { test } from 'bun:test'
import { buildApp } from '../src/app.ts'

const app = buildApp()

test('GET / returns capability JSON', async () => {
  const response = await app.fetch(new Request('http://test.local/'))
  assert.equal(response.status, 200)
  assert.match(response.headers.get('content-type') ?? '', /^application\/json/)
  const data = await response.json() as {
    name: string
    version: string
    auth_mode: string
    signup: string
    billing: boolean
    mutation_ids: {
      supported: boolean
      required: boolean
      retention_days: number
      successful_create_outcomes: string
    }
  }
  assert.equal(data.name, 'futo-notes')
  assert.match(data.version, /^\d+\.\d+\.\d+$/)
  assert.equal(data.auth_mode, 'dev')
  assert.equal(data.signup, 'closed')
  assert.equal(data.billing, false)
  assert.deepEqual(data.mutation_ids, {
    supported: true,
    required: false,
    retention_days: 30,
    successful_create_outcomes: 'durable',
  })
})
