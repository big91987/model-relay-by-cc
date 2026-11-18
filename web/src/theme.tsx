import { useEffect, useState } from 'react'

export type ThemeMode = 'light' | 'dark' | 'system'
const KEY = 'relay-theme'

function read(): ThemeMode {
  try {
    const v = localStorage.getItem(KEY)
    if (v === 'light' || v === 'dark' || v === 'system') return v
  } catch {}
  return 'system'
}

function apply(mode: ThemeMode) {
  const dark = mode === 'dark' || (mode === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.dataset.theme = dark ? 'dark' : 'light'
}

// 主题：浅色 / 深色 / 跟随系统。选择保存在 localStorage（只是界面偏好，不含任何密钥）。
export function useTheme(): [ThemeMode, (m: ThemeMode) => void] {
  const [mode, setMode] = useState<ThemeMode>(read)
  useEffect(() => {
    apply(mode)
    try {
      localStorage.setItem(KEY, mode)
    } catch {}
    if (mode !== 'system') return
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const h = () => apply('system')
    mq.addEventListener('change', h)
    return () => mq.removeEventListener('change', h)
  }, [mode])
  return [mode, setMode]
}

export function ThemeSwitch() {
  const [mode, setMode] = useTheme()
  const opts: [ThemeMode, string][] = [['light', '浅色'], ['dark', '深色'], ['system', '跟随系统']]
  return (
    <div className="seg" role="group" aria-label="主题">
      {opts.map(([m, label]) => (
        <button key={m} className={mode === m ? 'on' : ''} onClick={() => setMode(m)} aria-pressed={mode === m}>
          {label}
        </button>
      ))}
    </div>
  )
}
