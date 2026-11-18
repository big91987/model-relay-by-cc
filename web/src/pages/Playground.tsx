import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { ApiError, download, fmtDuration, fmtTime, gwError, gwHeaders } from '../api'
import { KeyPicker, NeedKey, useKeyAuth, useModels } from '../keyctx'
import { Field, useUI } from '../ui'

type Usage = { prompt: number; completion: number; total: number; estimated: boolean } | null
type Msg = {
  id: number
  role: 'user' | 'assistant' | 'note'
  content: string
  reasoning?: string
  state?: 'streaming' | 'done' | 'stopped' | 'error'
  error?: string
  errorCode?: string
  requestId?: string
  usage?: Usage
  durationMs?: number
  firstTokenMs?: number
  model?: string
  params?: Record<string, any>
  at: number
}

// 把网关的错误码翻译成用户能直接理解的说明（原始信息仍会显示）
const ERROR_HINT: Record<string, string> = {
  invalid_api_key: '网关 Key 无效，请检查后重新输入。',
  missing_api_key: '缺少网关 Key。',
  key_disabled: '该网关 Key 已被禁用。',
  key_expired: '该网关 Key 已过期。',
  not_logged_in: '控制台登录已过期，请重新登录。',
  model_not_allowed: '权限不足：该 Key 无权访问所选模型。',
  model_not_found: '所选模型当前没有可用渠道。',
  insufficient_quota: '额度不足：该 Key 的可用额度不够支付本次请求的费用上界。',
  price_not_configured: '该模型尚未定价，有限额度的 Key 不能调用。',
  rate_limit_exceeded: '触发限流：请求过于频繁，请稍后再试。',
  concurrency_limit_exceeded: '该 Key 的在途请求数已达上限，请稍后再试。',
  upstream_error: '上游服务失败。',
  upstream_timeout: '上游超时，本次请求的费用待核实。',
  upstream_rate_limited: '上游限流，请稍后再试。',
  upstream_bad_request: '上游拒绝了请求，请检查参数。',
  upstream_interrupted: '上游响应中断，内容可能不完整。',
  network_error: '连接中断：与网关的连接已断开。',
}

let seq = 1

