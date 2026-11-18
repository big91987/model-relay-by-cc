// 控制台与后端的通信封装。
// /api/*：管理接口，使用登录会话 Cookie。
// /v1/*：真实的网关接口。控制台可以“选择已有 Key”（只传 Key 的 ID，密钥不经过浏览器），
//        或使用用户临时粘贴的 Key（只保存在内存中）。

export class ApiError extends Error {
  status: number
  code: string
  requestId: string
  constructor(message: string, status: number, code = '', requestId = '') {
    super(message)
    this.status = status
    this.code = code
    this.requestId = requestId
  }
}

export async function api<T = any>(path: string, opts: { method?: string; body?: any } = {}): Promise<T> {
  let resp: Response
  try {
    resp = await fetch('/api' + path, {
      method: opts.method || 'GET',
      headers: opts.body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
      credentials: 'same-origin',
    })
  } catch {
    throw new ApiError('无法连接网关服务，请检查网络或服务是否在运行', 0)
  }
  const data = await resp.json().catch(() => ({}))
  if (resp.status === 401 && path !== '/login') {
    window.dispatchEvent(new Event('relay-unauthorized'))
  }
  if (!resp.ok) throw new ApiError(data?.error || `请求失败（HTTP ${resp.status}）`, resp.status)
  return data as T
}

export type KeyAuth = { mode: 'id'; id: number; name: string } | { mode: 'secret'; secret: string }

export function gwHeaders(auth: KeyAuth, json = true): Record<string, string> {
  const h: Record<string, string> = json ? { 'Content-Type': 'application/json' } : {}
  if (auth.mode === 'id') h['X-Console-Key-Id'] = String(auth.id)
  else {
    h['Authorization'] = 'Bearer ' + auth.secret
    h['X-Console'] = '1'
  }
  return h
}

export async function gwError(resp: Response): Promise<ApiError> {
  const reqId = resp.headers.get('X-Request-Id') || ''
  const data = await resp.json().catch(() => null)
  const e = data?.error
  const msg = (typeof e === 'string' ? e : e?.message) || `请求失败（HTTP ${resp.status}）`
  return new ApiError(msg, resp.status, (typeof e === 'object' && e?.code) || '', e?.request_id || reqId)
}

export async function gwFetch(path: string, auth: KeyAuth, init: RequestInit = {}): Promise<Response> {
  const isForm = init.body instanceof FormData
  let resp: Response
  try {
    resp = await fetch('/v1' + path, { ...init, headers: { ...gwHeaders(auth, !isForm), ...(init.headers || {}) }, credentials: 'same-origin' })
  } catch (err: any) {
    if (err?.name === 'AbortError') throw err
    throw new ApiError('连接中断：无法连接网关，请检查网络后重试', 0, 'network_error')
  }
  if (!resp.ok) throw await gwError(resp)
  return resp
}

export async function gw<T = any>(path: string, auth: KeyAuth, body?: any, method?: string): Promise<T> {
  const resp = await gwFetch(path, auth, { method: method || (body !== undefined ? 'POST' : 'GET'), body: body !== undefined ? JSON.stringify(body) : undefined })
  return resp.json()
}

// —— 格式化 ——
export function yuan(micro: number | undefined | null, digits = 6): string {
  const v = (micro || 0) / 1e6
  let s = v.toFixed(digits)
  if (digits > 2) s = s.replace(/(\.\d{2}\d*?)0+$/, '$1')
  return '¥' + s
}
export function fmtTime(ms: number | undefined): string {
  if (!ms) return '-'
  const d = new Date(ms)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}
export function fmtDuration(ms: number | undefined): string {
  if (!ms) return '-'
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(2)} s`
}
export function fmtSize(n: number): string {
  if (!n) return '-'
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
  return (n / 1024 / 1024).toFixed(1) + ' MB'
}
export function toLocalInput(ms: number): string {
  const d = new Date(ms - new Date(ms).getTimezoneOffset() * 60000)
  return d.toISOString().slice(0, 16)
}
export function fromLocalInput(v: string): number {
  return v ? new Date(v).getTime() : 0
}

export const KIND_LABEL: Record<string, string> = { chat: '聊天', embedding: '向量', video: '视频', tts: '语音', music: '音乐', voice: '音色' }
export const STATUS_LABEL: Record<string, string> = { success: '成功', error: '失败', canceled: '已取消', rejected: '已拒绝', in_progress: '进行中', interrupted: '中断' }
export const STATUS_CLS: Record<string, string> = { success: 'ok', error: 'err', canceled: 'warn', rejected: 'warn', in_progress: 'info', interrupted: 'err' }
export const COST_LABEL: Record<string, string> = { actual: '实际', estimated: '估算', pending: '待核实', none: '未计费', unpriced: '未定价' }
export const COST_CLS: Record<string, string> = { actual: 'ok', estimated: 'warn', pending: 'err', none: '', unpriced: 'warn' }
export const TASK_LABEL: Record<string, string> = { queued: '排队中', processing: '生成中', succeeded: '已完成', failed: '失败' }
export const TASK_CLS: Record<string, string> = { queued: 'info', processing: 'info', succeeded: 'ok', failed: 'err' }
export const PROVIDER_LABEL: Record<string, string> = { minimax: 'MiniMax', volcengine: 'Seedance（火山方舟）', openai: 'OpenAI 兼容', deepseek: 'DeepSeek', zhipu: '智谱' }

export function download(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 5000)
}
