import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { KeyAuth, api, download, fmtSize, fmtTime, gw, gwFetch } from '../api'
import { GwModel, KeyPicker, NeedKey, useKeyAuth, useModels } from '../keyctx'
import { Badge, Field, useUI } from '../ui'
import { AudioTaskList, useTasks } from './tasks'

type Voice = { voice_id: string; name: string }
type CustomVoice = { id: number; voice_id: string; name: string; kind: string; description: string; has_preview: boolean; consent_note: string; created_at: number }
type VoiceData = { system: Voice[]; custom: CustomVoice[]; fallback: boolean; message?: string }

function useVoices(auth: KeyAuth | null, model: string) {
  const [data, setData] = useState<VoiceData | null>(null)
  const [tick, setTick] = useState(0)
  useEffect(() => {
    if (!auth || !model) return setData(null)
    let alive = true
    gw('/audio/voices?model=' + encodeURIComponent(model), auth).then((d) => alive && setData(d)).catch(() => alive && setData(null))
    return () => { alive = false }
  }, [auth, model, tick])
  return { data, reload: () => setTick((t) => t + 1) }
}

function VoiceSelect({ data, value, onChange }: { data: VoiceData | null; value: string; onChange: (v: string) => void }) {
  const [q, setQ] = useState('')
  const sys = (data?.system || []).filter((v) => !q || v.name.includes(q) || v.voice_id.toLowerCase().includes(q.toLowerCase()))
  return (
    <>
      <div className="row">
        <input placeholder="搜索音色" value={q} onChange={(e) => setQ(e.target.value)} style={{ width: 130, flex: 'none' }} aria-label="搜索音色" />
        <select value={value} onChange={(e) => onChange(e.target.value)} style={{ flex: 1, minWidth: 0, width: 'auto' }} aria-label="音色">
          <option value="">请选择音色</option>
          {!!data?.custom.length && (
            <optgroup label="我的音色（复刻 / 设计）">
              {data.custom.map((v) => <option key={v.voice_id} value={v.voice_id}>{v.name}（{v.kind === 'clone' ? '复刻' : '设计'}）</option>)}
            </optgroup>
          )}
          <optgroup label={`系统音色（${sys.length}）`}>
            {sys.map((v) => <option key={v.voice_id} value={v.voice_id}>{v.name}｜{v.voice_id}</option>)}
          </optgroup>
        </select>
      </div>
      {data?.fallback && <div className="hint">{data.message}</div>}
    </>
  )
}

export default function Speech() {
  const { auth } = useKeyAuth()
  const { models, error, loading } = useModels('tts')
  const [tab, setTab] = useState<'tts' | 'long' | 'clone' | 'design'>('tts')
  const [model, setModel] = useState('')
  const [voice, setVoice] = useState('')
  useEffect(() => {
    if (!models.some((m) => m.id === model)) setModel(models[0]?.id || '')
  }, [models]) // eslint-disable-line react-hooks/exhaustive-deps
  const voices = useVoices(auth, model)
  // 复刻/设计完成后，选用新音色去朗读新文本
  const useVoice = (id: string) => {
    voices.reload()
    setVoice(id)
    setTab('tts')
  }
  const shared = { auth, models, model, setModel, voice, setVoice, voices: voices.data, reloadVoices: voices.reload }
  return (
    <>
      <div className="page-head">
        <div>
          <h1>语音与音色</h1>
          <p>MiniMax 文本转语音：系统音色朗读、流式朗读、长文本配音，以及音色复刻与音色设计。</p>
        </div>
      </div>
      <div className="card" style={{ marginBottom: 16 }}><KeyPicker /></div>
      <NeedKey models={models} error={error} loading={loading} kindLabel="语音" />
      <div className="tabs">
        {([['tts', '文本朗读'], ['long', '长文本配音'], ['clone', '音色复刻'], ['design', '音色设计']] as const).map(([k, label]) => (
          <button key={k} className={tab === k ? 'on' : ''} onClick={() => setTab(k)}>{label}</button>
        ))}
      </div>
      {tab === 'tts' && <TTS {...shared} />}
      {tab === 'long' && <LongTTS {...shared} />}
      {tab === 'clone' && <Clone {...shared} onUse={useVoice} />}
      {tab === 'design' && <Design {...shared} onUse={useVoice} />}
    </>
  )
}

