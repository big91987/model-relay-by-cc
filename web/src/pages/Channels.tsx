import { useState } from 'react'
import { PROVIDER_LABEL, api, fmtTime } from '../api'
import { Badge, Field, Modal, useLoad, useUI } from '../ui'

type CM = { model: string; upstream_model: string }
type Channel = {
  id: number; name: string; type: string; base_url: string; api_key?: string; api_key_masked: string; group_id: string
  status: number; priority: number; weight: number; models: CM[]; updated_at: number
}
const DEFAULT_URL: Record<string, string> = {
  openai: '',
  deepseek: 'https://api.deepseek.com/v1',
  zhipu: 'https://open.bigmodel.cn/api/paas/v4',
  minimax: 'https://api.minimaxi.com',
  volcengine: 'https://ark.cn-beijing.volces.com/api/v3',
}
const blank = (): Channel => ({ id: 0, name: '', type: 'openai', base_url: '', api_key: '', api_key_masked: '', group_id: '', status: 1, priority: 0, weight: 1, models: [], updated_at: 0 })

export default function Channels() {
  const { data, error, reload } = useLoad<{ items: Channel[] }>(() => api('/channels'), [])
  const [editing, setEditing] = useState<Channel | null>(null)
  const ui = useUI()
  const remove = async (c: Channel) => {
    if (!(await ui.confirm({ title: '删除渠道', content: `确定删除渠道“${c.name}”吗？该渠道下的模型将不再可用。`, danger: true, okText: '删除' }))) return
    await api('/channels/' + c.id, { method: 'DELETE' }).then(() => { ui.toast('已删除', 'ok'); reload() }).catch((e) => ui.toast(e.message, 'err'))
  }
  const toggle = async (c: Channel) => {
    await api('/channels/' + c.id, { method: 'PUT', body: { ...c, status: c.status === 1 ? 0 : 1, api_key: '' } }).then(reload).catch((e) => ui.toast(e.message, 'err'))
  }
  const test = async (c: Channel) => {
    const m = c.models[0]
    if (!m) return ui.toast('该渠道没有模型', 'err')
    ui.toast(`正在用模型 ${m.model} 测试…`)
    const r = await api(`/channels/${c.id}/test`, { method: 'POST', body: { model: m.model } }).catch((e) => ({ ok: false, message: e.message }))
    ui.toast((r.ok ? '✓ ' : '✗ ') + r.message + (r.latency_ms ? `（${r.latency_ms} ms）` : ''), r.ok ? 'ok' : 'err')
  }
  return (
    <>
      <div className="page-head">
        <div>
          <h1>渠道</h1>
          <p>渠道是网关访问上游厂商的通道。渠道密钥只保存在服务端，不会下发到浏览器。</p>
        </div>
        <button className="primary" onClick={() => setEditing(blank())}>新建渠道</button>
      </div>
      {error && <div className="alert err">{error}</div>}
      <div className="table-wrap">
        <table>
          <thead>
            <tr><th>名称</th><th>类型</th><th>Base URL</th><th>密钥</th><th>模型</th><th>优先级</th><th>状态</th><th>更新时间</th><th></th></tr>
          </thead>
          <tbody>
            {data?.items.map((c) => (
              <tr key={c.id}>
                <td>{c.name}</td>
                <td>{PROVIDER_LABEL[c.type] || c.type}</td>
                <td className="mono small">{c.base_url}</td>
                <td className="mono small">{c.api_key_masked || <span className="muted">未设置</span>}</td>
                <td>
                  <span title={c.models.map((m) => (m.model === m.upstream_model ? m.model : `${m.model} → ${m.upstream_model}`)).join('\n')}>{c.models.length} 个</span>
                </td>
                <td>{c.priority}</td>
                <td>{c.status === 1 ? <Badge cls="ok">启用</Badge> : <Badge>停用</Badge>}</td>
                <td className="small nowrap">{fmtTime(c.updated_at)}</td>
                <td className="nowrap right">
                  <button className="sm" onClick={() => test(c)}>测试</button>{' '}
                  <button className="sm" onClick={() => toggle(c)}>{c.status === 1 ? '停用' : '启用'}</button>{' '}
                  <button className="sm" onClick={() => setEditing({ ...c, api_key: '', models: c.models.map((m) => ({ ...m })) })}>编辑</button>{' '}
                  <button className="sm danger" onClick={() => remove(c)}>删除</button>
                </td>
              </tr>
            ))}
            {data && data.items.length === 0 && <tr><td colSpan={9} className="empty">还没有渠道，点击右上角“新建渠道”添加上游。</td></tr>}
          </tbody>
        </table>
      </div>
      {editing && <ChannelForm initial={editing} onClose={() => setEditing(null)} onSaved={() => { setEditing(null); reload() }} />}
    </>
  )
}

