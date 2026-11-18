import { ReactNode, createContext, useContext, useEffect, useState } from 'react'
import { KeyAuth, api, gw } from './api'

// 当前页面会话使用的网关 Key。只保存在内存（React state）中：
// 不写入 URL、localStorage/sessionStorage 或日志，刷新页面即丢失。
type Ctx = { auth: KeyAuth | null; setAuth: (a: KeyAuth | null) => void }
const KeyCtx = createContext<Ctx>({ auth: null, setAuth: () => {} })
export const useKeyAuth = () => useContext(KeyCtx)

export function KeyProvider({ children }: { children: ReactNode }) {
  const [auth, setAuth] = useState<KeyAuth | null>(null)
  return <KeyCtx.Provider value={{ auth, setAuth }}>{children}</KeyCtx.Provider>
}

type KeyRow = { id: number; name: string; key_prefix: string; status: number; quota_limit: number; used_cost: number; reserved_cost: number }

export function KeyPicker() {
  const { auth, setAuth } = useKeyAuth()
  const [keys, setKeys] = useState<KeyRow[]>([])
  const [mode, setMode] = useState<'id' | 'secret'>(auth?.mode || 'id')
  const [secret, setSecret] = useState('')
  useEffect(() => {
    api('/keys').then((d) => setKeys((d.items as KeyRow[]).filter((k) => k.status === 1))).catch(() => {})
  }, [])
  return (
    <div className="row">
      <div className="seg">
        <button className={mode === 'id' ? 'on' : ''} onClick={() => { setMode('id'); setAuth(null) }}>选择已有 Key</button>
        <button className={mode === 'secret' ? 'on' : ''} onClick={() => { setMode('secret'); setAuth(null) }}>手动输入 Key</button>
      </div>
      {mode === 'id' ? (
        <select
          aria-label="选择网关 Key"
          value={auth?.mode === 'id' ? auth.id : ''}
          onChange={(e) => {
            const k = keys.find((x) => x.id === Number(e.target.value))
            setAuth(k ? { mode: 'id', id: k.id, name: k.name } : null)
          }}
        >
          <option value="">{keys.length ? '请选择网关 Key' : '暂无可用 Key，请先到“网关 Key”页创建'}</option>
          {keys.map((k) => (
            <option key={k.id} value={k.id}>{k.name}（{k.key_prefix}）</option>
          ))}
        </select>
      ) : (
        <>
          <input
            type="password"
            autoComplete="off"
            placeholder="粘贴网关 Key（sk-relay-…）"
            aria-label="网关 Key"
            style={{ width: 280 }}
            value={secret}
            onChange={(e) => { setSecret(e.target.value); setAuth(null) }}
          />
          <button disabled={!secret.trim()} onClick={() => setAuth({ mode: 'secret', secret: secret.trim() })}>使用</button>
          {auth?.mode === 'secret' && <span className="badge ok">已使用（仅本页会话有效）</span>}
        </>
      )}
    </div>
  )
}

export type GwModel = {
  id: string; kind: string; priced: boolean; providers: string[]
  supports_temperature: boolean; temperature_max: number; max_output_tokens: number; default_max_tokens: number
}

// useModels：加载当前 Key 可用的某类模型
export function useModels(kind: string): { models: GwModel[]; error: string; loading: boolean } {
  const { auth } = useKeyAuth()
  const [models, setModels] = useState<GwModel[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  useEffect(() => {
    setModels([])
    setError('')
    if (!auth) return
    let alive = true
    setLoading(true)
    gw('/models?kind=' + kind, auth)
      .then((d) => alive && setModels(d.data || []))
      .catch((e) => alive && setError(e.message))
      .finally(() => alive && setLoading(false))
    return () => { alive = false }
  }, [auth, kind])
  return { models, error, loading }
}

export function NeedKey({ models, error, loading, kindLabel }: { models: GwModel[]; error: string; loading: boolean; kindLabel: string }) {
  const { auth } = useKeyAuth()
  if (!auth) return <div className="alert info">请先在上方选择或输入一个网关 Key。调用会经过真实网关，沿用该 Key 的模型权限、额度、限流并记入请求日志。</div>
  if (error) return <div className="alert err">{error}</div>
  if (!loading && models.length === 0) return <div className="alert warn">该 Key 没有可用的{kindLabel}模型：请检查渠道是否已配置并启用对应模型，或该 Key 的模型权限。</div>
  return null
}
