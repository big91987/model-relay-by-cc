import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { TASK_CLS, TASK_LABEL, api, fmtSize, fmtTime, yuan } from '../api'
import { Badge, useUI } from '../ui'

export type Task = {
  id: string; request_id: string; key_name: string; kind: string; provider: string; model: string; mode: string; prompt: string
  params: Record<string, any>; status: string; upstream_status: string; file_mime: string; file_size: number; has_file: boolean
  units: number; cost: number; error: string; created_at: number; finished_at: number
}

// useTasks：任务列表保存在服务端，刷新页面后仍可继续查看进度；有进行中的任务时自动轮询。
export function useTasks(kind: string) {
  const [tasks, setTasks] = useState<Task[]>([])
  const [error, setError] = useState('')
  const timer = useRef<number>()
  const load = () =>
    api('/tasks?kind=' + kind)
      .then((d) => { setTasks(d.items); setError('') })
      .catch((e) => setError(e.message))
  useEffect(() => {
    load()
    return () => window.clearTimeout(timer.current)
  }, [kind]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    window.clearTimeout(timer.current)
    if (tasks.some((t) => t.status === 'queued' || t.status === 'processing')) timer.current = window.setTimeout(load, 3000)
  }, [tasks]) // eslint-disable-line react-hooks/exhaustive-deps
  return { tasks, error, reload: load }
}

export function TaskMeta({ t }: { t: Task }) {
  return (
    <div className="small muted row" style={{ gap: 14 }}>
      <span>{fmtTime(t.created_at)}</span>
      <span>模型 {t.model}</span>
      <span>Key：{t.key_name}</span>
      {t.status === 'succeeded' && <span>费用 {yuan(t.cost, 4)}</span>}
      {t.file_size > 0 && <span>{fmtSize(t.file_size)}</span>}
      <Link to={`/logs?request_id=${t.request_id}`} className="mono">{t.request_id}</Link>
    </div>
  )
}

export function TaskStatus({ t }: { t: Task }) {
  const active = t.status === 'queued' || t.status === 'processing'
  return (
    <span>
      <Badge cls={TASK_CLS[t.status]}>{TASK_LABEL[t.status] || t.status}</Badge>
      {active && t.upstream_status && <span className="muted small"> 上游状态：{t.upstream_status}</span>}
    </span>
  )
}

export function TaskActions({ t, onChange }: { t: Task; onChange: () => void }) {
  const ui = useUI()
  const remove = async () => {
    if (!(await ui.confirm({ title: '删除任务', content: '确定删除该任务及其成品文件吗？费用记录不受影响。', danger: true, okText: '删除' }))) return
    await api('/tasks/' + t.id, { method: 'DELETE' }).then(onChange).catch((e) => ui.toast(e.message, 'err'))
  }
  return (
    <div className="row">
      {t.status === 'succeeded' && t.has_file && <a className="btn" href={`/api/tasks/${t.id}/content?download=1`}>下载</a>}
      {(t.status === 'succeeded' || t.status === 'failed') && <button className="sm danger" onClick={remove}>删除</button>}
    </div>
  )
}

// 音频类任务（长文本配音、音乐）的通用列表
export function AudioTaskList({ tasks, reload, empty, render }: { tasks: Task[]; reload: () => void; empty: string; render?: (t: Task) => React.ReactNode }) {
  if (!tasks.length) return <div className="empty">{empty}</div>
  return (
    <>
      {tasks.map((t) => (
        <div key={t.id} style={{ padding: '14px 0', borderBottom: '1px solid var(--border)' }}>
          <div className="row" style={{ marginBottom: 6 }}>
            <TaskStatus t={t} />
            <span className="spacer" />
            <TaskActions t={t} onChange={reload} />
          </div>
          {render ? render(t) : <div className="pre">{t.prompt}</div>}
          {(t.status === 'queued' || t.status === 'processing') && <div className="bar" style={{ margin: '8px 0' }}><i style={{ width: '60%', opacity: 0.6 }} /></div>}
          {t.status === 'succeeded' && t.has_file && <audio controls preload="none" src={`/api/tasks/${t.id}/content`} style={{ margin: '8px 0' }} />}
          {t.error && <div className={'alert ' + (t.status === 'failed' ? 'err' : 'warn')} style={{ margin: '8px 0' }}>{t.error}</div>}
          <TaskMeta t={t} />
        </div>
      ))}
    </>
  )
}
