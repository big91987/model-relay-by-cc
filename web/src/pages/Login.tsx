import { FormEvent, useState } from 'react'
import { api } from '../api'
import { ThemeSwitch } from '../theme'

export default function Login({ onLogin }: { onLogin: (u: { username: string }) => void }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const d = await api('/login', { method: 'POST', body: { username, password } })
      onLogin(d.user)
    } catch (err: any) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="login-wrap">
      <form className="card login" onSubmit={submit}>
        <div className="row" style={{ justifyContent: 'space-between', marginBottom: 16 }}>
          <h1>模型网关控制台</h1>
        </div>
        {error && <div className="alert err">{error}</div>}
        <div className="field">
          <label htmlFor="u">用户名</label>
          <input id="u" value={username} onChange={(e) => setUsername(e.target.value)} autoFocus autoComplete="username" />
        </div>
        <div className="field">
          <label htmlFor="p">密码</label>
          <input id="p" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />
        </div>
        <button className="primary" style={{ width: '100%', justifyContent: 'center' }} disabled={busy || !username || !password}>
          {busy ? '登录中…' : '登录'}
        </button>
        <div className="row" style={{ justifyContent: 'center', marginTop: 16 }}>
          <ThemeSwitch />
        </div>
      </form>
    </div>
  )
}
