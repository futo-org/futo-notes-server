export async function checkHealth(baseUrl: string): Promise<{ status: string; db: string; setup_complete: boolean } | null> {
  try {
    const res = await fetch(`${baseUrl}/health`)
    if (!res.ok) return null
    return await res.json() as { status: string; db: string; setup_complete: boolean }
  } catch {
    return null
  }
}

export async function setupServer(baseUrl: string, password: string): Promise<{ admin_token: string }> {
  const res = await fetch(`${baseUrl}/setup`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
  })
  if (res.status === 409) {
    throw new Error('Server is already configured.')
  }
  if (!res.ok) {
    const body = await res.text()
    throw new Error(`Setup failed (${res.status}): ${body}`)
  }
  return await res.json() as { admin_token: string }
}

export async function resetPassword(baseUrl: string, adminToken: string, password: string): Promise<void> {
  const res = await fetch(`${baseUrl}/admin/reset-password`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'AdminToken': adminToken,
    },
    body: JSON.stringify({ password }),
  })
  if (!res.ok) {
    const body = await res.text()
    throw new Error(`Reset failed (${res.status}): ${body}`)
  }
}

export async function waitForHealthy(baseUrl: string, timeoutMs = 30000): Promise<void> {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) {
    const health = await checkHealth(baseUrl)
    if (health && health.status === 'ok') return
    await new Promise((r) => setTimeout(r, 1000))
  }
  throw new Error(`Server did not become healthy within ${timeoutMs / 1000}s`)
}
