import { Link } from 'react-router-dom'
import { api, yuan } from '../api'
import { useLoad } from '../ui'

export default function Dashboard() {
  const dayStart = new Date().setHours(0, 0, 0, 0)
  const { data, error } = useLoad<any>(() => api('/dashboard?day_start=' + dayStart), [])
  if (error) return <div className="alert err">{error}</div>
  if (!data) return <div className="empty">加载中…</div>
  const maxReq = Math.max(1, ...data.series.map((s: any) => s.requests))
  return (
    <>
      <div className="page-head">
        <div>
          <h1>概览</h1>
          <p>网关的整体运行情况</p>
        </div>
      </div>
      {data.pending_review > 0 && (
        <div className="alert warn">
          有 {data.pending_review} 笔费用待核实的请求正在占用额度，<Link to="/billing">前往账务核实</Link>。
        </div>
      )}
      {data.channels === 0 && (
        <div className="alert info">
          还没有可用渠道。请先到 <Link to="/channels">渠道</Link> 添加上游，再到 <Link to="/keys">网关 Key</Link> 创建调用凭证。
        </div>
      )}
      <div className="grid cols-4">
        <div className="card stat"><div className="label">今日请求</div><div className="value">{data.today_requests}</div><div className="sub">今日 Token：{data.today_tokens.toLocaleString()}</div></div>
        <div className="card stat"><div className="label">今日费用</div><div className="value">{yuan(data.today_cost, 4)}</div><div className="sub">累计：{yuan(data.total_cost, 4)}</div></div>
        <div className="card stat"><div className="label">可用渠道 / 模型</div><div className="value">{data.channels} / {data.models}</div><div className="sub">启用的网关 Key：{data.keys}</div></div>
        <div className="card stat"><div className="label">进行中的媒体任务</div><div className="value">{data.active_tasks}</div><div className="sub">待核实预留：{data.pending_review}</div></div>
      </div>
      <div className="card" style={{ marginTop: 16 }}>
        <h2>近 7 天请求数</h2>
        <div className="spark">
          {data.series.map((s: any) => (
            <div key={s.day} title={`${s.requests} 次，${yuan(s.cost, 4)}`}>
              <span>{s.requests}</span>
              <i style={{ height: `${(s.requests / maxReq) * 60}px` }} />
              <span>{new Date(s.day).getMonth() + 1}/{new Date(s.day).getDate()}</span>
            </div>
          ))}
        </div>
      </div>
    </>
  )
}
