import { useEffect, useState } from 'react'
import { NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { api } from './api'
import { ThemeSwitch } from './theme'
import { UIProvider } from './ui'
import { KeyProvider } from './keyctx'
import Dashboard from './pages/Dashboard'
import Channels from './pages/Channels'
import Models from './pages/Models'
import Keys from './pages/Keys'
import Playground from './pages/Playground'
import Video from './pages/Video'
import Speech from './pages/Speech'
import Music from './pages/Music'
import Logs from './pages/Logs'
import Usage from './pages/Usage'
import Billing from './pages/Billing'
import Login from './pages/Login'

const NAV: [string, [string, string][]][] = [
  ['总览', [['/', '概览']]],
  ['配置', [['/channels', '渠道'], ['/models', '模型'], ['/keys', '网关 Key']]],
  ['试用', [['/playground', '聊天 Playground'], ['/video', '视频生成'], ['/speech', '语音与音色'], ['/music', '音乐生成']]],
  ['账务', [['/logs', '调用日志'], ['/usage', '用量与费用'], ['/billing', '账务核实']]],
]

export default function App() {
  const [user, setUser] = useState<{ username: string } | null | undefined>(undefined)
  const loc = useLocation()
  useEffect(() => {
    api('/me').then((d) => setUser(d.user)).catch(() => setUser(null))
    const h = () => setUser(null)
    window.addEventListener('relay-unauthorized', h)
    return () => window.removeEventListener('relay-unauthorized', h)
  }, [])
  if (user === undefined) return null
  return (
    <UIProvider>
      {user === null ? (
        <Login onLogin={setUser} />
      ) : (
        <KeyProvider>
          <div className="layout">
            <aside className="sidebar">
              <div className="brand">模型网关<small>Model Relay 控制台</small></div>
              <nav className="nav">
                {NAV.map(([group, items]) => (
                  <div key={group}>
                    <div className="nav-group">{group}</div>
                    {items.map(([to, label]) => (
                      <NavLink key={to} to={to} end={to === '/'}>{label}</NavLink>
                    ))}
                  </div>
                ))}
              </nav>
            </aside>
            <div className="main">
              <header className="topbar">
                <span className="muted small">{NAV.flatMap(([, items]) => items).find(([to]) => to === loc.pathname)?.[1] || ''}</span>
                <div className="row">
                  <ThemeSwitch />
                  <span className="muted">{user.username}</span>
                  <button className="sm" onClick={() => api('/logout', { method: 'POST' }).finally(() => setUser(null))}>退出登录</button>
                </div>
              </header>
              <main className="content">
                <Routes>
                  <Route path="/" element={<Dashboard />} />
                  <Route path="/channels" element={<Channels />} />
                  <Route path="/models" element={<Models />} />
                  <Route path="/keys" element={<Keys />} />
                  <Route path="/playground" element={<Playground />} />
                  <Route path="/video" element={<Video />} />
                  <Route path="/speech" element={<Speech />} />
                  <Route path="/music" element={<Music />} />
                  <Route path="/logs" element={<Logs />} />
                  <Route path="/usage" element={<Usage />} />
                  <Route path="/billing" element={<Billing />} />
                  <Route path="*" element={<Navigate to="/" replace />} />
                </Routes>
              </main>
            </div>
          </div>
        </KeyProvider>
      )}
    </UIProvider>
  )
}
