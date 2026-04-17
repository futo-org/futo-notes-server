import assert from 'node:assert/strict'
import { test } from 'node:test'
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
  }
  assert.equal(data.name, 'stonefruit')
  assert.match(data.version, /^\d+\.\d+\.\d+$/)
  assert.equal(data.auth_mode, 'dev')
  assert.equal(data.signup, 'closed')
  assert.equal(data.billing, false)
})
