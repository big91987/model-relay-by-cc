import { useEffect, useMemo, useState } from 'react'
import { PROVIDER_LABEL, gw } from '../api'
import { KeyPicker, NeedKey, useKeyAuth, useModels } from '../keyctx'
import { Field, useUI } from '../ui'
import { TaskActions, TaskMeta, TaskStatus, useTasks } from './tasks'

const RESOLUTIONS: Record<string, string[]> = { minimax: ['768P', '480P', '2K'], volcengine: ['720p', '480p', '1080p'] }

export default function Video() {
  const { auth } = useKeyAuth()
  const { models, error, loading } = useModels('video')
  const { tasks, error: taskErr, reload } = useTasks('video')
  const [vendor, setVendor] = useState('')
  const [model, setModel] = useState('')
  const [mode, setMode] = useState<'t2v' | 'i2v'>('t2v')
  const [prompt, setPrompt] = useState('')
  const [image, setImage] = useState('')
  const [duration, setDuration] = useState(5)
  const [resolution, setResolution] = useState('')
  const [busy, setBusy] = useState(false)
  const ui = useUI()

  const vendors = useMemo<string[]>(() => [...new Set(models.flatMap((m) => m.providers || []))].filter((v) => v === 'minimax' || v === 'volcengine'), [models])
  const vendorModels = useMemo(() => models.filter((m) => (m.providers || []).includes(vendor)), [models, vendor])
  useEffect(() => {
    if (vendors.length && !vendors.includes(vendor)) setVendor(vendors[0])
  }, [vendors]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (!vendorModels.some((m) => m.id === model)) setModel(vendorModels[0]?.id || '')
    setResolution((RESOLUTIONS[vendor] || [''])[0])
  }, [vendor, vendorModels]) // eslint-disable-line react-hooks/exhaustive-deps

  const pickImage = (f?: File) => {
    if (!f) return setImage('')
    if (!/^image\/(png|jpe?g|webp)$/.test(f.type)) return ui.toast('首帧图片仅支持 PNG、JPG、WEBP', 'err')
    if (f.size > 10 * 1024 * 1024) return ui.toast('首帧图片请不要超过 10MB', 'err')
    const r = new FileReader()
    r.onload = () => setImage(String(r.result))
    r.readAsDataURL(f)
  }
  const submit = async () => {
    if (!auth) return
    setBusy(true)
    try {
      await gw('/video/generations', auth, { model, prompt, duration, resolution, first_frame_image: mode === 'i2v' ? image : undefined })
      ui.toast('任务已提交，生成通常需要几分钟', 'ok')
      reload()
    } catch (e: any) {
      ui.toast(e.message + (e.requestId ? `（请求 ID：${e.requestId}）` : ''), 'err')
    } finally {
      setBusy(false)
    }
  }
  const ready = !!auth && !!model && !!prompt.trim() && (mode === 't2v' || !!image)
  return (
    <>
      <div className="page-head">
        <div>
          <h1>视频生成</h1>
          <p>支持 MiniMax 与 Seedance 的文生视频、首帧图生视频。任务在后台执行，刷新页面后仍可查看状态、预览和下载。</p>
        </div>
      </div>
      <div className="card" style={{ marginBottom: 16 }}><KeyPicker /></div>
      <NeedKey models={models} error={error} loading={loading} kindLabel="视频" />
      <div className="grid cols-2">
        <div className="card">
          <h2>新建任务</h2>
          <div className="grid cols-2">
            <Field label="厂商">
              <select value={vendor} onChange={(e) => setVendor(e.target.value)} disabled={!vendors.length} aria-label="厂商">
                {!vendors.length && <option value="">无可用厂商</option>}
                {vendors.map((v) => <option key={v} value={v}>{PROVIDER_LABEL[v]}</option>)}
              </select>
            </Field>
            <Field label="模型">
              <select value={model} onChange={(e) => setModel(e.target.value)} disabled={!vendorModels.length} aria-label="模型">
                {vendorModels.map((m) => <option key={m.id} value={m.id}>{m.id}</option>)}
              </select>
            </Field>
          </div>
          <Field label="生成方式">
            <div className="seg">
              <button className={mode === 't2v' ? 'on' : ''} onClick={() => setMode('t2v')}>文生视频</button>
              <button className={mode === 'i2v' ? 'on' : ''} onClick={() => setMode('i2v')}>首帧图生视频</button>
            </div>
          </Field>
          {mode === 'i2v' && (
            <Field label="首帧图片" hint="PNG / JPG / WEBP，不超过 10MB；图片随请求发送给上游，不在网关留存">
              <input type="file" accept="image/png,image/jpeg,image/webp" onChange={(e) => pickImage(e.target.files?.[0])} />
              {image && <img className="thumb" src={image} alt="首帧预览" />}
            </Field>
          )}
          <Field label="画面描述"><textarea rows={4} value={prompt} onChange={(e) => setPrompt(e.target.value)} placeholder="例如：清晨的海边，一只金毛犬迎着朝阳奔跑，镜头缓慢跟随" /></Field>
          <div className="grid cols-2">
            <Field label="时长（秒）"><input type="number" min={4} max={15} value={duration} onChange={(e) => setDuration(Math.max(1, Number(e.target.value) || 5))} /></Field>
            <Field label="分辨率">
              <select value={resolution} onChange={(e) => setResolution(e.target.value)}>{(RESOLUTIONS[vendor] || []).map((r) => <option key={r}>{r}</option>)}</select>
            </Field>
          </div>
          <button className="primary" disabled={!ready || busy} onClick={submit}>{busy ? '提交中…' : '提交生成任务'}</button>
          <p className="hint">视频生成按上游实际结果计费：成功后结算，失败不扣费。提交时会按费用上界预留额度。</p>
        </div>
        <div className="card">
          <div className="row"><h2 style={{ margin: 0 }}>任务列表</h2><span className="spacer" /><button className="sm" onClick={reload}>刷新</button></div>
          {taskErr && <div className="alert err">{taskErr}</div>}
          {tasks.length === 0 && <div className="empty">还没有视频任务。</div>}
          {tasks.map((t) => (
            <div className="task" key={t.id}>
              <div>
                {t.status === 'succeeded' && t.has_file ? (
                  <video controls preload="metadata" src={`/api/tasks/${t.id}/content`} />
                ) : (
                  <div className="ph">{t.status === 'failed' ? '生成失败' : t.status === 'succeeded' ? '成品不可用' : '生成中，完成后可在此预览'}</div>
                )}
              </div>
              <div>
                <div className="row" style={{ marginBottom: 6 }}>
                  <TaskStatus t={t} />
                  <span className="badge">{PROVIDER_LABEL[t.provider] || t.provider}</span>
                  <span className="badge">{t.mode === 'i2v' ? '首帧图生视频' : '文生视频'}</span>
                  <span className="spacer" />
                  <TaskActions t={t} onChange={reload} />
                </div>
                <div className="pre" style={{ marginBottom: 6 }}>{t.prompt}</div>
                {t.error && <div className={'alert ' + (t.status === 'failed' ? 'err' : 'warn')}>{t.error}</div>}
                <TaskMeta t={t} />
              </div>
            </div>
          ))}
        </div>
      </div>
    </>
  )
}
