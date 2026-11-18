import { useState } from 'react'
import { KIND_LABEL, api, fmtTime, fromLocalInput, toLocalInput, yuan } from '../api'
import { Pager, useLoad } from '../ui'
import { Log, LogDetail, LogTable } from './Logs'

const DAY = 24 * 3600 * 1000
function preset(days: number): [string, string] {
  const start = new Date().setHours(0, 0, 0, 0) - (days - 1) * DAY
  return [toLocalInput(start), '']
}

export default function Usage() {
  const [range, setRange] = useState<[string, string]>(preset(7))
  const [active, setActive] = useState('7')
  const [page, setPage] = useState(1)
  const [open, setOpen] = useState<string | null>(null)
  const q = new URLSearchParams()
  if (range[0]) q.set('start', String(fromLocalInput(range[0])))
  if (range[1]) q.set('end', String(fromLocalInput(range[1])))
  const { data, error } = useLoad<{ summary: any; by_model: any[]; items: Log[]; total: number }>(() => api(`/usage?${q}&page=${page}&page_size=10`), [range[0], range[1], page])
  const pick = (days: string) => {
    setActive(days)
    setRange(days === 'all' ? ['', ''] : preset(Number(days)))
    setPage(1)
  }
  const custom = (i: 0 | 1, v: string) => {
    setActive('custom')
    setRange(i === 0 ? [v, range[1]] : [range[0], v])
    setPage(1)
  }
  const s = data?.summary
  const rangeText = `${range[0] ? fmtTime(fromLocalInput(range[0])) : '最早'} 至 ${range[1] ? fmtTime(fromLocalInput(range[1])) : '现在'}`
  return (
    <>
      <div className="page-head">
        <div>
          <h1>用量与费用</h1>
          <p>查看所选时间段的费用合计与按模型的费用明细。合计针对整个时间段，与下方明细的翻页无关。</p>
        </div>
      </div>
      <div className="card" style={{ marginBottom: 16 }}>
        <div className="row">
          <div className="seg">
            {[['1', '今天'], ['7', '近 7 天'], ['30', '近 30 天'], ['all', '全部']].map(([d, label]) => (
              <button key={d} className={active === d ? 'on' : ''} onClick={() => pick(d)}>{label}</button>
            ))}
          </div>
          <input type="datetime-local" value={range[0]} onChange={(e) => custom(0, e.target.value)} aria-label="开始时间" />
          <span className="muted">至</span>
          <input type="datetime-local" value={range[1]} onChange={(e) => custom(1, e.target.value)} aria-label="结束时间" />
        </div>
      </div>
      {error && <div className="alert err">{error}</div>}
      {s && (
        <>
          <div className="grid cols-4">
            <div className="card stat">
              <div className="label">费用合计（所选时间段）</div>
              <div className="value" data-testid="total-cost">{yuan(s.cost)}</div>
              <div className="sub">{rangeText}</div>
            </div>
            <div className="card stat"><div className="label">请求数</div><div className="value">{s.requests}</div><div className="sub">成功 {s.success_requests}</div></div>
            <div className="card stat"><div className="label">Token</div><div className="value">{(s.prompt_tokens + s.completion_tokens).toLocaleString()}</div><div className="sub">输入 {s.prompt_tokens.toLocaleString()} / 输出 {s.completion_tokens.toLocaleString()}</div></div>
            <div className="card stat"><div className="label">其中估算费用</div><div className="value">{yuan(s.estimated_cost)}</div><div className="sub">{s.pending_count > 0 ? `另有 ${s.pending_count} 笔费用待核实，未计入合计` : '没有待核实的请求'}</div></div>
          </div>
          <div className="card" style={{ marginTop: 16 }}>
            <h2>按模型汇总</h2>
            <div className="table-wrap">
              <table>
                <thead><tr><th>模型</th><th>类型</th><th className="right">请求数</th><th className="right">输入 Token</th><th className="right">输出 Token</th><th className="right">计费数量</th><th className="right">费用</th><th className="right">占比</th></tr></thead>
                <tbody>
                  {data!.by_model.map((m) => (
                    <tr key={m.model}>
                      <td className="mono">{m.model || '（未指定）'}</td><td>{KIND_LABEL[m.kind] || m.kind}</td><td className="right">{m.requests}</td>
                      <td className="right">{m.prompt_tokens.toLocaleString()}</td><td className="right">{m.completion_tokens.toLocaleString()}</td><td className="right">{m.units || '-'}</td>
                      <td className="right">{yuan(m.cost)}</td><td className="right">{s.cost > 0 ? ((m.cost / s.cost) * 100).toFixed(1) + '%' : '-'}</td>
                    </tr>
                  ))}
                  {data!.by_model.length === 0 && <tr><td colSpan={8} className="empty">该时间段内没有调用。</td></tr>}
                </tbody>
                {data!.by_model.length > 0 && (
                  <tfoot><tr><th>合计</th><th></th><th className="right">{s.requests}</th><th className="right">{s.prompt_tokens.toLocaleString()}</th><th className="right">{s.completion_tokens.toLocaleString()}</th><th></th><th className="right">{yuan(s.cost)}</th><th></th></tr></tfoot>
                )}
              </table>
            </div>
          </div>
          <div className="card" style={{ marginTop: 16 }}>
            <h2>调用明细</h2>
            <LogTable items={data!.items} onOpen={setOpen} />
            <Pager page={page} pageSize={10} total={data!.total} onChange={setPage} />
          </div>
        </>
      )}
      {open && <LogDetail requestId={open} onClose={() => setOpen(null)} />}
    </>
  )
}
