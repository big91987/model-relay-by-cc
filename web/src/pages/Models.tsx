import { useState } from 'react'
import { KIND_LABEL, api, yuan } from '../api'
import { Badge, Field, Modal, useLoad, useUI } from '../ui'

type Model = {
  name: string; kind: string; billing_mode: string; price_in: number; price_out: number; price_unit: number; priced: boolean
  max_output_tokens: number; default_max_tokens: number; supports_temperature: boolean; temperature_max: number; note: string
  channels: { id: number; name: string; status: number; upstream_model: string }[]; available: boolean; favorite: boolean
}

export function priceText(m: Model): string {
  if (!m.priced) return '未定价'
  if (m.billing_mode === 'request') return `${yuan(m.price_unit, 4)} / 次`
  if (m.billing_mode === 'char') return `${yuan(m.price_in / 100, 4)} / 万字符`
  const parts = [`输入 ${yuan(m.price_in / 1e6 * 1e6, 4)}`]
  if (m.price_out > 0) parts.push(`输出 ${yuan(m.price_out, 4)}`)
  return parts.join('，') + ' / 百万 Token'
}

export default function Models() {
  const { data, error, reload } = useLoad<{ items: Model[] }>(() => api('/models'), [])
  const [onlyFav, setOnlyFav] = useState(false)
  const [kind, setKind] = useState('')
  const [q, setQ] = useState('')
  const [editing, setEditing] = useState<Model | null>(null)
  const ui = useUI()
  const fav = async (m: Model) => {
    try {
      await api('/favorites', { method: 'PUT', body: { model: m.name, favorite: !m.favorite } })
      reload()
    } catch (e: any) {
      ui.toast(e.message, 'err')
    }
  }
  const remove = async (m: Model) => {
    if (!(await ui.confirm({ title: '删除模型配置', content: `确定删除模型“${m.name}”的配置吗？`, danger: true, okText: '删除' }))) return
    await api('/models?name=' + encodeURIComponent(m.name), { method: 'DELETE' }).then(reload).catch((e) => ui.toast(e.message, 'err'))
  }
  const items = (data?.items || []).filter((m) => (!onlyFav || m.favorite) && (!kind || m.kind === kind) && m.name.toLowerCase().includes(q.toLowerCase()))
  const favCount = (data?.items || []).filter((m) => m.favorite).length
  return (
    <>
      <div className="page-head">
        <div>
          <h1>模型</h1>
          <p>模型来自各渠道。价格用于额度控制与记账：未定价的模型只能被无限额度的 Key 调用。</p>
        </div>
        <button className="primary" onClick={() => setEditing({ name: '', kind: 'chat', billing_mode: 'token', price_in: 0, price_out: 0, price_unit: 0, priced: false, max_output_tokens: 0, default_max_tokens: 0, supports_temperature: true, temperature_max: 2, note: '', channels: [], available: false, favorite: false })}>新建模型配置</button>
      </div>
      {error && <div className="alert err">{error}</div>}
      <div className="row" style={{ marginBottom: 12 }}>
        <input placeholder="搜索模型名" value={q} onChange={(e) => setQ(e.target.value)} style={{ width: 220 }} />
        <select value={kind} onChange={(e) => setKind(e.target.value)} aria-label="类型">
          <option value="">全部类型</option>
          {Object.entries(KIND_LABEL).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
        </select>
        <label className="check">
          <input type="checkbox" checked={onlyFav} onChange={(e) => setOnlyFav(e.target.checked)} />
          仅看收藏（{favCount}）
        </label>
      </div>
      <div className="table-wrap">
        <table>
          <thead><tr><th style={{ width: 36 }}></th><th>模型</th><th>类型</th><th>渠道</th><th>价格</th><th>输出上限</th><th>状态</th><th></th></tr></thead>
          <tbody>
            {items.map((m) => (
              <tr key={m.name}>
                <td>
                  <button className={'star' + (m.favorite ? ' on' : '')} title={m.favorite ? '取消收藏' : '收藏'} aria-label={m.favorite ? '取消收藏' : '收藏'} aria-pressed={m.favorite} onClick={() => fav(m)}>
                    {m.favorite ? '★' : '☆'}
                  </button>
                </td>
                <td><span className="mono">{m.name}</span>{m.note && <div className="hint">{m.note}</div>}</td>
                <td>{KIND_LABEL[m.kind] || m.kind}</td>
                <td className="small">
                  {m.channels.length === 0 ? <span className="muted">无</span> : m.channels.map((c) => (
                    <div key={c.id}>{c.name}{c.upstream_model !== m.name && <span className="muted"> → {c.upstream_model}</span>}{c.status !== 1 && <span className="muted">（停用）</span>}</div>
                  ))}
                </td>
                <td className="small">{m.priced ? priceText(m) : <Badge cls="warn">未定价</Badge>}</td>
                <td className="small">{m.kind === 'chat' ? (m.max_output_tokens || '未限制') : '-'}</td>
                <td>{m.available ? <Badge cls="ok">可用</Badge> : <Badge>无可用渠道</Badge>}</td>
                <td className="nowrap right">
                  <button className="sm" onClick={() => setEditing(m)}>编辑</button>{' '}
                  {m.channels.length === 0 && <button className="sm danger" onClick={() => remove(m)}>删除</button>}
                </td>
              </tr>
            ))}
            {data && items.length === 0 && <tr><td colSpan={8} className="empty">{onlyFav ? '还没有收藏的模型，点击模型前的 ☆ 即可收藏。' : '没有匹配的模型。'}</td></tr>}
          </tbody>
        </table>
      </div>
      {editing && <ModelForm initial={editing} onClose={() => setEditing(null)} onSaved={() => { setEditing(null); reload() }} />}
    </>
  )
}

function ModelForm({ initial, onClose, onSaved }: { initial: Model; onClose: () => void; onSaved: () => void }) {
  const isNew = !initial.name
  const [m, setM] = useState(initial)
  // 表单里用“元”为单位：Token 为 元/百万Token，字符为 元/万字符，按次为 元/次
  const [pin, setPin] = useState(String(initial.billing_mode === 'char' ? initial.price_in / 1e8 : initial.price_in / 1e6))
  const [pout, setPout] = useState(String(initial.price_out / 1e6))
  const [punit, setPunit] = useState(String(initial.price_unit / 1e6))
  const ui = useUI()
  const set = (p: Partial<Model>) => setM((x) => ({ ...x, ...p }))
  const save = async () => {
    const n = (s: string) => Math.max(0, Number(s) || 0)
    const body = {
      ...m,
      price_in: Math.round(n(pin) * (m.billing_mode === 'char' ? 1e8 : 1e6)),
      price_out: Math.round(n(pout) * 1e6),
      price_unit: Math.round(n(punit) * 1e6),
    }
    try {
      await api('/models', { method: 'PUT', body })
      ui.toast('模型配置已保存', 'ok')
      onSaved()
    } catch (e: any) {
      ui.toast(e.message, 'err')
    }
  }
  return (
    <Modal title={isNew ? '新建模型配置' : `编辑模型：${m.name}`} onClose={onClose} footer={<><button onClick={onClose}>取消</button><button className="primary" disabled={!m.name.trim()} onClick={save}>保存</button></>}>
      {isNew && <Field label="模型名称"><input value={m.name} onChange={(e) => set({ name: e.target.value })} /></Field>}
      <div className="grid cols-2">
        <Field label="类型">
          <select value={m.kind} onChange={(e) => set({ kind: e.target.value })}>{Object.entries(KIND_LABEL).map(([k, v]) => <option key={k} value={k}>{v}</option>)}</select>
        </Field>
        <Field label="计费方式">
          <select value={m.billing_mode} onChange={(e) => set({ billing_mode: e.target.value })}>
            <option value="token">按 Token</option><option value="char">按字符</option><option value="request">按次</option>
          </select>
        </Field>
        {m.billing_mode === 'token' && <>
          <Field label="输入价格（元 / 百万 Token）"><input type="number" min={0} step="any" value={pin} onChange={(e) => setPin(e.target.value)} /></Field>
          <Field label="输出价格（元 / 百万 Token）"><input type="number" min={0} step="any" value={pout} onChange={(e) => setPout(e.target.value)} /></Field>
        </>}
        {m.billing_mode === 'char' && <Field label="价格（元 / 万字符）" hint="按上游计费字符数，中文每字计 2 字符"><input type="number" min={0} step="any" value={pin} onChange={(e) => setPin(e.target.value)} /></Field>}
        {m.billing_mode === 'request' && <Field label="价格（元 / 次）"><input type="number" min={0} step="any" value={punit} onChange={(e) => setPunit(e.target.value)} /></Field>}
      </div>
      <Field label="">
        <label className="check"><input type="checkbox" checked={m.priced} onChange={(e) => set({ priced: e.target.checked })} />已核对定价（勾选后，有限额度的 Key 才能调用该模型；价格为 0 表示免费）</label>
      </Field>
      {m.kind === 'chat' && (
        <div className="grid cols-2">
          <Field label="最大输出 Token（0 = 不限制）" hint="请求的 max_tokens 超过该值会被拒绝"><input type="number" min={0} value={m.max_output_tokens} onChange={(e) => set({ max_output_tokens: Number(e.target.value) })} /></Field>
          <Field label="默认输出上限（0 = 使用全局 4096）" hint="有限额度 Key 未指定 max_tokens 时，网关注入该上限"><input type="number" min={0} value={m.default_max_tokens} onChange={(e) => set({ default_max_tokens: Number(e.target.value) })} /></Field>
          <Field label="温度上限"><input type="number" min={0} step="0.1" value={m.temperature_max} disabled={!m.supports_temperature} onChange={(e) => set({ temperature_max: Number(e.target.value) })} /></Field>
          <Field label={"\u00a0"}><label className="check"><input type="checkbox" checked={m.supports_temperature} onChange={(e) => set({ supports_temperature: e.target.checked })} />支持调节温度</label></Field>
        </div>
      )}
      <Field label="备注"><input value={m.note} onChange={(e) => set({ note: e.target.value })} /></Field>
    </Modal>
  )
}
