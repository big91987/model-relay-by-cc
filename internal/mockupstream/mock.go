// Package mockupstream 是受控的假上游，用于回归测试和确定性的账务边界验证。
// 它不能代替真实上游验收。行为通过请求体里的 "mock" 字段控制（网关会原样透传未知字段）：
//
//	{"mock": {"prompt_tokens": 100, "completion_tokens": 50, "delay_ms": 200, "status": 500,
//	          "no_usage": true, "hang": true, "drop_after": 2, "chunks": 5, "chunk_delay_ms": 50}}
package mockupstream

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Mock struct {
	Calls     atomic.Int64 // 收到的计费类调用次数（聊天、向量）
	mux       *http.ServeMux
	mu        sync.Mutex
	tasks     map[string]int // 任务 ID -> 已被查询次数
	FailModel atomic.Bool    // /models 返回 500
	baseURL   func(r *http.Request) string
}

type ctl struct {
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	DelayMs          int    `json:"delay_ms"`
	Status           int    `json:"status"`
	NoUsage          bool   `json:"no_usage"`
	Hang             bool   `json:"hang"`
	DropAfter        int    `json:"drop_after"`
	Chunks           int    `json:"chunks"`
	ChunkDelayMs     int    `json:"chunk_delay_ms"`
	Content          string `json:"content"`
}

func New() *Mock {
	m := &Mock{mux: http.NewServeMux(), tasks: map[string]int{}}
	m.baseURL = func(r *http.Request) string { return "http://" + r.Host }
	for _, p := range []string{"/chat/completions", "/v1/chat/completions"} {
		m.mux.HandleFunc("POST "+p, m.chat)
	}
	for _, p := range []string{"/embeddings", "/v1/embeddings"} {
		m.mux.HandleFunc("POST "+p, m.embeddings)
	}
	for _, p := range []string{"/models", "/v1/models"} {
		m.mux.HandleFunc("GET "+p, m.models)
	}
	m.mux.HandleFunc("POST /v1/t2a_v2", m.t2a)
	m.mux.HandleFunc("POST /v1/get_voice", m.voices)
	m.mux.HandleFunc("POST /v1/t2a_async_v2", m.t2aAsync)
	m.mux.HandleFunc("GET /v1/query/t2a_async_query_v2", m.t2aAsyncQuery)
	m.mux.HandleFunc("GET /v1/files/retrieve", m.fileRetrieve)
	m.mux.HandleFunc("GET /v1/files/retrieve_content", m.fileContent)
	m.mux.HandleFunc("POST /v1/files/upload", m.fileUpload)
	m.mux.HandleFunc("POST /v1/voice_clone", m.voiceClone)
	m.mux.HandleFunc("POST /v1/voice_design", m.voiceDesign)
	m.mux.HandleFunc("POST /v1/music_generation", m.music)
	m.mux.HandleFunc("POST /v2/video_generation", m.videoCreate)
	m.mux.HandleFunc("GET /v2/query/video_generation/{id}", m.videoQuery)
	m.mux.HandleFunc("POST /contents/generations/tasks", m.videoCreate)
	m.mux.HandleFunc("GET /contents/generations/tasks/{id}", m.videoQuery)
	m.mux.HandleFunc("GET /files/{name}", m.staticFile)
	return m
}

func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readReq(r *http.Request) (map[string]any, ctl) {
	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	json.Unmarshal(b, &body)
	var c ctl
	if raw, ok := body["mock"]; ok {
		rb, _ := json.Marshal(raw)
		json.Unmarshal(rb, &c)
	}
	return body, c
}

// pre 处理延迟、挂起和错误状态；返回 false 表示已结束响应。
func pre(w http.ResponseWriter, r *http.Request, c ctl) bool {
	if c.DelayMs > 0 {
		select {
		case <-time.After(time.Duration(c.DelayMs) * time.Millisecond):
		case <-r.Context().Done():
			return false
		}
	}
	if c.Hang {
		<-r.Context().Done()
		return false
	}
	if c.Status >= 400 {
		writeJSON(w, c.Status, map[string]any{"error": map[string]any{"message": fmt.Sprintf("mock upstream error %d", c.Status), "type": "mock_error"}})
		return false
	}
	return true
}