export default function Playground() {
  const { auth } = useKeyAuth()
  const { models, error: modelErr, loading } = useModels('chat')
  const [model, setModel] = useState('')
  const [system, setSystem] = useState('')
  const [temperature, setTemperature] = useState(1)
  const [useTemp, setUseTemp] = useState(true)
  const [maxTokens, setMaxTokens] = useState(1024)
  const [stream, setStream] = useState(true)
  const [msgs, setMsgs] = useState<Msg[]>([])
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const boxRef = useRef<HTMLDivElement>(null)
  const ui = useUI()
  const cur = useMemo(() => models.find((m) => m.id === model), [models, model])

  // 模型列表变化时（切换 Key），保持或重置所选模型
  useEffect(() => {
    if (models.length && !models.some((m) => m.id === model)) setModel(models[0].id)
    if (!models.length) setModel('')
  }, [models]) // eslint-disable-line react-hooks/exhaustive-deps

  // 参数受所选模型的支持范围约束
  const tempMax = cur?.temperature_max || 2
  const tempSupported = cur ? cur.supports_temperature : true
  const maxLimit = cur?.max_output_tokens || 0
  useEffect(() => {
    if (temperature > tempMax) setTemperature(tempMax)
    if (maxLimit > 0 && maxTokens > maxLimit) setMaxTokens(maxLimit)
  }, [tempMax, maxLimit]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    boxRef.current?.scrollTo({ top: boxRef.current.scrollHeight })
  }, [msgs])

  const patch = (id: number, p: Partial<Msg> | ((m: Msg) => Partial<Msg>)) =>
    setMsgs((list) => list.map((m) => (m.id === id ? { ...m, ...(typeof p === 'function' ? p(m) : p) } : m)))

  const changeModel = (next: string) => {
    if (next === model) return
    setModel(next)
    if (msgs.some((m) => m.role !== 'note')) {
      setMsgs((l) => [...l, { id: seq++, role: 'note', content: `已切换到模型 ${next}：当前对话保留，后续消息将由新模型回复。`, at: Date.now() }])
    }
  }

  // run：以 history（不含错误和提示）为上下文发起一次真实的网关请求
  const run = async (history: Msg[]) => {
    if (!auth || !model || busy) return
    const params: Record<string, any> = { model, stream, max_tokens: maxTokens }
    if (tempSupported && useTemp) params.temperature = temperature
    const messages: { role: string; content: string }[] = []
    if (system.trim()) messages.push({ role: 'system', content: system })
    for (const m of history) {
      if (m.role === 'note' || m.state === 'error') continue
      if (m.role === 'assistant' && !m.content) continue
      messages.push({ role: m.role, content: m.content })
    }
    const body: Record<string, any> = { ...params, messages }
    if (stream) body.stream_options = { include_usage: true }
    const id = seq++
    const started = performance.now()
    setMsgs([...history, { id, role: 'assistant', content: '', state: 'streaming', model, params, at: Date.now() }])
    setBusy(true)
    const ac = new AbortController()
    abortRef.current = ac
    let requestId = ''
    try {
      // 对话始终经过真实网关的 /v1/chat/completions，页面不直连上游
      const resp = await fetch('/v1/chat/completions', { method: 'POST', headers: gwHeaders(auth), body: JSON.stringify(body), signal: ac.signal, credentials: 'same-origin' })
      requestId = resp.headers.get('X-Request-Id') || ''
      patch(id, { requestId })
      if (!resp.ok) throw await gwError(resp)
      if (!stream) {
        const data = await resp.json()
        const u = data.usage
        patch(id, {
          content: data.choices?.[0]?.message?.content || '',
          reasoning: data.choices?.[0]?.message?.reasoning_content || '',
          state: 'done',
          durationMs: Math.round(performance.now() - started),
          usage: u ? { prompt: u.prompt_tokens || 0, completion: u.completion_tokens || 0, total: u.total_tokens || 0, estimated: !!u.estimated || resp.headers.get('X-Usage-Source') === 'estimated' } : null,
        })
        return
      }
      const reader = resp.body!.getReader()
      const decoder = new TextDecoder()
      let buf = ''
      let usage: Usage = null
      let firstTokenMs = 0
      let streamErr: ApiError | null = null
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        let idx: number
        while ((idx = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, idx).trim()
          buf = buf.slice(idx + 1)
          if (!line.startsWith('data:')) continue
          const payload = line.slice(5).trim()
          if (!payload || payload === '[DONE]') continue
          let chunk: any
          try {
            chunk = JSON.parse(payload)
          } catch {
            continue
          }
          if (chunk.error) {
            streamErr = new ApiError(chunk.error.message || '上游流式响应中断', 502, chunk.error.code || 'upstream_interrupted', requestId)
            continue
          }
          const delta = chunk.choices?.[0]?.delta
          if (delta?.content || delta?.reasoning_content) {
            if (!firstTokenMs) firstTokenMs = Math.round(performance.now() - started)
            patch(id, (m) => ({ content: m.content + (delta.content || ''), reasoning: (m.reasoning || '') + (delta.reasoning_content || '') }))
          }
          if (chunk.usage) usage = { prompt: chunk.usage.prompt_tokens || 0, completion: chunk.usage.completion_tokens || 0, total: chunk.usage.total_tokens || 0, estimated: !!chunk.usage.estimated }
        }
      }
      if (streamErr) throw streamErr
      patch(id, { state: 'done', usage, durationMs: Math.round(performance.now() - started), firstTokenMs })
    } catch (e: any) {
      const durationMs = Math.round(performance.now() - started)
      if (e?.name === 'AbortError') {
        // 用户停止：保留已生成的内容。已尽力取消上游，但不保证不产生费用。
        patch(id, { state: 'stopped', durationMs, usage: null })
      } else {
        const err: ApiError = e instanceof ApiError ? e : new ApiError('连接中断：与网关的连接已断开，回复可能不完整', 0, 'network_error', requestId)
        patch(id, (m) => ({ state: 'error', error: err.message, errorCode: err.code, requestId: err.requestId || m.requestId, durationMs }))
      }
    } finally {
      abortRef.current = null
      setBusy(false)
    }
  }

  const send = () => {
    const text = input.trim()
    if (!text || busy || !auth || !model) return
    setInput('')
    run([...msgs, { id: seq++, role: 'user', content: text, at: Date.now() }])
  }
  const stop = () => abortRef.current?.abort()
  const clear = async () => {
    if (busy) return
    if (msgs.length && !(await ui.confirm({ title: '清空对话', content: '清空当前对话的全部消息？此操作只影响本页面，不会发起请求，也不会删除请求日志。', okText: '清空' }))) return
    setMsgs([])
  }
  // 重新发送：去掉最后一条回复，用当前参数对同一条用户消息发起新的请求
  const retry = async () => {
    if (busy) return
    const lastUser = msgs.map((m) => m.role).lastIndexOf('user')
    if (lastUser < 0) return
    const ok = await ui.confirm({
      title: '重新发送',
      content: (
        <>
          <p style={{ marginTop: 0 }}>将使用<b>当前所选的模型和参数</b>，对最后一条用户消息<b>发起一次新的请求</b>，原回复会被替换。</p>
          <div className="alert warn" style={{ marginBottom: 0 }}>新的请求会被网关单独计费并记入日志，可能产生新的费用。</div>
        </>
      ),
      okText: '发起新请求',
    })
    if (ok) run(msgs.slice(0, lastUser + 1))
  }
  const exportMd = () => {
    const chat = msgs.filter((m) => m.role !== 'note' && m.state !== 'error' && (m.role === 'user' || m.content))
    if (!chat.length) return ui.toast('当前会话还没有可导出的消息', 'err')
    const lines = ['# Playground 对话', '', `- 导出时间：${fmtTime(Date.now())}`, `- 模型：${model}`, '']
    if (system.trim()) lines.push('## 系统提示词', '', system.trim(), '')
    chat.forEach((m) => {
      lines.push(m.role === 'user' ? '## 用户' : `## 模型回复${m.model ? `（${m.model}）` : ''}`, '', m.content.trim() + (m.state === 'stopped' ? '\n\n> （生成被手动停止，内容不完整）' : ''), '')
    })
    const p = (n: number) => String(n).padStart(2, '0')
    const d = new Date()
    download(new Blob([lines.join('\n')], { type: 'text/markdown;charset=utf-8' }), `对话_${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}_${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}.md`)
  }

  const canSend = !!auth && !!model && !busy
  const hasUser = msgs.some((m) => m.role === 'user')
  return (
    <>
      <div className="page-head">
        <div>
          <h1>聊天 Playground</h1>
          <p>在控制台直接试用聊天模型。请求经过真实网关，沿用所选 Key 的模型权限、额度、限流和请求日志。</p>
        </div>
      </div>
      <div className="card" style={{ marginBottom: 16 }}>
        <KeyPicker />
        <div className="hint" style={{ marginTop: 8 }}>手动输入的 Key 只保存在当前页面的内存中：不写入地址栏和浏览器存储，刷新页面后需重新输入。</div>
      </div>
      <NeedKey models={models} error={modelErr} loading={loading} kindLabel="聊天" />
      <div className="pg">
        <div className="card chat">
          <div className="msgs" ref={boxRef}>
            {msgs.length === 0 && <div className="empty">{auth ? '输入消息开始对话。支持多轮上下文。' : '选择网关 Key 和模型后即可开始对话。'}</div>}
            {msgs.map((m) =>
              m.role === 'note' ? (
                <div key={m.id} className="note">{m.content}</div>
              ) : (
                <div key={m.id} className={`msg ${m.role} ${m.state === 'error' ? 'error' : ''}`}>
                  {m.state === 'error' ? (
                    <div className="bubble" role="alert">
                      <b>{ERROR_HINT[m.errorCode || ''] || '请求失败。'}</b>
                      <div className="small" style={{ marginTop: 4 }}>{m.error}</div>
                      {m.content && <div className="small" style={{ marginTop: 6 }}>已收到的部分内容：{m.content}</div>}
                    </div>
                  ) : (
                    <div className="bubble">
                      {m.reasoning && <div className="reasoning">{m.reasoning}</div>}
                      <span className={m.state === 'streaming' ? 'cursor' : ''}>{m.content}</span>
                      {m.state === 'streaming' && !m.content && !m.reasoning && <span className="muted">正在等待回复…</span>}
                    </div>
                  )}
                  {m.role === 'assistant' && m.state !== 'streaming' && (
                    <div className="meta">
                      {m.state === 'stopped' && <span className="badge warn">已停止</span>}
                      {m.state === 'done' &&
                        (m.usage ? (
                          <span>
                            Token：输入 {m.usage.prompt} / 输出 {m.usage.completion} / 合计 {m.usage.total}
                            {m.usage.estimated && <span className="badge warn" style={{ marginLeft: 6 }} title="上游没有返回用量，该数值为网关估算">估算</span>}
                          </span>
                        ) : (
                          <span title="上游没有返回用量">Token 用量：不可用</span>
                        ))}
                      {m.state === 'stopped' && <span title="停止后不再读取响应，实际用量与费用以请求日志为准">Token 用量：不可用（以日志为准）</span>}
                      <span>耗时 {fmtDuration(m.durationMs)}{m.firstTokenMs ? `（首字 ${fmtDuration(m.firstTokenMs)}）` : ''}</span>
                      {m.requestId && (
                        <span>
                          请求 ID：<Link to={`/logs?request_id=${m.requestId}`} title="在调用日志中查看该请求" className="mono">{m.requestId}</Link>
                        </span>
                      )}
                      {m.params && <span title={JSON.stringify(m.params, null, 2)} style={{ cursor: 'help' }}>参数：{m.params.model} · 温度 {m.params.temperature ?? '未设置'} · 最大输出 {m.params.max_tokens} · {m.params.stream ? '流式' : '非流式'}</span>}
                    </div>
                  )}
                </div>
              ),
            )}
          </div>
          <div className="composer">
            <textarea
              value={input}
              placeholder={auth ? '输入消息，Enter 发送，Shift+Enter 换行' : '请先选择网关 Key'}
              disabled={!auth || !model}
              aria-label="消息内容"
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                  e.preventDefault()
                  send()
                }
              }}
            />
            {busy ? <button className="danger" onClick={stop}>停止生成</button> : <button className="primary" disabled={!canSend || !input.trim()} onClick={send}>发送</button>}
          </div>
        </div>
        <div className="card" style={{ overflowY: 'auto' }}>
          <Field label="模型">
            <select value={model} onChange={(e) => changeModel(e.target.value)} disabled={busy || !models.length} aria-label="模型">
              {!models.length && <option value="">{auth ? '无可用模型' : '请先选择 Key'}</option>}
              {models.map((m) => <option key={m.id} value={m.id}>{m.id}</option>)}
            </select>
          </Field>
          <Field label="系统提示词">
            <textarea value={system} onChange={(e) => setSystem(e.target.value)} placeholder="可选。例如：你是一个简洁的助手。" disabled={busy} />
          </Field>
          <Field
            label={`温度：${tempSupported && useTemp ? temperature.toFixed(1) : '不发送'}`}
            hint={tempSupported ? `该模型支持的范围：0 ~ ${tempMax}` : '所选模型不支持调节温度，请求中不会携带该参数。'}
          >
            <div className="row">
              <input type="range" min={0} max={tempMax} step={0.1} value={Math.min(temperature, tempMax)} disabled={!tempSupported || !useTemp || busy} onChange={(e) => setTemperature(Number(e.target.value))} style={{ flex: 1 }} aria-label="温度" />
              <label className="check small"><input type="checkbox" checked={tempSupported && useTemp} disabled={!tempSupported || busy} onChange={(e) => setUseTemp(e.target.checked)} />发送</label>
            </div>
          </Field>
          <Field label="最大输出长度（Token）" hint={maxLimit > 0 ? `该模型上限：${maxLimit}` : '该模型未配置上限；该值同时决定网关预留的费用上界'}>
            <input
              type="number" min={1} max={maxLimit || undefined} value={maxTokens} disabled={busy} aria-label="最大输出长度"
              onChange={(e) => {
                let v = Math.max(1, Math.floor(Number(e.target.value) || 1))
                if (maxLimit > 0 && v > maxLimit) v = maxLimit
                setMaxTokens(v)
              }}
            />
          </Field>
          <Field label="">
            <label className="check"><input type="checkbox" checked={stream} disabled={busy} onChange={(e) => setStream(e.target.checked)} />流式显示</label>
          </Field>
          <div className="row">
            <button onClick={retry} disabled={busy || !hasUser || !auth || !model} title="对最后一条用户消息发起新的请求，可能产生新的费用">重新发送</button>
            <button onClick={clear} disabled={busy || !msgs.length}>清空对话</button>
            <button onClick={exportMd} disabled={busy || !hasUser}>导出 Markdown</button>
          </div>
          <p className="hint">“停止生成”会停止页面读取并尽力取消上游请求，但已产生的用量仍可能计费。“重新发送”会发起新的请求并可能产生新的费用。</p>
        </div>
      </div>
    </>
  )
}