function ChannelForm({ initial, onClose, onSaved }: { initial: Channel; onClose: () => void; onSaved: () => void }) {
  const [c, setC] = useState<Channel>(initial)
  const [fetched, setFetched] = useState<string[] | null>(null)
  const [picked, setPicked] = useState<Set<string>>(new Set())
  const [fetching, setFetching] = useState(false)
  const [fetchErr, setFetchErr] = useState('')
  const [filter, setFilter] = useState('')
  const [manual, setManual] = useState('')
  const [saving, setSaving] = useState(false)
  const ui = useUI()
  const set = (patch: Partial<Channel>) => setC((x) => ({ ...x, ...patch }))
  const has = (m: string) => c.models.some((x) => x.upstream_model === m || x.model === m)

  const fetchModels = async () => {
    setFetching(true)
    setFetchErr('')
    try {
      const d = await api('/channels/fetch-models', { method: 'POST', body: { channel_id: c.id, type: c.type, base_url: c.base_url || DEFAULT_URL[c.type], api_key: c.api_key } })
      setFetched(d.models)
      setPicked(new Set())
    } catch (e: any) {
      // 获取失败：保留已有模型配置，不做任何改动
      setFetchErr(e.message + '。已有的模型配置保持不变，你仍可以手动填写。')
    } finally {
      setFetching(false)
    }
  }
  const importPicked = () => {
    const add = [...picked].filter((m) => !has(m)).map((m) => ({ model: m, upstream_model: m }))
    set({ models: [...c.models, ...add] })
    setPicked(new Set())
    ui.toast(`已导入 ${add.length} 个模型`, 'ok')
  }
  const addManual = () => {
    const names = manual.split(/[\s,，]+/).map((s) => s.trim()).filter(Boolean).filter((m) => !has(m))
    if (names.length) set({ models: [...c.models, ...names.map((m) => ({ model: m, upstream_model: m }))] })
    setManual('')
  }
  const save = async () => {
    setSaving(true)
    try {
      await api(c.id ? '/channels/' + c.id : '/channels', { method: c.id ? 'PUT' : 'POST', body: { ...c, base_url: c.base_url || DEFAULT_URL[c.type] } })
      ui.toast('渠道已保存', 'ok')
      onSaved()
    } catch (e: any) {
      ui.toast(e.message, 'err')
    } finally {
      setSaving(false)
    }
  }
  const visible = (fetched || []).filter((m) => m.toLowerCase().includes(filter.toLowerCase()))
  return (
    <Modal
      wide
      title={c.id ? '编辑渠道' : '新建渠道'}
      onClose={onClose}
      footer={<><button onClick={onClose}>取消</button><button className="primary" disabled={saving || !c.name.trim()} onClick={save}>{saving ? '保存中…' : '保存'}</button></>}
    >
      <div className="grid cols-2">
        <Field label="渠道名称"><input value={c.name} onChange={(e) => set({ name: e.target.value })} placeholder="例如：DeepSeek 官方" /></Field>
        <Field label="类型">
          <select value={c.type} onChange={(e) => set({ type: e.target.value, base_url: DEFAULT_URL[e.target.value] })}>
            {Object.keys(DEFAULT_URL).map((t) => <option key={t} value={t}>{PROVIDER_LABEL[t]}</option>)}
          </select>
        </Field>
        <Field label="Base URL" hint="OpenAI 兼容类型需包含版本路径，如 https://api.deepseek.com/v1">
          <input value={c.base_url} onChange={(e) => set({ base_url: e.target.value })} placeholder={DEFAULT_URL[c.type] || 'https://…/v1'} />
        </Field>
        <Field label="API 密钥" hint={c.id ? `当前：${c.api_key_masked || '未设置'}；留空表示不修改` : '只保存在服务端'}>
          <input type="password" autoComplete="new-password" value={c.api_key || ''} onChange={(e) => set({ api_key: e.target.value })} placeholder={c.id ? '留空则保留原密钥' : '上游的 API Key'} />
        </Field>
        <Field label="优先级" hint="数值越大越优先；同优先级按权重随机"><input type="number" value={c.priority} onChange={(e) => set({ priority: Number(e.target.value) })} /></Field>
        <Field label="权重"><input type="number" min={1} value={c.weight} onChange={(e) => set({ weight: Number(e.target.value) })} /></Field>
      </div>
      <h3 style={{ marginTop: 8 }}>模型</h3>
      <div className="row" style={{ marginBottom: 10 }}>
        <button onClick={fetchModels} disabled={fetching}>{fetching ? '获取中…' : '从上游获取模型列表'}</button>
        <input style={{ flex: 1, minWidth: 180 }} value={manual} onChange={(e) => setManual(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && addManual()} placeholder="手动填写模型名，多个用逗号或空格分隔" />
        <button onClick={addManual} disabled={!manual.trim()}>添加</button>
      </div>
      {fetchErr && <div className="alert err">{fetchErr}</div>}
      {fetched && (
        <div className="card" style={{ marginBottom: 12, background: 'var(--panel-2)' }}>
          <div className="row" style={{ marginBottom: 8 }}>
            <strong>上游返回 {fetched.length} 个模型</strong>
            <input value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="筛选" style={{ width: 160 }} />
            <button className="sm" onClick={() => setPicked(new Set(visible.filter((m) => !has(m))))}>全选</button>
            <button className="sm" onClick={() => setPicked(new Set())}>清空</button>
            <span className="spacer" />
            <button className="primary sm" disabled={picked.size === 0} onClick={importPicked}>导入所选（{picked.size}）</button>
          </div>
          <div className="chips">
            {visible.map((m) => (
              <label key={m} className="check badge" style={{ opacity: has(m) ? 0.55 : 1 }}>
                <input
                  type="checkbox"
                  disabled={has(m)}
                  checked={has(m) || picked.has(m)}
                  onChange={(e) => {
                    const n = new Set(picked)
                    e.target.checked ? n.add(m) : n.delete(m)
                    setPicked(n)
                  }}
                />
                {m}{has(m) ? '（已添加）' : ''}
              </label>
            ))}
          </div>
        </div>
      )}
      <div className="table-wrap">
        <table>
          <thead><tr><th>对外模型名（别名）</th><th>上游模型名</th><th></th></tr></thead>
          <tbody>
            {c.models.map((m, i) => (
              <tr key={i}>
                <td><input style={{ width: '100%' }} value={m.model} aria-label="对外模型名" onChange={(e) => set({ models: c.models.map((x, j) => (j === i ? { ...x, model: e.target.value } : x)) })} /></td>
                <td><input style={{ width: '100%' }} value={m.upstream_model} aria-label="上游模型名" onChange={(e) => set({ models: c.models.map((x, j) => (j === i ? { ...x, upstream_model: e.target.value } : x)) })} /></td>
                <td className="right"><button className="sm danger" onClick={() => set({ models: c.models.filter((_, j) => j !== i) })}>移除</button></td>
              </tr>
            ))}
            {c.models.length === 0 && <tr><td colSpan={3} className="empty">尚未添加模型。可从上游获取后勾选导入，也可手动填写。</td></tr>}
          </tbody>
        </table>
      </div>
      <p className="hint">调用方使用“对外模型名”；网关转发时替换为“上游模型名”。两者不同即为模型别名。</p>
    </Modal>
  )
}