func (m *Mock) chat(w http.ResponseWriter, r *http.Request) {
	body, c := readReq(r)
	m.Calls.Add(1)
	if !pre(w, r, c) {
		return
	}
	if c.PromptTokens == 0 {
		c.PromptTokens = 12
	}
	if c.CompletionTokens == 0 {
		c.CompletionTokens = 8
	}
	if c.Content == "" {
		msgs, _ := body["messages"].([]any)
		c.Content = fmt.Sprintf("这是假上游的回复（共收到 %d 条消息）。", len(msgs))
	}
	model, _ := body["model"].(string)
	usage := map[string]any{"prompt_tokens": c.PromptTokens, "completion_tokens": c.CompletionTokens, "total_tokens": c.PromptTokens + c.CompletionTokens}
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		n := c.Chunks
		if n <= 0 {
			n = 4
		}
		runes := []rune(c.Content)
		per := (len(runes) + n - 1) / n
		for i := 0; i < n; i++ {
			if c.DropAfter > 0 && i >= c.DropAfter {
				// 模拟上游断流：不发送结束标记直接断开
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, _ := hj.Hijack()
					conn.Close()
				}
				return
			}
			lo, hi := min(i*per, len(runes)), min((i+1)*per, len(runes))
			chunk := map[string]any{"id": "mock", "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": string(runes[lo:hi])}, "finish_reason": nil}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
			if c.ChunkDelayMs > 0 {
				select {
				case <-time.After(time.Duration(c.ChunkDelayMs) * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
		}
		fin, _ := json.Marshal(map[string]any{"id": "mock", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		fmt.Fprintf(w, "data: %s\n\n", fin)
		so, _ := body["stream_options"].(map[string]any)
		if inc, _ := so["include_usage"].(bool); inc && !c.NoUsage {
			ub, _ := json.Marshal(map[string]any{"id": "mock", "object": "chat.completion.chunk", "model": model, "choices": []any{}, "usage": usage})
			fmt.Fprintf(w, "data: %s\n\n", ub)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
		return
	}
	resp := map[string]any{"id": "mock", "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": c.Content}, "finish_reason": "stop"}}}
	if !c.NoUsage {
		resp["usage"] = usage
	}
	writeJSON(w, 200, resp)
}

func (m *Mock) embeddings(w http.ResponseWriter, r *http.Request) {
	body, c := readReq(r)
	m.Calls.Add(1)
	if !pre(w, r, c) {
		return
	}
	n := 1
	if arr, ok := body["input"].([]any); ok {
		n = len(arr)
	}
	if c.PromptTokens == 0 {
		c.PromptTokens = int64(5 * n)
	}
	data := []any{}
	for i := 0; i < n; i++ {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": []float64{0.1, 0.2, 0.3, float64(i)}})
	}
	resp := map[string]any{"object": "list", "model": body["model"], "data": data}
	if !c.NoUsage {
		resp["usage"] = map[string]any{"prompt_tokens": c.PromptTokens, "total_tokens": c.PromptTokens}
	}
	writeJSON(w, 200, resp)
}

func (m *Mock) models(w http.ResponseWriter, r *http.Request) {
	if m.FailModel.Load() {
		writeJSON(w, 500, map[string]any{"error": map[string]any{"message": "mock models failure"}})
		return
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "mock-chat"}, map[string]any{"id": "mock-chat-pro"}, map[string]any{"id": "mock-embedding"}}})
}

var ok = map[string]any{"status_code": 0, "status_msg": "success"}

// fakeMP3 是一小段可识别的假音频数据。
func fakeMP3(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte{0xFF, 0xFB, 0x90, 0x00})
	return b
}

func (m *Mock) t2a(w http.ResponseWriter, r *http.Request) {
	body, c := readReq(r)
	if !pre(w, r, c) {
		return
	}
	text, _ := body["text"].(string)
	if strings.Contains(text, "[mock-error]") {
		writeJSON(w, 200, map[string]any{"base_resp": map[string]any{"status_code": 2013, "status_msg": "invalid params"}})
		return
	}
	chars := int64(0)
	for _, ru := range text {
		if ru > 0x2E7F {
			chars += 2
		} else {
			chars++
		}
	}
	audio := fakeMP3(4000)
	extra := map[string]any{"usage_characters": chars, "audio_size": len(audio), "audio_length": 1000}
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			b, _ := json.Marshal(map[string]any{"data": map[string]any{"audio": hex.EncodeToString(audio[i*1000 : (i+1)*1000]), "status": 1}, "base_resp": ok})
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
			delay := c.ChunkDelayMs
			if delay == 0 {
				delay = 20
			}
			select {
			case <-time.After(time.Duration(delay) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		// 结束块重复整段音频（上游默认行为），网关不应重复下发
		b, _ := json.Marshal(map[string]any{"data": map[string]any{"audio": hex.EncodeToString(audio), "status": 2}, "extra_info": extra, "base_resp": ok})
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		return
	}
	writeJSON(w, 200, map[string]any{"data": map[string]any{"audio": hex.EncodeToString(audio), "status": 2}, "extra_info": extra, "base_resp": ok})
}

func (m *Mock) voices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"system_voice": []any{
		map[string]any{"voice_id": "mock-voice-1", "voice_name": "假上游音色一"}, map[string]any{"voice_id": "mock-voice-2", "voice_name": "假上游音色二"}}, "base_resp": ok})
}

