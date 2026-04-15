export async function checkHealth(baseUrl: string): Promise<{ status: string; db: string; setup_complete: boolean } | null> {
  try {
    const res = await fetch(`${baseUrl}/health`)
    if (!res.ok) return null
    return await res.json() as { status: string; db: string; setup_complete: boolean }
  } catch {
    return null
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