type Shared = {
  auth: KeyAuth | null; models: GwModel[]; model: string; setModel: (m: string) => void
  voice: string; setVoice: (v: string) => void; voices: VoiceData | null; reloadVoices: () => void
}

function ModelVoice(p: Shared) {
  return (
    <div className="grid cols-2">
      <Field label="模型">
        <select value={p.model} onChange={(e) => p.setModel(e.target.value)} disabled={!p.models.length} aria-label="模型">{p.models.map((m) => <option key={m.id}>{m.id}</option>)}</select>
      </Field>
      <Field label="音色"><VoiceSelect data={p.voices} value={p.voice} onChange={p.setVoice} /></Field>
    </div>
  )
}

// —— 文本朗读：普通生成 + 流式边生成边播放 ——
function TTS(p: Shared) {
  const [text, setText] = useState('你好，欢迎使用模型网关的文本转语音功能。')
  const [speed, setSpeed] = useState(1)
  const [state, setState] = useState<'idle' | 'generating' | 'streaming'>('idle')
  const [result, setResult] = useState<{ url: string; blob: Blob; reqId: string; chars: string; partial?: boolean } | null>(null)
  const [note, setNote] = useState('')
  const audioRef = useRef<HTMLAudioElement>(null)
  const abortRef = useRef<AbortController | null>(null)
  const ui = useUI()
  const ready = !!p.auth && !!p.model && !!p.voice && !!text.trim() && state === 'idle'
  const body = (stream: boolean) => JSON.stringify({ model: p.model, input: text, voice: p.voice, speed, stream })
  const setBlob = (blob: Blob, reqId: string, chars: string, partial = false) => {
    setResult((old) => {
      if (old) URL.revokeObjectURL(old.url)
      return { url: URL.createObjectURL(blob), blob, reqId, chars, partial }
    })
  }

  const generate = async () => {
    if (!p.auth) return
    setState('generating')
    setNote('')
    try {
      const resp = await gwFetch('/audio/speech', p.auth, { method: 'POST', body: body(false) })
      setBlob(await resp.blob(), resp.headers.get('X-Request-Id') || '', resp.headers.get('X-Usage-Characters') || '')
      setTimeout(() => audioRef.current?.play().catch(() => {}), 50)
    } catch (e: any) {
      ui.toast(e.message, 'err')
    } finally {
      setState('idle')
    }
  }

  // 流式朗读：通过 MediaSource 把收到的 MP3 分片立即送入播放器
  const streamPlay = async () => {
    if (!p.auth) return
    const audio = audioRef.current!
    setState('streaming')
    setNote('')
    setResult(null)
    const ac = new AbortController()
    abortRef.current = ac
    const chunks: Uint8Array[] = []
    let reqId = ''
    const canMSE = typeof MediaSource !== 'undefined' && MediaSource.isTypeSupported('audio/mpeg')
    let ms: MediaSource | null = null
    let sb: SourceBuffer | null = null
    const queue: Uint8Array[] = []
    let ended = false
    const pump = () => {
      if (!sb || sb.updating || !ms || ms.readyState !== 'open') return
      const next = queue.shift()
      if (next) sb.appendBuffer(next as BufferSource)
      else if (ended) ms.endOfStream()
    }
    try {
      if (canMSE) {
        ms = new MediaSource()
        audio.src = URL.createObjectURL(ms)
        await new Promise<void>((res) => ms!.addEventListener('sourceopen', () => res(), { once: true }))
        sb = ms.addSourceBuffer('audio/mpeg')
        sb.addEventListener('updateend', pump)
        audio.play().catch(() => {})
      } else {
        setNote('当前浏览器不支持边生成边播放，将在生成完成后播放。')
      }
      const resp = await gwFetch('/audio/speech', p.auth, { method: 'POST', body: body(true), signal: ac.signal })
      reqId = resp.headers.get('X-Request-Id') || ''
      const reader = resp.body!.getReader()
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        chunks.push(value)
        if (canMSE) {
          queue.push(value)
          pump()
        }
      }
      ended = true
      pump()
      const blob = new Blob(chunks as BlobPart[], { type: 'audio/mpeg' })
      if (!canMSE) {
        setBlob(blob, reqId, '')
        setTimeout(() => audioRef.current?.play().catch(() => {}), 50)
      } else {
        // 播放结束后把完整音频换成可拖动、可下载的文件
        const swap = () => setBlob(blob, reqId, '')
        if (audio.ended || audio.paused) swap()
        else audio.addEventListener('ended', swap, { once: true })
      }
    } catch (e: any) {
      if (e?.name === 'AbortError') {
        audio.pause()
        setNote('已停止播放并取消生成。已合成的部分可能仍会计费，以请求日志为准。')
        if (chunks.length) setBlob(new Blob(chunks as BlobPart[], { type: 'audio/mpeg' }), reqId, '', true)
      } else {
        audio.pause()
        ui.toast(e.message, 'err')
      }
    } finally {
      abortRef.current = null
      setState('idle')
    }
  }
  const stop = () => {
    abortRef.current?.abort()
    audioRef.current?.pause()
  }
  useEffect(() => () => abortRef.current?.abort(), [])

  return (
    <div className="card">
      <ModelVoice {...p} />
      <Field label={`朗读文本（${text.length} 字）`} hint="同步朗读需少于 10000 字符；更长的文本请使用“长文本配音”"><textarea rows={6} value={text} onChange={(e) => setText(e.target.value)} /></Field>
      <Field label={`语速：${speed.toFixed(1)}`}><input type="range" min={0.5} max={2} step={0.1} value={speed} onChange={(e) => setSpeed(Number(e.target.value))} style={{ maxWidth: 320 }} aria-label="语速" /></Field>
      <div className="row">
        <button className="primary" disabled={!ready} onClick={generate}>{state === 'generating' ? '生成中…' : '生成语音'}</button>
        {state === 'streaming' ? <button className="danger" onClick={stop}>停止播放</button> : <button disabled={!ready} onClick={streamPlay}>流式朗读（边生成边播放）</button>}
        {state === 'streaming' && <Badge cls="info">正在边生成边播放…</Badge>}
      </div>
      {note && <div className="alert warn" style={{ marginTop: 12 }}>{note}</div>}
      <div style={{ marginTop: 14 }}>
        <audio ref={audioRef} controls src={state === 'streaming' ? undefined : result?.url} style={{ display: state === 'streaming' || result ? 'block' : 'none' }} />
        {result && state === 'idle' && (
          <div className="row small" style={{ marginTop: 8 }}>
            <button className="sm" onClick={() => download(result.blob, `语音_${Date.now()}.mp3`)}>下载{result.partial ? '已生成部分' : '语音'}</button>
            <span className="muted">{fmtSize(result.blob.size)}</span>
            {result.chars && <span className="muted">计费字符数 {result.chars}</span>}
            {result.reqId && <Link to={`/logs?request_id=${result.reqId}`} className="mono">{result.reqId}</Link>}
          </div>
        )}
      </div>
    </div>
  )
}

