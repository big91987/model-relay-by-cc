import { useState } from 'react'
import { api, fmtTime, toLocalInput, fromLocalInput, yuan } from '../api'
import { Badge, Field, Modal, useLoad, useUI } from '../ui'

type Key = {
  id: number; name: string; key_prefix: string; status: number; quota_limit: number; used_cost: number; reserved_cost: number
  rpm_limit: number; max_concurrency: number; models: string[]; expires_at: number; created_at: number; last_used_at: number; inflight: number
}

export default function Keys() {
  const { data, error, reload } = useLoad<{ items: Key[] }>(() => api('/keys'), [])
  const [editing, setEditing] = useState<Key | null>(null)
  const [created, setCreated] = useState<string | null>(null)
  const ui = useUI()
  const toggle = (k: Key) => api('/keys/' + k.id, { method: 'PUT', body: { ...k, status: k.status === 1 ? 0 : 1 } }).then(reload).catch((e) => ui.toast(e.message, 'err'))
  const remove = async (k: Key) => {
    if (!(await ui.confirm({ title: '删除 Key', content: `确定删除“${k.name}”吗？使用该 Key 的调用将立即失效，历史日志会保留。`, danger: true, okText: '删除' }))) return
    await api('/keys/' + k.id, { method: 'DELETE' }).then(() => { ui.toast('已删除', 'ok'); reload() }).catch((e) => ui.toast(e.message, 'err'))
  }
  return (
    <>
      <div className="page-head">
        <div>
          <h1>网关 Key</h1>
          <p>调用方使用网关 Key 访问 /v1 接口。可用额度 = 额度上限 − 已用 − 在途/待核实占用。</p>
        </div>
        <button className="primary" onClick={() => setEditing({ id: 0, name: '', key_prefix: '', status: 1, quota_limit: -1, used_cost: 0, reserved_cost: 0, rpm_limit: 0, max_concurrency: 0, models: [], expires_at: 0, created_at: 0, last_used_at: 0, inflight: 0 })}>创建 Key</button>
      </div>
      {error && <div className="alert err">{error}</div>}
      <div className="table-wrap">
        <table>
          <thead><tr><th>名称</th><th>Key</th><th>额度</th><th>已用</th><th>在途占用</th><th>限流</th><th>模型权限</th><th>状态</th><th>最近使用</th><th></th></tr></thead>
          <tbody>
            {data?.items.map((k) => (
              <tr key={k.id}>
                <td>{k.name}</td>
                <td className="mono small">{k.key_prefix}</td>
                <td className="nowrap">{k.quota_limit < 0 ? <Badge>无限</Badge> : <>{yuan(k.quota_limit, 4)}<div className="hint">可用 {yuan(Math.max(0, k.quota_limit - k.used_cost - k.reserved_cost), 4)}</div></>}</td>
                <td className="nowrap">{yuan(k.used_cost)}</td>
                <td className="nowrap">{k.reserved_cost > 0 ? yuan(k.reserved_cost) : '-'}{k.inflight > 0 && <div className="hint">{k.inflight} 个请求进行中</div>}</td>
                <td className="small nowrap">{k.rpm_limit > 0 ? `${k.rpm_limit} 次/分钟` : '不限频率'}<br />{k.max_concurrency > 0 ? `并发 ≤ ${k.max_concurrency}` : '不限并发'}</td>
                <td className="small">{k.models.length ? <span title={k.models.join('\n')}>{k.models.length} 个模型</span> : '全部模型'}</td>
                <td>{k.status !== 1 ? <Badge>已禁用</Badge> : k.expires_at > 0 && k.expires_at < Date.now() ? <Badge cls="err">已过期</Badge> : <Badge cls="ok">启用</Badge>}</td>
                <td className="small nowrap">{fmtTime(k.last_used_at)}</td>
                <td className="nowrap right">
                  <button className="sm" onClick={() => toggle(k)}>{k.status === 1 ? '禁用' : '启用'}</button>{' '}
                  <button className="sm" onClick={() => setEditing(k)}>编辑</button>{' '}
                  <button className="sm danger" onClick={() => remove(k)}>删除</button>
                </td>
              </tr>
            ))}
            {data && data.items.length === 0 && <tr><td colSpan={10} className="empty">还没有网关 Key。</td></tr>}
          </tbody>
        </table>
      </div>
      {editing && <KeyForm initial={editing} onClose={() => setEditing(null)} onSaved={(secret) => { setEditing(null); reload(); if (secret) setCreated(secret) }} />}
      {created && (
        <Modal title="Key 已创建" onClose={() => setCreated(null)} footer={<button className="primary" onClick={() => setCreated(null)}>我已保存</button>}>
          <div className="alert warn">完整的 Key 只显示这一次，服务端仅保存哈希，关闭后无法再次查看。</div>
          <div className="row">
            <code style={{ flex: 1, padding: '8px 10px', wordBreak: 'break-all' }}>{created}</code>
            <button onClick={() => navigator.clipboard.writeText(created).then(() => ui.toast('已复制', 'ok'), () => ui.toast('复制失败，请手动选择复制', 'err'))}>复制</button>
          </div>
          <p className="hint">在控制台的 Playground 等页面可以直接“选择已有 Key”，无需复制密钥。</p>
        </Modal>
      )}
    </>
  )
}

