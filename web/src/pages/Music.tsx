import { useEffect, useState } from 'react'
import { gw } from '../api'
import { KeyPicker, NeedKey, useKeyAuth, useModels } from '../keyctx'
import { Field, useUI } from '../ui'
import { AudioTaskList, useTasks } from './tasks'

export default function Music() {
  const { auth } = useKeyAuth()
  const { models, error, loading } = useModels('music')
  const { tasks, error: taskErr, reload } = useTasks('music')
  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('')
  const [lyrics, setLyrics] = useState('')
  const [busy, setBusy] = useState(false)
  const ui = useUI()
  useEffect(() => {
    if (!models.some((m) => m.id === model)) setModel(models[0]?.id || '')
  }, [models]) // eslint-disable-line react-hooks/exhaustive-deps
  const submit = async () => {
    if (!auth) return
    setBusy(true)
    try {
      await gw('/music/generations', auth, { model, prompt, lyrics })
      ui.toast('已提交，音乐生成通常需要 1~3 分钟', 'ok')
      reload()
    } catch (e: any) {
      ui.toast(e.message, 'err')
    } finally {
      setBusy(false)
    }
  }
  return (
    <>
      <div className="page-head">
        <div>
          <h1>音乐生成</h1>
          <p>按风格描述和歌词生成音乐，完成后可试听和下载。任务在后台执行，刷新页面后仍可查看。</p>
        </div>
      </div>
      <div className="card" style={{ marginBottom: 16 }}><KeyPicker /></div>
      <NeedKey models={models} error={error} loading={loading} kindLabel="音乐" />
      <div className="grid cols-2">
        <div className="card">
          <h2>新建音乐</h2>
          <Field label="模型">
            <select value={model} onChange={(e) => setModel(e.target.value)} disabled={!models.length} aria-label="模型">{models.map((m) => <option key={m.id}>{m.id}</option>)}</select>
          </Field>
          <Field label="风格描述" hint="最多 2000 字符"><textarea rows={3} value={prompt} onChange={(e) => setPrompt(e.target.value)} placeholder="例如：轻快的独立流行，木吉他与口琴，温暖治愈，适合清晨" /></Field>
          <Field label="歌词" hint="最多 3500 字符；可用 [Verse]、[Chorus] 等标签标注结构，换行分句"><textarea rows={10} value={lyrics} onChange={(e) => setLyrics(e.target.value)} placeholder={'[Verse]\n阳光洒在窗台\n新的一天醒来\n[Chorus]\n向前走 不回头'} /></Field>
          <button className="primary" disabled={!auth || !model || !prompt.trim() || !lyrics.trim() || busy} onClick={submit}>{busy ? '提交中…' : '生成音乐'}</button>
        </div>
        <div className="card">
          <div className="row"><h2 style={{ margin: 0 }}>我的音乐</h2><span className="spacer" /><button className="sm" onClick={reload}>刷新</button></div>
          {taskErr && <div className="alert err">{taskErr}</div>}
          <AudioTaskList tasks={tasks} reload={reload} empty="还没有音乐任务。" render={(t) => (
            <>
              <div><b>风格：</b>{t.prompt}</div>
              <details className="small" style={{ marginTop: 4 }}><summary className="muted">查看歌词</summary><div className="pre">{t.params?.lyrics}</div></details>
            </>
          )} />
        </div>
      </div>
    </>
  )
}
