import { ReactNode, createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'

// —— 弹窗 ——
export function Modal(props: { title: string; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  useEffect(() => {
    const h = (e: KeyboardEvent) => e.key === 'Escape' && props.onClose()
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  })
  return (
    <div className="modal-mask" onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div className={'modal' + (props.wide ? ' wide' : '')} role="dialog" aria-modal="true" aria-label={props.title}>
        <div className="modal-head">
          <span>{props.title}</span>
          <button className="link" onClick={props.onClose} aria-label="关闭">✕</button>
        </div>
        <div className="modal-body">{props.children}</div>
        {props.footer && <div className="modal-foot">{props.footer}</div>}
      </div>
    </div>
  )
}

// —— 全局提示与确认（不使用浏览器原生 alert/confirm，保证随主题变化）——
type Toast = { id: number; text: string; kind: 'ok' | 'err' | 'info' }
type ConfirmOpts = { title: string; content: ReactNode; okText?: string; danger?: boolean }
type Ctx = { toast: (text: string, kind?: Toast['kind']) => void; confirm: (o: ConfirmOpts) => Promise<boolean> }
const UICtx = createContext<Ctx>({ toast: () => {}, confirm: async () => false })
export const useUI = () => useContext(UICtx)

export function UIProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const [cf, setCf] = useState<(ConfirmOpts & { resolve: (v: boolean) => void }) | null>(null)
  const seq = useRef(0)
  const toast = useCallback((text: string, kind: Toast['kind'] = 'info') => {
    const id = ++seq.current
    setToasts((t) => [...t, { id, text, kind }])
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), kind === 'err' ? 6000 : 3000)
  }, [])
  const confirm = useCallback((o: ConfirmOpts) => new Promise<boolean>((resolve) => setCf({ ...o, resolve })), [])
  const close = (v: boolean) => {
    cf?.resolve(v)
    setCf(null)
  }
  return (
    <UICtx.Provider value={{ toast, confirm }}>
      {children}
      <div className="toasts">
        {toasts.map((t) => (
          <div key={t.id} className={'toast ' + t.kind}>{t.text}</div>
        ))}
      </div>
      {cf && (
        <Modal
          title={cf.title}
          onClose={() => close(false)}
          footer={
            <>
              <button onClick={() => close(false)}>取消</button>
              <button className={cf.danger ? 'danger solid' : 'primary'} onClick={() => close(true)} autoFocus>
                {cf.okText || '确定'}
              </button>
            </>
          }
        >
          {cf.content}
        </Modal>
      )}
    </UICtx.Provider>
  )
}

export function Pager(props: { page: number; pageSize: number; total: number; onChange: (p: number) => void }) {
  const pages = Math.max(1, Math.ceil(props.total / props.pageSize))
  return (
    <div className="row" style={{ justifyContent: 'flex-end', marginTop: 12 }}>
      <span className="muted small">共 {props.total} 条，第 {props.page} / {pages} 页</span>
      <button className="sm" disabled={props.page <= 1} onClick={() => props.onChange(props.page - 1)}>上一页</button>
      <button className="sm" disabled={props.page >= pages} onClick={() => props.onChange(props.page + 1)}>下一页</button>
    </div>
  )
}

export function Field(props: { label: ReactNode; hint?: ReactNode; children: ReactNode }) {
  return (
    <div className="field">
      <label>{props.label}</label>
      {props.children}
      {props.hint && <div className="hint">{props.hint}</div>}
    </div>
  )
}

export function Badge({ cls, children }: { cls?: string; children: ReactNode }) {
  return <span className={'badge ' + (cls || '')}>{children}</span>
}

// useLoad：加载数据并暴露刷新函数
export function useLoad<T>(fn: () => Promise<T>, deps: any[]): { data: T | null; error: string; loading: boolean; reload: () => void } {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)
  useEffect(() => {
    let alive = true
    setLoading(true)
    fn()
      .then((d) => alive && (setData(d), setError('')))
      .catch((e) => alive && setError(e.message || '加载失败'))
      .finally(() => alive && setLoading(false))
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick])
  return { data, error, loading, reload: () => setTick((t) => t + 1) }
}