// —— 长文本配音：后台任务 ——
function LongTTS(p: Shared) {
  const { tasks, error, reload } = useTasks('tts_async')
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const ui = useUI()
  const submit = async () => {
    if (!p.auth) return
    setBusy(true)
    try {
      await gw('/audio/speech/async', p.auth, { model: p.model, input: text, voice: p.voice })
      ui.toast('配音任务已提交到后台', 'ok')
      setText('')
      reload()
    } catch (e: any) {
      ui.toast(e.message, 'err')
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="grid cols-2">
      <div className="card">
        <h2>提交配音任务</h2>
        <ModelVoice {...p} />
        <Field label={`长文本（${text.length} 字）`} hint="单次最多 10 万计费字符（中文每字计 2 字符）。提交后可离开或刷新页面，任务在后台继续。">
          <textarea rows={12} value={text} onChange={(e) => setText(e.target.value)} placeholder="粘贴需要配音的长文本…" />
        </Field>
        <button className="primary" disabled={!p.auth || !p.model || !p.voice || !text.trim() || busy} onClick={submit}>{busy ? '提交中…' : '提交后台配音'}</button>
      </div>
      <div className="card">
        <div className="row"><h2 style={{ margin: 0 }}>配音任务</h2><span className="spacer" /><button className="sm" onClick={reload}>刷新</button></div>
        {error && <div className="alert err">{error}</div>}
        <AudioTaskList tasks={tasks} reload={reload} empty="还没有配音任务。" render={(t) => (
          <>
            <div className="pre">{t.prompt}</div>
            <div className="small muted">音色 {t.params?.voice} · 共 {t.params?.chars} 字{t.units > 0 ? ` · 计费字符 ${t.units}` : ''}</div>
          </>
        )} />
      </div>
    </div>
  )
}

function MyVoices({ kind, voices, onUse, reload }: { kind: string; voices: VoiceData | null; onUse: (id: string) => void; reload: () => void }) {
  const ui = useUI()
  const list = (voices?.custom || []).filter((v) => v.kind === kind)
  const remove = async (v: CustomVoice) => {
    if (!(await ui.confirm({ title: '移除音色', content: `从网关移除音色“${v.name}”？上游账号中的音色按其自身规则保留或过期。`, danger: true, okText: '移除' }))) return
    await api('/voices/' + v.id, { method: 'DELETE' }).then(reload).catch((e) => ui.toast(e.message, 'err'))
  }
  if (!list.length) return <div className="empty">还没有{kind === 'clone' ? '复刻' : '设计'}的音色。</div>
  return (
    <>
      {list.map((v) => (
        <div key={v.id} style={{ padding: '12px 0', borderBottom: '1px solid var(--border)' }}>
          <div className="row">
            <b>{v.name}</b>
            <span className="mono small muted">{v.voice_id}</span>
            <span className="spacer" />
            <button className="sm primary" onClick={() => onUse(v.voice_id)}>用该音色朗读</button>
            <button className="sm danger" onClick={() => remove(v)}>移除</button>
          </div>
          {v.description && <div className="small muted">描述：{v.description}</div>}
          {v.has_preview && <audio controls preload="none" src={`/api/voices/${v.id}/preview`} style={{ marginTop: 6 }} />}
          <div className="small muted">{fmtTime(v.created_at)}{v.consent_note ? ` · ${v.consent_note}` : ''}</div>
        </div>
      ))}
    </>
  )
}

// —— 音色复刻 ——
function Clone(p: Shared & { onUse: (id: string) => void }) {
  const [file, setFile] = useState<File | null>(null)
  const [name, setName] = useState('')
  const [preview, setPreview] = useState('你好，这是用复刻音色朗读的一段试听文本。')
  const [consent, setConsent] = useState(false)
  const [busy, setBusy] = useState(false)
  const ui = useUI()
  const fileOk = useMemo(() => !!file && /\.(mp3|m4a|wav)$/i.test(file.name) && file.size <= 20 * 1024 * 1024, [file])
  const submit = async () => {
    if (!p.auth || !file) return
    setBusy(true)
    try {
      const fd = new FormData()
      fd.append('model', p.model)
      fd.append('name', name)
      fd.append('preview_text', preview)
      fd.append('consent', 'true')
      fd.append('file', file)
      const resp = await gwFetch('/audio/voices/clone', p.auth, { method: 'POST', body: fd })
      const d = await resp.json()
      ui.toast(`音色“${d.name}”已创建，可在右侧试听`, 'ok')
      setFile(null)
      setConsent(false)
      p.reloadVoices()
    } catch (e: any) {
      ui.toast(e.message, 'err')
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="grid cols-2">
      <div className="card">
        <h2>创建复刻音色</h2>
        <Field label="用于试听的语音模型">
          <select value={p.model} onChange={(e) => p.setModel(e.target.value)} disabled={!p.models.length}>{p.models.map((m) => <option key={m.id}>{m.id}</option>)}</select>
        </Field>
        <Field label="样音文件" hint="mp3 / m4a / wav，时长 10 秒 ~ 5 分钟，不超过 20MB">
          <input type="file" accept=".mp3,.m4a,.wav,audio/*" onChange={(e) => setFile(e.target.files?.[0] || null)} />
          {file && !fileOk && <div className="alert err" style={{ marginTop: 6 }}>文件格式或大小不符合要求。</div>}
        </Field>
        <Field label="音色名称"><input value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：张老师的声音" /></Field>
        <Field label="试听文本"><textarea rows={3} value={preview} onChange={(e) => setPreview(e.target.value)} /></Field>
        <div className="alert warn">
          <label className="check"><input type="checkbox" checked={consent} onChange={(e) => setConsent(e.target.checked)} />我确认该样音已获得说话人本人授权，可用于音色复刻</label>
          <div className="small" style={{ marginTop: 4 }}>未经授权复刻他人声音可能违法。网关会记录本次授权确认。</div>
        </div>
        <button className="primary" disabled={!p.auth || !p.model || !fileOk || !consent || busy} onClick={submit}>{busy ? '复刻中（可能需要几十秒）…' : '开始复刻'}</button>
      </div>
      <div className="card">
        <h2>我的复刻音色</h2>
        <MyVoices kind="clone" voices={p.voices} onUse={p.onUse} reload={p.reloadVoices} />
      </div>
    </div>
  )
}

// —— 音色设计 ——
function Design(p: Shared & { onUse: (id: string) => void }) {
  const [prompt, setPrompt] = useState('')
  const [preview, setPreview] = useState('大家好，欢迎收听今天的节目，我们马上开始。')
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const ui = useUI()
  const submit = async () => {
    if (!p.auth) return
    setBusy(true)
    try {
      const d = await gw('/audio/voices/design', p.auth, { model: p.model, prompt, preview_text: preview, name })
      ui.toast(`音色“${d.name}”已生成，可在右侧试听`, 'ok')
      p.reloadVoices()
    } catch (e: any) {
      ui.toast(e.message, 'err')
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="grid cols-2">
      <div className="card">
        <h2>根据描述设计音色</h2>
        <Field label="语音模型">
          <select value={p.model} onChange={(e) => p.setModel(e.target.value)} disabled={!p.models.length}>{p.models.map((m) => <option key={m.id}>{m.id}</option>)}</select>
        </Field>
        <Field label="音色描述"><textarea rows={4} value={prompt} onChange={(e) => setPrompt(e.target.value)} placeholder="例如：沉稳温和的中年男声，语速偏慢，带一点磁性，适合纪录片旁白" /></Field>
        <Field label="试听文本" hint="最多 500 字符"><textarea rows={3} value={preview} onChange={(e) => setPreview(e.target.value)} /></Field>
        <Field label="音色名称"><input value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：纪录片旁白" /></Field>
        <button className="primary" disabled={!p.auth || !p.model || !prompt.trim() || !preview.trim() || busy} onClick={submit}>{busy ? '设计中…' : '生成音色并试听'}</button>
        <p className="hint">满意后点击右侧“用该音色朗读”，即可用它朗读另一段文案。</p>
      </div>
      <div className="card">
        <h2>我设计的音色</h2>
        <MyVoices kind="design" voices={p.voices} onUse={p.onUse} reload={p.reloadVoices} />
      </div>
    </div>
  )
}
