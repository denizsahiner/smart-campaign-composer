// Thin client for the Go API. Errors from the API arrive as {error:{code,message,errors}}.
export class ApiError extends Error {
  constructor(status, body) {
    super(body?.error?.message || `Request failed (${status})`)
    this.status = status
    this.code = body?.error?.code
    this.fields = body?.error?.errors || []
  }
}

async function call(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  })
  const data = await res.json().catch(() => null)
  if (!res.ok) throw new ApiError(res.status, data)
  return data
}

export const api = {
  list: () => call('GET', '/api/campaigns').then((d) => d.campaigns),
  get: (id) => call('GET', `/api/campaigns/${id}`),
  create: (brief) => call('POST', '/api/campaigns', { brief }),
  rerunAudience: (id, brief) => call('POST', `/api/campaigns/${id}/audience`, { brief }),
  acknowledge: (id) => call('POST', `/api/campaigns/${id}/acknowledge`),
  refreshAudience: (id) => call('POST', `/api/campaigns/${id}/audience/refresh`),
  approve: (id, content) => call('POST', `/api/campaigns/${id}/approve`, { content }),
}

// Streams the messages step (Server-Sent Events over POST). onEvent gets {type, ...}.
// Events: delta, retry, then one of validated | invalid | error.
export async function generateMessages(id, feedback, onEvent, signal) {
  const res = await fetch(`/api/campaigns/${id}/messages`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ feedback: feedback || '' }),
    signal,
  })
  if (!res.ok) throw new ApiError(res.status, await res.json().catch(() => null))
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    let i
    while ((i = buf.indexOf('\n\n')) >= 0) {
      const block = buf.slice(0, i)
      buf = buf.slice(i + 2)
      const data = block.split('\n').find((l) => l.startsWith('data: '))
      if (data) onEvent(JSON.parse(data.slice(6)))
    }
  }
}
