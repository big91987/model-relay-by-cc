import { useState } from 'react'
import { api, fmtTime, yuan } from '../api'
import { Field, Modal, useLoad, useUI } from '../ui'
import { LogDetail } from './Logs'

type Resv = {
  id: number; request_id: string; key_name: string; kind: string; amount: number; dispatched: boolean; note: string
  created_at: number; updated_at: number; model: string; endpoint: string; log_error: string; auto_settle_at: number
}

export default function Billing() {
  const { data, error, reload } = useLoad<{ items: Resv[]; ttl_seconds: number }>(() => api('/reservations'), [])
  const [settling, setSettling] = useState<Resv | null>(null)
  const [amount, setAmount] = useState('')
  const [note, setNote] = useState('')
  const [open, setOpen] = useState<string | null>(null)
  const ui = useUI()
  const resolve = async (r: Resv, action: 'settle' | 'release', micro = 0) => {
    try {
      await api(`/reservations/${r.id}/resolve`, { method: 'POST', body: { action, amount: micro, note } })
      ui.toast(action === 'release' ? '已释放预留，未计费' : `已按 ${yuan(micro)} 结算`, 'ok')
      setSettling(null)
      setNote('')
      reload()
    } catch (e: any) {
      ui.toast(e.message, 'err')
    }
  }
  const release = async (r: Resv) => {
    if (await ui.confirm({ title: '释放预留', content: `确认请求 ${r.request_id} 没有在上游产生费用？释放后这 ${yuan(r.amount)} 额度将恢复可用，且不记费用。`, okText: '确认未产生费用' })) resolve(r, 'release')
  }
  const hours = data ? Math.round(data.ttl_seconds / 360) / 10 : 24
  return (
    <>
      <div className="page-head">
        <div>
          <h1>账务核实</h1>
          <p>
            请求已发往上游，但因超时、断连或网关重启而没有拿到用量时，费用未知：预留的额度会继续占用，不会被当作免费放行。
            请对照上游账单核实后结算或释放；超过 {hours} 小时未处理的，将按预留上界自动结算并标记为“估算”。
          </p>
        </div>
        <button onClick={reload}>刷新</button>
      </div>
      {error && <div className="alert err">{error}</div>}
      <div className="table-wrap">
        <table>
          <thead><tr><th>时间</th><th>请求 ID</th><th>Key</th><th>模型</th><th className="right">占用额度（费用上界）</th><th>原因</th><th>自动结算时间</th><th></th></tr></thead>
          <tbody>
            {data?.items.map((r) => (
              <tr key={r.id}>
                <td className="small nowrap">{fmtTime(r.created_at)}</td>
                <td><button className="link mono" onClick={() => setOpen(r.request_id)}>{r.request_id}</button></td>
                <td>{r.key_name}</td>
                <td className="mono small">{r.model}</td>
                <td className="right">{yuan(r.amount)}</td>
                <td className="small">{r.note || r.log_error}</td>
                <td className="small nowrap">{fmtTime(r.auto_settle_at)}</td>
                <td className="nowrap right">
                  <button className="sm primary" onClick={() => { setSettling(r); setAmount(String(r.amount / 1e6)); setNote('') }}>核实结算</button>{' '}
                  <button className="sm" onClick={() => release(r)}>未产生费用</button>
                </td>
              </tr>
            ))}
            {data && data.items.length === 0 && <tr><td colSpan={8} className="empty">没有待核实的预留，账务清晰。</td></tr>}
          </tbody>
        </table>
      </div>
      {settling && (
        <Modal
          title="核实结算"
          onClose={() => setSettling(null)}
          footer={<><button onClick={() => setSettling(null)}>取消</button><button className="primary" disabled={!(Number(amount) >= 0) || amount === ''} onClick={() => resolve(settling, 'settle', Math.round(Number(amount) * 1e6))}>确认结算</button></>}
        >
          <p style={{ marginTop: 0 }}>请求 <code>{settling.request_id}</code> 预留了 {yuan(settling.amount)}。请填写对照上游账单核实后的实际费用。</p>
          <Field label="实际费用（元）"><input type="number" min={0} step="any" value={amount} onChange={(e) => setAmount(e.target.value)} autoFocus /></Field>
          <Field label="备注（可选）"><input value={note} onChange={(e) => setNote(e.target.value)} placeholder="例如：已对照上游 11 月账单" /></Field>
        </Modal>
      )}
      {open && <LogDetail requestId={open} onClose={() => setOpen(null)} />}
    </>
  )
}
