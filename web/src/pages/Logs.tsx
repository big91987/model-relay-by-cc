import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { COST_CLS, COST_LABEL, KIND_LABEL, STATUS_CLS, STATUS_LABEL, api, fmtDuration, fmtTime, fromLocalInput, yuan } from '../api'
import { Badge, Modal, Pager, useLoad } from '../ui'

export type Log = {
  request_id: string; key_id: number; key_name: string; source: string; endpoint: string; kind: string; model: string; channel_name: string; upstream_model: string
  stream: boolean; status: string; http_status: number; error_code: string; error_message: string; prompt_tokens: number; completion_tokens: number; total_tokens: number
  units: number; cost: number; cost_status: string; usage_source: string; reserved: number; attempts: number; duration_ms: number; first_token_ms: number; created_at: number
}

export function LogTable({ items, onOpen }: { items: Log[]; onOpen: (id: string) => void }) {
  return (
    <div className="table-wrap">
      <table>
        <thead><tr><th>时间</th><th>请求 ID</th><th>Key</th><th>模型</th><th>类型</th><th>状态</th><th className="right">输入 / 输出</th><th className="right">费用</th><th className="right">耗时</th></tr></thead>
        <tbody>
          {items.map((l) => (
            <tr key={l.request_id}>
              <td className="small nowrap">{fmtTime(l.created_at)}</td>
              <td><button className="link mono" onClick={() => onOpen(l.request_id)} title="查看详情">{l.request_id}</button></td>
              <td className="small">{l.key_name}{l.source === 'console' && <span className="muted">（控制台）</span>}</td>
              <td className="mono small">{l.model}</td>
              <td className="small">{KIND_LABEL[l.kind] || l.kind}{l.stream ? ' · 流式' : ''}</td>
              <td><Badge cls={STATUS_CLS[l.status]}>{STATUS_LABEL[l.status] || l.status}</Badge>{l.error_message && <div className="hint" title={l.error_message} style={{ maxWidth: 220, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{l.error_message}</div>}</td>
              <td className="right small nowrap">{l.kind === 'chat' || l.kind === 'embedding' ? `${l.prompt_tokens} / ${l.completion_tokens}` : l.units ? `${l.units}` : '-'}</td>
              <td className="right nowrap">{yuan(l.cost)} {l.cost_status !== 'actual' && l.cost_status !== 'none' && <Badge cls={COST_CLS[l.cost_status]}>{COST_LABEL[l.cost_status]}</Badge>}</td>
              <td className="right small nowrap">{fmtDuration(l.duration_ms)}</td>
            </tr>
          ))}
          {items.length === 0 && <tr><td colSpan={9} className="empty">没有符合条件的调用记录。</td></tr>}
        </tbody>
      </table>
    </div>
  )
}

export function LogDetail({ requestId, onClose }: { requestId: string; onClose: () => void }) {
  const { data, error } = useLoad<any>(() => api('/logs/' + encodeURIComponent(requestId)), [requestId])
  const l: Log | undefined = data?.log
  const r = data?.reservation
  return (
    <Modal wide title="请求详情" onClose={onClose} footer={<button onClick={onClose}>关闭</button>}>
      {error && <div className="alert err">{error}</div>}
      {l && (
        <>
          <dl className="kv">
            <dt>请求 ID</dt><dd className="mono">{l.request_id}</dd>
            <dt>时间</dt><dd>{fmtTime(l.created_at)}</dd>
            <dt>Key</dt><dd>{l.key_name}（ID {l.key_id}，来源：{l.source === 'console' ? '控制台' : 'API'}）</dd>
            <dt>接口 / 模型</dt><dd><span className="mono">{l.endpoint}</span> · <span className="mono">{l.model}</span>{l.stream ? ' · 流式' : ''}</dd>
            <dt>渠道</dt><dd>{l.channel_name || '-'}{l.upstream_model && l.upstream_model !== l.model ? `（上游模型 ${l.upstream_model}）` : ''}</dd>
            <dt>状态</dt><dd><Badge cls={STATUS_CLS[l.status]}>{STATUS_LABEL[l.status] || l.status}</Badge> HTTP {l.http_status || '-'}{l.error_code && <span className="mono"> · {l.error_code}</span>}</dd>
            {l.error_message && <><dt>错误信息</dt><dd>{l.error_message}</dd></>}
            <dt>Token</dt><dd>输入 {l.prompt_tokens} / 输出 {l.completion_tokens} / 合计 {l.total_tokens}{l.units ? ` · 计费数量 ${l.units}` : ''}（用量来源：{l.usage_source === 'upstream' ? '上游返回' : l.usage_source === 'estimated' ? '网关估算' : '无'}）</dd>
            <dt>费用</dt><dd>{yuan(l.cost)} <Badge cls={COST_CLS[l.cost_status]}>{COST_LABEL[l.cost_status]}</Badge>，调用前预留的费用上界 {yuan(l.reserved)}</dd>
            <dt>耗时</dt><dd>{fmtDuration(l.duration_ms)}{l.first_token_ms ? `，首字 ${fmtDuration(l.first_token_ms)}` : ''}</dd>
          </dl>
          {r && (
            <div className="alert" style={{ marginTop: 12 }}>
              额度预留：{yuan(r.amount)}，状态 <b>{({ held: '占用中', settled: '已结算', released: '已释放', pending_review: '待核实' } as any)[r.status]}</b>
              {r.status === 'settled' && `，实际结算 ${yuan(r.actual_cost)}`}{r.note && `；${r.note}`}{r.resolved_by && `（处理人：${r.resolved_by}）`}
            </div>
          )}
          <h3 style={{ marginTop: 14 }}>上游调用明细（{data.attempts.length} 次）</h3>
          <div className="table-wrap">
            <table>
              <thead><tr><th>#</th><th>渠道</th><th>上游模型</th><th>结果</th><th>HTTP</th><th className="right">Token</th><th className="right">费用</th><th className="right">耗时</th></tr></thead>
              <tbody>
                {data.attempts.map((a: any, i: number) => (
                  <tr key={i}>
                    <td>{a.attempt}</td><td>{a.channel_name}</td><td className="mono small">{a.upstream_model}</td>
                    <td>{({ success: '成功', failed: '失败（未产生用量）', unknown: '结果未知', canceled: '已取消', interrupted: '中断' } as any)[a.status] || a.status}{a.error && <div className="hint">{a.error}</div>}</td>
                    <td>{a.http_status || '-'}</td><td className="right small">{a.prompt_tokens} / {a.completion_tokens}</td>
                    <td className="right">{yuan(a.cost)} <span className="muted small">{COST_LABEL[a.cost_status]}</span></td><td className="right small">{fmtDuration(a.duration_ms)}</td>
                  </tr>
                ))}
                {data.attempts.length === 0 && <tr><td colSpan={8} className="empty">该请求没有调用上游。</td></tr>}
              </tbody>
            </table>
          </div>
        </>
      )}
    </Modal>
  )
}

export function filterQuery(f: Record<string, string>): string {
  const q = new URLSearchParams()
  if (f.request_id?.trim()) q.set('request_id', f.request_id.trim())
  for (const k of ['model', 'key_id', 'status', 'kind', 'cost_status']) if (f[k]) q.set(k, f[k])
  if (f.start) q.set('start', String(fromLocalInput(f.start)))
  if (f.end) q.set('end', String(fromLocalInput(f.end)))
  return q.toString()
}

export default function Logs() {
  const [sp, setSp] = useSearchParams()
  const [f, setF] = useState<Record<string, string>>({ request_id: sp.get('request_id') || '', model: '', key_id: '', status: '', start: '', end: '' })
  const [applied, setApplied] = useState(f)
  const [page, setPage] = useState(1)
  const [open, setOpen] = useState<string | null>(null)
  const keys = useLoad<{ items: { id: number; name: string }[] }>(() => api('/keys'), [])
  const { data, error, loading, reload } = useLoad<{ items: Log[]; total: number }>(() => api(`/logs?${filterQuery(applied)}&page=${page}&page_size=20`), [applied, page])
  // 从 Playground 等页面带着请求 ID 跳转过来
  useEffect(() => {
    const id = sp.get('request_id') || ''
    if (id && id !== applied.request_id) {
      const next = { ...f, request_id: id }
      setF(next)
      setApplied(next)
      setPage(1)
    }
  }, [sp]) // eslint-disable-line react-hooks/exhaustive-deps
  const search = () => {
    setApplied({ ...f })
    setPage(1)
    setSp(f.request_id.trim() ? { request_id: f.request_id.trim() } : {}, { replace: true })
  }
  const reset = () => {
    const empty = { request_id: '', model: '', key_id: '', status: '', start: '', end: '' }
    setF(empty)
    setApplied(empty)
    setPage(1)
    setSp({}, { replace: true })
  }
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const exportUrl = `/api/logs/export?${filterQuery(applied)}&tz_offset=${-new Date().getTimezoneOffset()}`
  return (
    <>
      <div className="page-head">
        <div>
          <h1>调用日志</h1>
          <p>每次经过网关的调用都有唯一的请求 ID（响应头 X-Request-Id）。可按请求 ID 精确查询，并导出明细供财务对账。</p>
        </div>
        <a className="btn" href={exportUrl} title="按当前筛选条件导出 CSV（可用 Excel 打开）">导出调用明细（CSV）</a>
      </div>
      <div className="card" style={{ marginBottom: 16 }}>
        <div className="row">
          <input className="mono" style={{ width: 300 }} placeholder="请求 ID（精确匹配，如 req_…）" aria-label="请求 ID" value={f.request_id} onChange={(e) => set('request_id', e.target.value)} onKeyDown={(e) => e.key === 'Enter' && search()} />
          <input style={{ width: 170 }} placeholder="模型名" aria-label="模型" value={f.model} onChange={(e) => set('model', e.target.value)} onKeyDown={(e) => e.key === 'Enter' && search()} />
          <select value={f.key_id} onChange={(e) => set('key_id', e.target.value)} aria-label="Key">
            <option value="">全部 Key</option>
            {keys.data?.items.map((k) => <option key={k.id} value={k.id}>{k.name}</option>)}
          </select>
          <select value={f.status} onChange={(e) => set('status', e.target.value)} aria-label="状态">
            <option value="">全部状态</option>
            {Object.entries(STATUS_LABEL).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
          </select>
          <input type="datetime-local" value={f.start} onChange={(e) => set('start', e.target.value)} aria-label="开始时间" />
          <span className="muted">至</span>
          <input type="datetime-local" value={f.end} onChange={(e) => set('end', e.target.value)} aria-label="结束时间" />
          <button className="primary" onClick={search}>查询</button>
          <button onClick={reset}>重置</button>
          <button onClick={reload}>刷新</button>
        </div>
      </div>
      {error && <div className="alert err">{error}</div>}
      {applied.request_id && data && data.total === 0 && !loading && <div className="alert warn">没有找到请求 ID 为 <code>{applied.request_id}</code> 的记录。请求 ID 为精确匹配，请确认填写完整。</div>}
      {data && <LogTable items={data.items} onOpen={setOpen} />}
      {data && <Pager page={page} pageSize={20} total={data.total} onChange={setPage} />}
      {open && <LogDetail requestId={open} onClose={() => setOpen(null)} />}
    </>
  )
}