func (m *Mock) t2aAsync(w http.ResponseWriter, r *http.Request) {
	body, _ := readReq(r)
	text, _ := body["text"].(string)
	writeJSON(w, 200, map[string]any{"task_id": 9001, "file_id": 7001, "usage_characters": len([]rune(text)) * 2, "base_resp": ok})
}

func (m *Mock) progress(id string, need int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[id]++
	return m.tasks[id] >= need
}

func (m *Mock) t2aAsyncQuery(w http.ResponseWriter, r *http.Request) {
	status := "Processing"
	if m.progress("tts:"+r.URL.Query().Get("task_id"), 2) {
		status = "Success"
	}
	writeJSON(w, 200, map[string]any{"task_id": 9001, "status": status, "file_id": 7001, "base_resp": ok})
}

func (m *Mock) fileRetrieve(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"file": map[string]any{"file_id": 7001, "download_url": m.baseURL(r) + "/files/audio.mp3"}, "base_resp": ok})
}

func (m *Mock) fileContent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(fakeMP3(6000))
}

func (m *Mock) fileUpload(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	writeJSON(w, 200, map[string]any{"file": map[string]any{"file_id": 8001}, "base_resp": ok})
}

func (m *Mock) voiceClone(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"demo_audio": m.baseURL(r) + "/files/audio.mp3", "input_sensitive": false, "base_resp": ok})
}

func (m *Mock) voiceDesign(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"voice_id": "mockdesign" + fmt.Sprint(time.Now().UnixNano()%100000), "trial_audio": hex.EncodeToString(fakeMP3(3000)), "base_resp": ok})
}

func (m *Mock) music(w http.ResponseWriter, r *http.Request) {
	_, c := readReq(r)
	if !pre(w, r, c) {
		return
	}
	time.Sleep(300 * time.Millisecond)
	writeJSON(w, 200, map[string]any{"data": map[string]any{"audio": hex.EncodeToString(fakeMP3(8000)), "status": 2}, "extra_info": map[string]any{"music_duration": 30000}, "base_resp": ok})
}

func (m *Mock) videoCreate(w http.ResponseWriter, r *http.Request) {
	body, c := readReq(r)
	if !pre(w, r, c) {
		return
	}
	id := fmt.Sprintf("mockvid%d", time.Now().UnixNano())
	if content, _ := body["content"].([]any); len(content) > 0 {
		if first, _ := content[0].(map[string]any); first != nil {
			if t, _ := first["text"].(string); strings.Contains(t, "[mock-fail]") {
				id = "fail" + id
			}
		}
	}
	writeJSON(w, 200, map[string]any{"id": id, "task_id": id})
}

func (m *Mock) videoQuery(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task := map[string]any{"id": id, "status": "running"}
	if m.progress("video:"+id, 2) {
		if strings.HasPrefix(id, "fail") {
			task["status"] = "failed"
			task["error"] = map[string]any{"code": "mock", "message": "mock video failure"}
		} else {
			task["status"] = "succeeded"
			u := m.baseURL(r) + "/files/video.mp4"
			task["content"] = map[string]any{"video_url": u, "url": u}
			task["usage"] = map[string]any{"completion_tokens": 50000, "total_tokens": 50000}
		}
	}
	if strings.HasPrefix(r.URL.Path, "/v2/") {
		writeJSON(w, 200, map[string]any{"task": task})
		return
	}
	writeJSON(w, 200, task)
}

func (m *Mock) staticFile(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.PathValue("name"), ".mp4") {
		w.Header().Set("Content-Type", "video/mp4")
		b := make([]byte, 5000)
		copy(b, []byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'})
		w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Write(fakeMP3(5000))
}