function KeyForm({ initial, onClose, onSaved }: { initial: Key; onClose: () => void; onSaved: (secret?: string) => void }) {
  const [k, setK] = useState(initial)
  const [unlimited, setUnlimited] = useState(initial.quota_limit < 0)
  const [quota, setQuota] = useState(initial.quota_limit < 0 ? '10' : String(initial.quota_limit / 1e6))
  const { data } = useLoad<{ items: { name: string; kind: string }[] }>(() => api('/models'), [])
  const ui = useUI()
  const set = (p: Partial<Key>) => setK((x) => ({ ...x, ...p }))
  const save = async () => {
    const body = { ...k, quota_limit: unlimited ? -1 : Math.round(Math.max(0, Number(quota) || 0) * 1e6) }
    try {
      const d = await api(k.id ? '/keys/' + k.id : '/keys', { method: k.id ? 'PUT' : 'POST', body })
      ui.toast('已保存', 'ok')
      onSaved(d.key)
    } catch (e: any) {
      ui.toast(e.message, 'err')
    }
  }
  const all = (data?.items || []).filter((m) => m.kind !== 'voice').map((m) => m.name)
  return (
    <Modal title={k.id ? '编辑 Key' : '创建 Key'} onClose={onClose} footer={<><button onClick={onClose}>取消</button><button className="primary" disabled={!k.name.trim()} onClick={save}>保存</button></>}>
      <Field label="名称"><input value={k.name} onChange={(e) => set({ name: e.target.value })} placeholder="例如：财务系统" autoFocus /></Field>
      <Field label="额度" hint={k.id ? `已用 ${yuan(k.used_cost)}，在途/待核实占用 ${yuan(k.reserved_cost)}` : undefined}>
        <div className="row">
          <label className="check"><input type="radio" checked={unlimited} onChange={() => setUnlimited(true)} />无限额度</label>
          <label className="check"><input type="radio" checked={!unlimited} onChange={() => setUnlimited(false)} />有限额度</label>
          {!unlimited && <><input type="number" min={0} step="any" style={{ width: 140 }} value={quota} onChange={(e) => setQuota(e.target.value)} aria-label="额度（元）" /><span>元</span></>}
        </div>
      </Field>
      <div className="grid cols-2">
        <Field label="每分钟请求数上限（RPM，0 = 不限）"><input type="number" min={0} value={k.rpm_limit} onChange={(e) => set({ rpm_limit: Number(e.target.value) })} /></Field>
        <Field label="在途请求数上限（并发，0 = 不限）" hint="超过上限时返回 429，请求结束后释放名额"><input type="number" min={0} value={k.max_concurrency} onChange={(e) => set({ max_concurrency: Number(e.target.value) })} /></Field>
      </div>
      <Field label="过期时间（留空 = 永不过期）">
        <input type="datetime-local" value={k.expires_at ? toLocalInput(k.expires_at) : ''} onChange={(e) => set({ expires_at: fromLocalInput(e.target.value) })} />
      </Field>
      <Field label="模型权限" hint="不勾选任何模型表示允许全部模型">
        <div className="chips">
          {all.map((m) => (
            <label key={m} className="check badge">
              <input type="checkbox" checked={k.models.includes(m)} onChange={(e) => set({ models: e.target.checked ? [...k.models, m] : k.models.filter((x) => x !== m) })} />
              {m}
            </label>
          ))}
          {all.length === 0 && <span className="muted">暂无模型</span>}
        </div>
      </Field>
    </Modal>
  )
}
