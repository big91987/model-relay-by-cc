package gateway

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ———————————————————— 任务模型 ————————————————————

type MediaTask struct {
	ID             string         `json:"id"`
	RequestID      string         `json:"request_id"`
	KeyID          int64          `json:"key_id"`
	KeyName        string         `json:"key_name"`
	Kind           string         `json:"kind"` // video | tts_async | music
	Provider       string         `json:"provider"`
	ChannelID      int64          `json:"channel_id"`
	Model          string         `json:"model"`
	UpstreamModel  string         `json:"upstream_model"`
	Mode           string         `json:"mode"`
	Prompt         string         `json:"prompt"`
	Params         map[string]any `json:"params"`
	Status         string         `json:"status"` // queued | processing | succeeded | failed
	UpstreamStatus string         `json:"upstream_status"`
	UpstreamTaskID string         `json:"-"`
	UpstreamFileID string         `json:"-"`
	ResultURL      string         `json:"-"`
	FilePath       string         `json:"-"`
	FileMime       string         `json:"file_mime"`
	FileSize       int64          `json:"file_size"`
	HasFile        bool           `json:"has_file"`
	Units          int64          `json:"units"`
	Cost           int64          `json:"cost"`
	Error          string         `json:"error"`
	CreatedAt      int64          `json:"created_at"`
	UpdatedAt      int64          `json:"updated_at"`
	FinishedAt     int64          `json:"finished_at"`
}

const taskColumns = `id, request_id, key_id, key_name, kind, provider, channel_id, model, upstream_model, mode, prompt, params, status, upstream_status,
	upstream_task_id, upstream_file_id, result_url, file_path, file_mime, file_size, units, cost, error, created_at, updated_at, finished_at`

func scanTask(row rowScanner) (*MediaTask, error) {
	var t MediaTask
	var params string
	if err := row.Scan(&t.ID, &t.RequestID, &t.KeyID, &t.KeyName, &t.Kind, &t.Provider, &t.ChannelID, &t.Model, &t.UpstreamModel, &t.Mode, &t.Prompt, &params, &t.Status, &t.UpstreamStatus,
		&t.UpstreamTaskID, &t.UpstreamFileID, &t.ResultURL, &t.FilePath, &t.FileMime, &t.FileSize, &t.Units, &t.Cost, &t.Error, &t.CreatedAt, &t.UpdatedAt, &t.FinishedAt); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(params), &t.Params)
	if t.Params == nil {
		t.Params = map[string]any{}
	}
	t.HasFile = t.FilePath != "" || t.ResultURL != ""
	return &t, nil
}

func (s *Server) insertTask(t *MediaTask) error {
	params, _ := json.Marshal(t.Params)
	now := nowMs()
	t.CreatedAt, t.UpdatedAt = now, now
	_, err := s.db.Exec(`INSERT INTO media_tasks(id, request_id, key_id, key_name, kind, provider, channel_id, model, upstream_model, mode, prompt, params, status, upstream_task_id, upstream_file_id, units, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, t.ID, t.RequestID, t.KeyID, t.KeyName, t.Kind, t.Provider, t.ChannelID, t.Model, t.UpstreamModel, t.Mode, t.Prompt, string(params),
		t.Status, t.UpstreamTaskID, t.UpstreamFileID, t.Units, now, now)
	return err
}

func (s *Server) taskByID(id string) (*MediaTask, error) {
	return scanTask(s.db.QueryRow(`SELECT `+taskColumns+` FROM media_tasks WHERE id=?`, id))
}

func (s *Server) channelByID(id int64) (*Candidate, error) {
	var c Candidate
	var status int
	err := s.db.QueryRow(`SELECT id, name, type, base_url, api_key, group_id, status FROM channels WHERE id=?`, id).
		Scan(&c.ChannelID, &c.ChannelName, &c.Type, &c.BaseURL, &c.APIKey, &c.GroupID, &status)
	return &c, err
}

// mmURL 构造 MiniMax 原生接口地址（path 形如 /v1/t2a_v2）。
func mmURL(ch *Candidate, path string) string {
	u := strings.TrimRight(ch.BaseURL, "/") + path
	if ch.GroupID != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + "GroupId=" + url.QueryEscape(ch.GroupID)
	}
	return u
}

// mmError 解析 MiniMax 的业务错误（HTTP 200 但 base_resp.status_code != 0）。
func mmError(parsed map[string]any) (int64, string) {
	b, ok := parsed["base_resp"].(map[string]any)
	if !ok {
		return 0, ""
	}
	code, _ := num(b["status_code"])
	return code, str(b["status_msg"])
}

func mmHTTPStatus(code int64) int {
	switch code {
	case 1002, 1039:
		return 429
	case 2013, 1026, 1042, 2038:
		return 400
	}
	return 502
}

func decodeBody(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	err := dec.Decode(&m)
	return m, err
}

func anyString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatInt(int64(t), 10)
	}
	return ""
}

// billChars 按 MiniMax 的计费口径估算字符数：中日韩等宽字符计 2，其余计 1。
func billChars(s string) int64 {
	var n int64
	for _, r := range s {
		if r > 0x2E7F {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// billAs 让本次调用按另一个（按次计费的）模型条目计价，并在日志中以该条目归类。
func (c *call) billAs(name string) {
	c.mc = c.s.modelConfig(name)
	c.s.db.Exec(`UPDATE request_logs SET model=?, kind='voice' WHERE request_id=?`, name, c.reqID)
}

func (c *call) unitPrice() int64 {
	if c.mc == nil {
		return 0
	}
	return c.mc.PriceUnit
}

// readUpstreamJSON 读取 2xx 上游响应并解析；失败时按“费用未知”处理并返回 nil。
func (c *call) readUpstreamJSON(resp *http.Response, t0 time.Time) map[string]any {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	var parsed map[string]any
	if err == nil {
		parsed, err = decodeBody(body)
	}
	if err != nil {
		c.recordAttempt(c.used, "unknown", resp.StatusCode, 0, 0, 0, costPending, "读取或解析上游响应失败: "+err.Error(), t0)
		o := outcome{costStatus: costPending, pendingNote: "上游返回 2xx 但响应读取/解析失败，费用待核实"}
		if c.r.Context().Err() != nil {
			o.status = "canceled"
			c.fail(499, "client_canceled", "调用方已取消请求", o)
		} else {
			c.fail(502, "upstream_invalid_response", "读取上游响应失败，本次请求费用待核实", o)
		}
		return nil
	}
	return parsed
}

// mmCheck 处理 MiniMax 业务错误：上游明确拒绝，确认未产生用量。
func (c *call) mmCheck(parsed map[string]any, resp *http.Response, t0 time.Time) bool {
	code, msg := mmError(parsed)
	if code == 0 {
		return true
	}
	c.recordAttempt(c.used, "failed", resp.StatusCode, 0, 0, 0, costNone, fmt.Sprintf("上游业务错误 %d: %s", code, msg), t0)
	c.fail(mmHTTPStatus(code), "upstream_error", fmt.Sprintf("上游返回错误（%d）：%s", code, msg), outcome{costStatus: costNone})
	return false
}

func mediaExt(mime string) string {
	switch {
	case strings.Contains(mime, "mp4"):
		return ".mp4"
	case strings.Contains(mime, "wav"):
		return ".wav"
	case strings.Contains(mime, "flac"):
		return ".flac"
	case strings.Contains(mime, "mpeg"), strings.Contains(mime, "mp3"):
		return ".mp3"
	}
	return ".bin"
}

func sniffAudioMime(b []byte) string {
	switch {
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "audio/wav"
	case len(b) > 4 && string(b[:4]) == "fLaC":
		return "audio/flac"
	case len(b) > 8 && string(b[4:8]) == "ftyp":
		return "audio/mp4"
	}
	return "audio/mpeg"
}

func (s *Server) mediaPath(name string) string { return filepath.Join(s.cfg.DataDir, name) }

func (s *Server) saveMedia(name string, data []byte) (string, error) {
	p := s.mediaPath(name)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// download 把远端文件保存到媒体目录（上游下载链接有时效，成品在本地留存）。
func (s *Server) download(ctx context.Context, rawURL string, ch *Candidate, name string) (path string, size int64, mime string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", 0, "", err
	}
	if ch != nil {
		ch.authorize(req)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return "", 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", 0, "", fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}
	p := s.mediaPath(name)
	f, err := os.Create(p + ".tmp")
	if err != nil {
		return "", 0, "", err
	}
	size, err = io.Copy(f, io.LimitReader(resp.Body, 4<<30))
	f.Close()
	if err != nil {
		os.Remove(p + ".tmp")
		return "", 0, "", err
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		return "", 0, "", err
	}
	return p, size, resp.Header.Get("Content-Type"), nil
}

// ———————————————————— 视频生成 ————————————————————

var videoDims = map[string][2]int64{"480p": {864, 480}, "512p": {912, 512}, "720p": {1280, 720}, "768p": {1366, 768}, "1080p": {1920, 1080}, "2k": {2560, 1440}, "4k": {3840, 2160}}

func isMiniMaxV2Video(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "minimax-h3")
}

func (s *Server) handleVideoCreate(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 64<<20)
	if err != nil {
		writeGatewayError(w, 400, "invalid_request", "读取请求失败: "+err.Error())
		return
	}
	var req struct {
		Model           string `json:"model"`
		Prompt          string `json:"prompt"`
		FirstFrameImage string `json:"first_frame_image"`
		Duration        int64  `json:"duration"`
		Resolution      string `json:"resolution"`
		Ratio           string `json:"ratio"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeGatewayError(w, 400, "invalid_json", "请求体不是合法的 JSON")
		return
	}
	c := s.begin(w, r, "/v1/video/generations", "video", req.Model, false, "minimax", "volcengine")
	if c == nil {
		return
	}
	defer c.abandon()
	req.Prompt = strings.TrimSpace(req.Prompt)
	mode := "t2v"
	if req.FirstFrameImage != "" {
		mode = "i2v"
		if !strings.HasPrefix(req.FirstFrameImage, "data:image/") && !strings.HasPrefix(req.FirstFrameImage, "http://") && !strings.HasPrefix(req.FirstFrameImage, "https://") {
			c.reject(400, "invalid_image", "first_frame_image 必须是图片 URL 或 data:image/...;base64 数据")
			return
		}
	}
	if req.Prompt == "" {
		c.reject(400, "prompt_required", "请填写视频描述（prompt）")
		return
	}
	if req.Duration < 0 || req.Duration > 60 {
		c.reject(400, "invalid_duration", "duration 不合法")
		return
	}

	// 费用上界：按次计费取单价；按 Token 计费（Seedance）按 宽×高×帧率×时长/1024 估算并留 20% 余量。
	var upper int64
	if c.mc != nil {
		if c.mc.BillingMode == "token" {
			dim, ok := videoDims[strings.ToLower(req.Resolution)]
			if !ok {
				dim = videoDims["1080p"]
			}
			dur := req.Duration
			if dur <= 0 {
				dur = 15
			}
			tokens := dim[0] * dim[1] * 24 * dur / 1024 * 12 / 10
			upper = tokenCost(0, tokens, 0, c.mc.PriceOut)
		} else {
			upper = c.mc.PriceUnit
		}
	}
	if !c.reserveFor(upper, "task") {
		return
	}

	resp, t0 := c.tryChannels(func(ch *Candidate) (*http.Request, error) {
		var path string
		var payload map[string]any
		contentArr := func() []any {
			arr := []any{map[string]any{"type": "text", "text": req.Prompt}}
			if mode == "i2v" {
				arr = append(arr, map[string]any{"type": "image_url", "image_url": map[string]any{"url": req.FirstFrameImage}, "role": "first_frame"})
			}
			return arr
		}
		switch {
		case ch.Type == "volcengine":
			payload = map[string]any{"model": ch.UpstreamModel, "content": contentArr(), "watermark": false}
			if req.Resolution != "" {
				payload["resolution"] = strings.ToLower(req.Resolution)
			}
			if req.Duration > 0 {
				payload["duration"] = req.Duration
			}
			if req.Ratio != "" {
				payload["ratio"] = req.Ratio
			} else if mode == "t2v" {
				payload["ratio"] = "16:9"
			}
			return jsonRequest(c, ch, strings.TrimRight(ch.BaseURL, "/")+"/contents/generations/tasks", payload)
		case isMiniMaxV2Video(ch.UpstreamModel):
			path = "/v2/video_generation"
			res, dur := strings.ToUpper(req.Resolution), req.Duration
			if res == "" {
				res = "768P"
			}
			if dur <= 0 {
				dur = 5
			}
			payload = map[string]any{"model": ch.UpstreamModel, "content": contentArr(), "resolution": res, "duration": dur}
			if req.Ratio != "" {
				payload["ratio"] = req.Ratio
			} else if mode == "t2v" {
				payload["ratio"] = "16:9"
			}
		default: // MiniMax 旧版 Hailuo 接口
			path = "/v1/video_generation"
			payload = map[string]any{"model": ch.UpstreamModel, "prompt": req.Prompt}
			if mode == "i2v" {
				payload["first_frame_image"] = req.FirstFrameImage
			}
			if req.Duration > 0 {
				payload["duration"] = req.Duration
			}
			if req.Resolution != "" {
				payload["resolution"] = strings.ToUpper(req.Resolution)
			}
		}
		return jsonRequest(c, ch, mmURL(ch, path), payload)
	})
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	parsed := c.readUpstreamJSON(resp, t0)
	if parsed == nil {
		return
	}
	if c.used.Type == "minimax" && !c.mmCheck(parsed, resp, t0) {
		return
	}
	upID := anyString(parsed["task_id"])
	if upID == "" {
		upID = anyString(parsed["id"])
	}
	if upID == "" {
		if t, ok := parsed["task"].(map[string]any); ok {
			upID = anyString(t["id"])
		}
	}
	if upID == "" {
		c.recordAttempt(c.used, "unknown", resp.StatusCode, 0, 0, 0, costPending, "上游未返回任务 ID", t0)
		c.fail(502, "upstream_invalid_response", "上游未返回任务 ID，本次请求费用待核实", outcome{costStatus: costPending, pendingNote: "视频任务创建响应缺少任务 ID，费用待核实"})
		return
	}
	c.recordAttempt(c.used, "success", resp.StatusCode, 0, 0, 0, costPending, "", t0)
	task := &MediaTask{ID: newID("task_"), RequestID: c.reqID, KeyID: c.key.ID, KeyName: c.key.Name, Kind: "video", Provider: c.used.Type, ChannelID: c.used.ChannelID,
		Model: c.model, UpstreamModel: c.used.UpstreamModel, Mode: mode, Prompt: req.Prompt, Status: "queued", UpstreamTaskID: upID,
		Params: map[string]any{"duration": req.Duration, "resolution": req.Resolution, "ratio": req.Ratio, "has_first_frame": mode == "i2v"}}
	c.acceptTask(task)
}

// acceptTask 登记异步任务：预留保持到任务结束后再结算或释放。
func (c *call) acceptTask(task *MediaTask) bool {
	if err := c.s.insertTask(task); err != nil {
		log.Printf("[%s] 保存任务失败: %v", c.reqID, err)
		c.fail(500, "internal_error", "保存任务失败，本次请求费用待核实", outcome{costStatus: costPending, pendingNote: "上游已受理任务但网关保存任务失败，费用待核实"})
		return false
	}
	task.HasFile = false
	writeJSON(c.w, 200, task)
	c.finish(outcome{status: "success", httpStatus: 200, costStatus: costPending, keepHold: true, units: task.Units})
	c.s.kickTasks()
	return true
}

func (s *Server) kickTasks() {
	select {
	case s.taskKick <- struct{}{}:
	default:
	}
}

// ———————————————————— 语音合成 ————————————————————

type speechReq struct {
	Model  string   `json:"model"`
	Input  string   `json:"input"`
	Voice  string   `json:"voice"`
	Speed  *float64 `json:"speed"`
	Vol    *float64 `json:"vol"`
	Pitch  *float64 `json:"pitch"`
	Format string   `json:"response_format"`
	Stream bool     `json:"stream"`
}

func (q *speechReq) voiceSetting() map[string]any {
	vs := map[string]any{"voice_id": q.Voice, "speed": 1.0, "vol": 1.0, "pitch": 0}
	if q.Speed != nil {
		vs["speed"] = *q.Speed
	}
	if q.Vol != nil {
		vs["vol"] = *q.Vol
	}
	if q.Pitch != nil {
		vs["pitch"] = int(*q.Pitch)
	}
	return vs
}

func (q *speechReq) validate() (string, string) {
	q.Input = strings.TrimSpace(q.Input)
	if q.Input == "" {
		return "input_required", "请填写要朗读的文本（input）"
	}
	if q.Voice == "" {
		return "voice_required", "请选择音色（voice）"
	}
	if q.Speed != nil && (*q.Speed < 0.5 || *q.Speed > 2) {
		return "invalid_speed", "语速 speed 的范围是 0.5 ~ 2"
	}
	switch q.Format {
	case "":
		q.Format = "mp3"
	case "mp3", "wav", "flac":
	default:
		return "invalid_format", "response_format 仅支持 mp3、wav、flac"
	}
	return "", ""
}

func audioMime(format string) string {
	switch format {
	case "wav":
		return "audio/wav"
	case "flac":
		return "audio/flac"
	}
	return "audio/mpeg"
}

func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	var req speechReq
	body, err := readBody(r, 4<<20)
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		writeGatewayError(w, 400, "invalid_json", "请求体不是合法的 JSON")
		return
	}
	c := s.begin(w, r, "/v1/audio/speech", "tts", req.Model, req.Stream, "minimax")
	if c == nil {
		return
	}
	defer c.abandon()
	if code, msg := req.validate(); code != "" {
		c.reject(400, code, msg)
		return
	}
	if utf8.RuneCountInString(req.Input) >= 10000 {
		c.reject(400, "input_too_long", "同步朗读的文本需少于 10000 字符，更长的文本请使用“长文本配音”")
		return
	}
	if req.Stream && req.Format != "mp3" {
		c.reject(400, "invalid_format", "流式朗读仅支持 mp3")
		return
	}
	// 上界：字节数不小于计费字符数（中文 3 字节计 2 字符）
	var upper int64
	if c.mc != nil {
		upper = tokenCost(int64(len(req.Input)), 0, c.mc.PriceIn, 0)
	}
	if !c.reserveFor(upper, "sync") {
		return
	}
	resp, t0 := c.tryChannels(func(ch *Candidate) (*http.Request, error) {
		payload := map[string]any{"model": ch.UpstreamModel, "text": req.Input, "stream": req.Stream, "voice_setting": req.voiceSetting(),
			"audio_setting": map[string]any{"sample_rate": 32000, "bitrate": 128000, "format": req.Format, "channel": 1}}
		if req.Stream {
			payload["stream_options"] = map[string]any{"exclude_aggregated_audio": true}
		}
		return jsonRequest(c, ch, mmURL(ch, "/v1/t2a_v2"), payload)
	})
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	if req.Stream && strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		c.proxySpeechStream(resp, t0, &req)
		return
	}
	parsed := c.readUpstreamJSON(resp, t0)
	if parsed == nil || !c.mmCheck(parsed, resp, t0) {
		return
	}
	data, _ := parsed["data"].(map[string]any)
	audio, err := hex.DecodeString(str(data["audio"]))
	if err != nil || len(audio) == 0 {
		c.recordAttempt(c.used, "unknown", resp.StatusCode, 0, 0, 0, costPending, "上游未返回音频数据", t0)
		c.fail(502, "upstream_invalid_response", "上游未返回音频数据，本次请求费用待核实", outcome{costStatus: costPending, pendingNote: "语音合成响应缺少音频，费用待核实"})
		return
	}
	o := c.speechOutcome(parsed, &req, true)
	c.recordAttempt(c.used, "success", resp.StatusCode, 0, 0, o.cost, o.costStatus, "", t0)
	h := c.w.Header()
	h.Set("Content-Type", audioMime(req.Format))
	h.Set("Content-Length", strconv.Itoa(len(audio)))
	h.Set("X-Usage-Characters", strconv.FormatInt(o.units, 10))
	h.Set("X-Usage-Source", o.usageSource)
	c.w.WriteHeader(200)
	c.w.Write(audio)
	c.finish(o)
}

// speechOutcome 根据上游 extra_info.usage_characters 结算；缺失时按计费口径估算。
func (c *call) speechOutcome(parsed map[string]any, req *speechReq, ok bool) outcome {
	o := outcome{status: "success", httpStatus: 200}
	if extra, _ := parsed["extra_info"].(map[string]any); extra != nil {
		o.units, _ = num(extra["usage_characters"])
	}
	if o.units > 0 {
		o.costStatus, o.usageSource = costActual, "upstream"
	} else {
		o.units = billChars(req.Input)
		o.costStatus, o.usageSource = costEstimated, "estimated"
	}
	if c.mc != nil {
		o.cost = tokenCost(o.units, 0, c.mc.PriceIn, 0)
	}
	return o
}

func (c *call) proxySpeechStream(resp *http.Response, t0 time.Time, req *speechReq) {
	idle := time.AfterFunc(c.s.cfg.StreamIdleTimeout, c.cancel)
	defer idle.Stop()
	flusher, _ := c.w.(http.Flusher)
	reader := bufio.NewReaderSize(resp.Body, 256<<10)
	var (
		started, clientGone, done bool
		streamed                  int64
		final                     map[string]any
		readErr                   error
	)
	start := func() {
		if !started {
			started = true
			h := c.w.Header()
			h.Set("Content-Type", "audio/mpeg")
			h.Set("Cache-Control", "no-cache")
			h.Set("X-Accel-Buffering", "no")
			c.w.WriteHeader(200)
		}
	}
	for !done && !clientGone {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			idle.Reset(c.s.cfg.StreamIdleTimeout)
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				trimmed = bytes.TrimSpace(trimmed[5:])
			}
			if len(trimmed) > 0 && trimmed[0] == '{' {
				if chunk, e := decodeBody(trimmed); e == nil {
					if code, msg := mmError(chunk); code != 0 {
						if !started {
							c.recordAttempt(c.used, "failed", resp.StatusCode, 0, 0, 0, costNone, fmt.Sprintf("上游业务错误 %d: %s", code, msg), t0)
							c.fail(mmHTTPStatus(code), "upstream_error", fmt.Sprintf("上游返回错误（%d）：%s", code, msg), outcome{costStatus: costNone})
							return
						}
						readErr = fmt.Errorf("上游业务错误 %d: %s", code, msg)
						break
					}
					data, _ := chunk["data"].(map[string]any)
					status, _ := num(data["status"])
					audio, _ := hex.DecodeString(str(data["audio"]))
					if status == 2 {
						final, done = chunk, true
						// 结束块默认会重复整段音频；仅当它恰好是尚未下发的剩余部分时才转发
						var total int64
						if extra, _ := chunk["extra_info"].(map[string]any); extra != nil {
							total, _ = num(extra["audio_size"])
						}
						if !(len(audio) > 0 && total > 0 && streamed+int64(len(audio)) == total) {
							audio = nil
						}
					}
					if len(audio) > 0 {
						start()
						if _, werr := c.w.Write(audio); werr != nil {
							clientGone = true
							c.cancel()
						} else {
							streamed += int64(len(audio))
							if flusher != nil {
								flusher.Flush()
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	if c.r.Context().Err() != nil {
		clientGone = true
	}
	var o outcome
	switch {
	case final != nil:
		o = c.speechOutcome(final, req, true)
		start()
	case streamed > 0 || clientGone:
		// 已开始合成后被停止或中断：上游通常按整段文本计费，按全文估算，不视为免费
		o = outcome{httpStatus: 200, units: billChars(req.Input), costStatus: costEstimated, usageSource: "estimated"}
		if c.mc != nil {
			o.cost = tokenCost(o.units, 0, c.mc.PriceIn, 0)
		}
		if clientGone {
			o.status, o.httpStatus, o.errCode, o.errMsg = "canceled", 499, "client_canceled", "调用方停止播放或断开连接"
		} else {
			o.status, o.errCode, o.errMsg = "error", "upstream_interrupted", "上游音频流中断"
		}
	default:
		msg := "上游音频流在返回数据前中断"
		if readErr != nil {
			msg += ": " + readErr.Error()
		}
		c.recordAttempt(c.used, "unknown", resp.StatusCode, 0, 0, 0, costPending, msg, t0)
		c.fail(502, "upstream_interrupted", msg+"，本次请求费用待核实", outcome{costStatus: costPending, pendingNote: "流式语音在收到数据前中断，费用待核实"})
		return
	}
	st := "success"
	if o.status != "success" {
		st = "interrupted"
	}
	c.recordAttempt(c.used, st, resp.StatusCode, 0, 0, o.cost, o.costStatus, o.errMsg, t0)
	c.finish(o)
}

// ———————————————————— 长文本配音（异步） ————————————————————

func (s *Server) handleSpeechAsync(w http.ResponseWriter, r *http.Request) {
	var req speechReq
	body, err := readBody(r, 8<<20)
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		writeGatewayError(w, 400, "invalid_json", "请求体不是合法的 JSON")
		return
	}
	c := s.begin(w, r, "/v1/audio/speech/async", "tts", req.Model, false, "minimax")
	if c == nil {
		return
	}
	defer c.abandon()
	if code, msg := req.validate(); code != "" {
		c.reject(400, code, msg)
		return
	}
	if billChars(req.Input) > 100000 {
		c.reject(400, "input_too_long", "长文本配音单次最多 10 万计费字符（中文每字计 2 字符），请拆分后提交")
		return
	}
	var upper int64
	if c.mc != nil {
		upper = tokenCost(int64(len(req.Input)), 0, c.mc.PriceIn, 0)
	}
	if !c.reserveFor(upper, "task") {
		return
	}
	resp, t0 := c.tryChannels(func(ch *Candidate) (*http.Request, error) {
		return jsonRequest(c, ch, mmURL(ch, "/v1/t2a_async_v2"), map[string]any{"model": ch.UpstreamModel, "text": req.Input, "voice_setting": req.voiceSetting(),
			"audio_setting": map[string]any{"audio_sample_rate": 32000, "bitrate": 128000, "format": req.Format, "channel": 1}})
	})
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	parsed := c.readUpstreamJSON(resp, t0)
	if parsed == nil || !c.mmCheck(parsed, resp, t0) {
		return
	}
	upID := anyString(parsed["task_id"])
	if upID == "" {
		c.recordAttempt(c.used, "unknown", resp.StatusCode, 0, 0, 0, costPending, "上游未返回任务 ID", t0)
		c.fail(502, "upstream_invalid_response", "上游未返回任务 ID，本次请求费用待核实", outcome{costStatus: costPending, pendingNote: "长文本配音创建响应缺少任务 ID，费用待核实"})
		return
	}
	units, _ := num(parsed["usage_characters"])
	c.recordAttempt(c.used, "success", resp.StatusCode, 0, 0, 0, costPending, "", t0)
	task := &MediaTask{ID: newID("task_"), RequestID: c.reqID, KeyID: c.key.ID, KeyName: c.key.Name, Kind: "tts_async", Provider: "minimax", ChannelID: c.used.ChannelID,
		Model: c.model, UpstreamModel: c.used.UpstreamModel, Mode: "tts", Prompt: truncate(req.Input, 300), Status: "processing", UpstreamTaskID: upID,
		UpstreamFileID: anyString(parsed["file_id"]), Units: units,
		Params: map[string]any{"voice": req.Voice, "format": req.Format, "chars": utf8.RuneCountInString(req.Input), "bill_chars_estimate": billChars(req.Input)}}
	c.acceptTask(task)
}

// ———————————————————— 音乐生成 ————————————————————

func (s *Server) handleMusicCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Lyrics string `json:"lyrics"`
	}
	body, err := readBody(r, 1<<20)
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		writeGatewayError(w, 400, "invalid_json", "请求体不是合法的 JSON")
		return
	}
	c := s.begin(w, r, "/v1/music/generations", "music", req.Model, false, "minimax")
	if c == nil {
		return
	}
	defer c.abandon()
	req.Prompt, req.Lyrics = strings.TrimSpace(req.Prompt), strings.TrimSpace(req.Lyrics)
	if req.Prompt == "" || req.Lyrics == "" {
		c.reject(400, "prompt_lyrics_required", "请同时填写风格描述（prompt）和歌词（lyrics）")
		return
	}
	if utf8.RuneCountInString(req.Prompt) > 2000 || utf8.RuneCountInString(req.Lyrics) > 3500 {
		c.reject(400, "input_too_long", "风格描述最多 2000 字符，歌词最多 3500 字符")
		return
	}
	if !c.reserveFor(c.unitPrice(), "task") {
		return
	}
	ch := c.cands[0]
	c.used = &ch
	task := &MediaTask{ID: newID("task_"), RequestID: c.reqID, KeyID: c.key.ID, KeyName: c.key.Name, Kind: "music", Provider: "minimax", ChannelID: ch.ChannelID,
		Model: c.model, UpstreamModel: ch.UpstreamModel, Mode: "music", Prompt: req.Prompt, Status: "processing",
		Params: map[string]any{"lyrics": req.Lyrics}}
	price := c.unitPrice()
	priced := c.mc != nil && c.mc.Priced
	c.attempts = 1
	if !c.acceptTask(task) {
		return
	}
	// 音乐接口是同步长请求：放到后台执行，页面刷新后仍可查看进度
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runMusic(task, &ch, req.Lyrics, price, priced)
	}()
}

func (s *Server) runMusic(task *MediaTask, ch *Candidate, lyrics string, price int64, priced bool) {
	ctx, cancel := context.WithTimeout(s.bg, 20*time.Minute)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"model": ch.UpstreamModel, "prompt": task.Prompt, "lyrics": lyrics, "output_format": "hex",
		"audio_setting": map[string]any{"sample_rate": 44100, "bitrate": 256000, "format": "mp3"}})
	req, _ := http.NewRequestWithContext(ctx, "POST", mmURL(ch, "/v1/music_generation"), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	ch.authorize(req)
	s.markDispatched(task.RequestID)
	t0 := time.Now()
	attempt := func(status string, httpStatus int, cost int64, cs, msg string) {
		s.db.Exec(`INSERT INTO upstream_attempts(request_id, attempt, channel_id, channel_name, upstream_model, status, http_status, cost, cost_status, error, duration_ms, created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, task.RequestID, 1, ch.ChannelID, ch.ChannelName, ch.UpstreamModel, status, httpStatus, cost, cs, truncate(msg, 500), time.Since(t0).Milliseconds(), t0.UnixMilli())
	}
	// 独立的长超时客户端：音乐生成可能持续数分钟
	client := &http.Client{Transport: s.httpc.Transport.(*http.Transport).Clone()}
	client.Transport.(*http.Transport).ResponseHeaderTimeout = 20 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		if classifyErr(err, context.Background()) == errNoUsage {
			attempt("failed", 0, 0, costNone, err.Error())
			s.failTask(task, "连接上游失败："+err.Error(), false)
		} else {
			attempt("unknown", 0, 0, costPending, err.Error())
			s.failTask(task, "上游超时或连接中断，费用待核实", true)
		}
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if resp.StatusCode >= 400 {
		msg := upstreamErrMessage(body)
		attempt("failed", resp.StatusCode, 0, costNone, msg)
		s.failTask(task, fmt.Sprintf("上游返回 HTTP %d：%s", resp.StatusCode, msg), false)
		return
	}
	var parsed map[string]any
	if err == nil {
		parsed, err = decodeBody(body)
	}
	if err != nil {
		attempt("unknown", resp.StatusCode, 0, costPending, "读取上游响应失败")
		s.failTask(task, "读取上游响应失败，费用待核实", true)
		return
	}
	if code, msg := mmError(parsed); code != 0 {
		attempt("failed", resp.StatusCode, 0, costNone, msg)
		s.failTask(task, fmt.Sprintf("上游返回错误（%d）：%s", code, msg), false)
		return
	}
	data, _ := parsed["data"].(map[string]any)
	audio, _ := hex.DecodeString(str(data["audio"]))
	if len(audio) == 0 {
		attempt("unknown", resp.StatusCode, 0, costPending, "上游未返回音频")
		s.failTask(task, "上游未返回音频数据，费用待核实", true)
		return
	}
	mime := sniffAudioMime(audio)
	path, err := s.saveMedia(task.ID+mediaExt(mime), audio)
	if err != nil {
		attempt("success", resp.StatusCode, price, costActual, "保存文件失败")
		s.finishTask(task, "", "", 0, "保存音频文件失败："+err.Error(), price, priced, 1)
		return
	}
	attempt("success", resp.StatusCode, price, costActual, "")
	s.finishTask(task, path, mime, int64(len(audio)), "", price, priced, 1)
}

// finishTask 任务成功：登记成品并结算。
func (s *Server) finishTask(task *MediaTask, path, mime string, size int64, warn string, cost int64, priced bool, units int64) {
	cs := costActual
	if !priced {
		cost, cs = 0, costUnpriced
	}
	now := nowMs()
	s.db.Exec(`UPDATE media_tasks SET status='succeeded', file_path=?, file_mime=?, file_size=?, error=?, cost=?, units=CASE WHEN ?>0 THEN ? ELSE units END, updated_at=?, finished_at=? WHERE id=?`,
		path, mime, size, warn, cost, units, units, now, now, task.ID)
	if err := s.settle(task.RequestID, cost, cs, "任务成功，按实际结算", units); err != nil {
		log.Printf("[%s] 任务结算失败: %v", task.RequestID, err)
	}
}

// failTask 任务失败：确认未产生费用则释放预留；费用未知则转待核实。
func (s *Server) failTask(task *MediaTask, msg string, unknownCost bool) {
	now := nowMs()
	s.db.Exec(`UPDATE media_tasks SET status='failed', error=?, updated_at=?, finished_at=? WHERE id=?`, truncate(msg, 500), now, now, task.ID)
	// 请求日志同步标记为失败，便于按请求 ID 定位原因
	s.db.Exec(`UPDATE request_logs SET status='error', error_code='task_failed', error_message=? WHERE request_id=?`, truncate(msg, 1000), task.RequestID)
	var err error
	if unknownCost {
		err = s.markPending(task.RequestID, "异步任务结果未知："+msg)
	} else {
		err = s.settle(task.RequestID, 0, costNone, "任务失败，未产生费用", 0)
	}
	if err != nil {
		log.Printf("[%s] 任务失败后的账务处理出错: %v", task.RequestID, err)
	}
}

// ———————————————————— 任务轮询 ————————————————————

func (s *Server) taskLoop() {
	defer s.wg.Done()
	// 音乐任务没有上游任务 ID，进程重启后无法续查：标记失败并转待核实
	rows, err := s.db.Query(`SELECT ` + taskColumns + ` FROM media_tasks WHERE kind='music' AND status IN ('queued','processing')`)
	if err == nil {
		var stale []*MediaTask
		for rows.Next() {
			if t, e := scanTask(rows); e == nil {
				stale = append(stale, t)
			}
		}
		rows.Close()
		for _, t := range stale {
			s.failTask(t, "网关重启导致音乐生成中断，上游可能已计费，费用待核实", true)
		}
	}
	ticker := time.NewTicker(s.cfg.TaskPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.bg.Done():
			return
		case <-ticker.C:
		case <-s.taskKick:
			select {
			case <-s.bg.Done():
				return
			case <-time.After(min(s.cfg.TaskPollInterval, 2*time.Second)):
			}
		}
		s.pollTasks()
	}
}

func (s *Server) pollTasks() {
	rows, err := s.db.Query(`SELECT ` + taskColumns + ` FROM media_tasks WHERE kind IN ('video','tts_async') AND status IN ('queued','processing') ORDER BY created_at`)
	if err != nil {
		return
	}
	var tasks []*MediaTask
	for rows.Next() {
		if t, e := scanTask(rows); e == nil {
			tasks = append(tasks, t)
		}
	}
	rows.Close()
	for _, t := range tasks {
		if s.bg.Err() != nil {
			return
		}
		s.pollTask(t)
	}
}

func (s *Server) getJSON(ctx context.Context, ch *Candidate, u string) (map[string]any, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, 0, err
	}
	ch.authorize(req)
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	parsed, err := decodeBody(body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(string(body), 200))
	}
	if resp.StatusCode >= 400 {
		return parsed, resp.StatusCode, fmt.Errorf("HTTP %d：%s", resp.StatusCode, upstreamErrMessage(body))
	}
	return parsed, resp.StatusCode, nil
}

func (s *Server) pollTask(t *MediaTask) {
	if nowMs()-t.CreatedAt > (6 * time.Hour).Milliseconds() {
		s.failTask(t, "任务超过 6 小时仍未完成，已停止跟踪；上游可能已计费，费用待核实", true)
		return
	}
	ch, err := s.channelByID(t.ChannelID)
	if err != nil {
		s.failTask(t, "任务所属渠道已被删除，无法继续查询；费用待核实", true)
		return
	}
	mc := s.modelConfig(t.Model)
	priced := mc != nil && mc.Priced
	setStatus := func(status, upstream string) {
		if status != t.Status || upstream != t.UpstreamStatus {
			s.db.Exec(`UPDATE media_tasks SET status=?, upstream_status=?, updated_at=? WHERE id=?`, status, upstream, nowMs(), t.ID)
		}
	}
	switch {
	case t.Kind == "video":
		var statusRaw, videoURL, fileID, errMsg string
		var tokens int64
		var parsed map[string]any
		v2 := ch.Type == "volcengine" || isMiniMaxV2Video(t.UpstreamModel)
		if ch.Type == "volcengine" {
			parsed, _, err = s.getJSON(s.bg, ch, strings.TrimRight(ch.BaseURL, "/")+"/contents/generations/tasks/"+url.PathEscape(t.UpstreamTaskID))
		} else if v2 {
			parsed, _, err = s.getJSON(s.bg, ch, mmURL(ch, "/v2/query/video_generation/"+url.PathEscape(t.UpstreamTaskID)))
		} else {
			parsed, _, err = s.getJSON(s.bg, ch, mmURL(ch, "/v1/query/video_generation?task_id="+url.QueryEscape(t.UpstreamTaskID)))
		}
		if err != nil {
			log.Printf("[%s] 查询视频任务失败（稍后重试）: %v", t.ID, err)
			return
		}
		obj := parsed
		if inner, ok := parsed["task"].(map[string]any); ok {
			obj = inner
		}
		statusRaw = str(obj["status"])
		if content, ok := obj["content"].(map[string]any); ok {
			videoURL = str(content["video_url"])
			if videoURL == "" {
				videoURL = str(content["url"])
			}
		}
		if e, ok := obj["error"].(map[string]any); ok {
			errMsg = str(e["message"])
		}
		if u, ok := obj["usage"].(map[string]any); ok {
			tokens, _ = num(u["completion_tokens"])
			if tokens == 0 {
				tokens, _ = num(u["total_tokens"])
			}
		}
		fileID = anyString(obj["file_id"])
		if !v2 {
			if code, msg := mmError(parsed); code != 0 && errMsg == "" {
				errMsg = msg
			}
		}
		switch strings.ToLower(statusRaw) {
		case "succeeded", "success":
			if videoURL == "" && fileID != "" {
				fr, _, ferr := s.getJSON(s.bg, ch, mmURL(ch, "/v1/files/retrieve?file_id="+url.QueryEscape(fileID)))
				if ferr != nil {
					log.Printf("[%s] 获取视频下载地址失败（稍后重试）: %v", t.ID, ferr)
					return
				}
				if f, ok := fr["file"].(map[string]any); ok {
					videoURL = str(f["download_url"])
				}
			}
			var cost, units int64 = 0, 1
			if mc != nil {
				if mc.BillingMode == "token" && tokens > 0 {
					cost, units = tokenCost(0, tokens, 0, mc.PriceOut), tokens
				} else {
					cost = mc.PriceUnit
				}
			}
			var path, mime, warn string
			var size int64
			if videoURL != "" {
				path, size, mime, err = s.download(s.bg, videoURL, nil, t.ID+".mp4")
				if err != nil {
					path, warn = "", "成品已生成，但保存到网关失败（将直接使用上游的限时链接）："+err.Error()
				}
				if mime == "" || !strings.HasPrefix(mime, "video/") {
					mime = "video/mp4"
				}
				s.db.Exec(`UPDATE media_tasks SET result_url=?, upstream_status=? WHERE id=?`, videoURL, statusRaw, t.ID)
			} else {
				warn = "上游报告成功但没有返回视频地址"
			}
			if tokens > 0 {
				s.db.Exec(`UPDATE request_logs SET completion_tokens=?, total_tokens=? WHERE request_id=?`, tokens, tokens, t.RequestID)
			}
			s.finishTask(t, path, mime, size, warn, cost, priced, units)
		case "failed", "fail", "cancelled", "canceled", "expired":
			if errMsg == "" {
				errMsg = "上游任务状态：" + statusRaw
			}
			s.db.Exec(`UPDATE media_tasks SET upstream_status=? WHERE id=?`, statusRaw, t.ID)
			s.failTask(t, "视频生成失败："+errMsg, false)
		case "queued", "preparing", "queueing":
			setStatus("queued", statusRaw)
		default:
			setStatus("processing", statusRaw)
		}
	case t.Kind == "tts_async":
		parsed, _, err := s.getJSON(s.bg, ch, mmURL(ch, "/v1/query/t2a_async_query_v2?task_id="+url.QueryEscape(t.UpstreamTaskID)))
		if err != nil {
			log.Printf("[%s] 查询配音任务失败（稍后重试）: %v", t.ID, err)
			return
		}
		if code, msg := mmError(parsed); code != 0 {
			log.Printf("[%s] 查询配音任务返回错误 %d: %s（稍后重试）", t.ID, code, msg)
			return
		}
		statusRaw := str(parsed["status"])
		switch strings.ToLower(statusRaw) {
		case "success":
			fileID := anyString(parsed["file_id"])
			if fileID == "" || fileID == "0" {
				fileID = t.UpstreamFileID
			}
			audio, err := s.fetchMiniMaxFile(ch, fileID)
			var cost int64
			if mc != nil {
				cost = tokenCost(t.Units, 0, mc.PriceIn, 0)
			}
			if err != nil {
				s.finishTask(t, "", "", 0, "配音已完成，但下载成品失败："+err.Error(), cost, priced, t.Units)
				return
			}
			mime := sniffAudioMime(audio)
			path, err := s.saveMedia(t.ID+mediaExt(mime), audio)
			warn := ""
			if err != nil {
				warn = "保存成品失败：" + err.Error()
			}
			s.db.Exec(`UPDATE media_tasks SET upstream_status=? WHERE id=?`, statusRaw, t.ID)
			s.finishTask(t, path, mime, int64(len(audio)), warn, cost, priced, t.Units)
		case "failed", "expired":
			s.db.Exec(`UPDATE media_tasks SET upstream_status=? WHERE id=?`, statusRaw, t.ID)
			s.failTask(t, "长文本配音失败，上游任务状态："+statusRaw, false)
		default:
			setStatus("processing", statusRaw)
		}
	}
}

// fetchMiniMaxFile 下载 MiniMax 文件；长文本配音的成品可能是 tar 包，从中取出音频。
func (s *Server) fetchMiniMaxFile(ch *Candidate, fileID string) ([]byte, error) {
	if fileID == "" {
		return nil, errors.New("上游未返回文件 ID")
	}
	fetch := func(u string, auth bool) ([]byte, error) {
		ctx, cancel := context.WithTimeout(s.bg, 10*time.Minute)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		if auth {
			ch.authorize(req)
		}
		resp, err := s.httpc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<30))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return b, nil
	}
	var data []byte
	var err error
	if fr, _, ferr := s.getJSON(s.bg, ch, mmURL(ch, "/v1/files/retrieve?file_id="+url.QueryEscape(fileID))); ferr == nil {
		if f, ok := fr["file"].(map[string]any); ok && str(f["download_url"]) != "" {
			data, err = fetch(str(f["download_url"]), false)
		}
	}
	if len(data) == 0 {
		data, err = fetch(mmURL(ch, "/v1/files/retrieve_content?file_id="+url.QueryEscape(fileID)), true)
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[0] == '{' {
		if parsed, e := decodeBody(data); e == nil {
			if code, msg := mmError(parsed); code != 0 {
				return nil, fmt.Errorf("上游返回错误（%d）：%s", code, msg)
			}
		}
	}
	if len(data) > 262 && string(data[257:262]) == "ustar" {
		tr := tar.NewReader(bytes.NewReader(data))
		for {
			hdr, e := tr.Next()
			if e != nil {
				break
			}
			name := strings.ToLower(hdr.Name)
			if strings.HasSuffix(name, ".mp3") || strings.HasSuffix(name, ".wav") || strings.HasSuffix(name, ".flac") {
				return io.ReadAll(tr)
			}
		}
		return nil, errors.New("成品压缩包中没有找到音频文件")
	}
	if len(data) == 0 {
		return nil, errors.New("成品文件为空")
	}
	return data, nil
}

// ———————————————————— 音色 ————————————————————

type voiceCacheEntry struct {
	at     time.Time
	system []map[string]any
}

var fallbackVoices = []map[string]any{
	{"voice_id": "male-qn-qingse", "name": "青涩青年音色"}, {"voice_id": "male-qn-jingying", "name": "精英青年音色"},
	{"voice_id": "female-shaonv", "name": "少女音色"}, {"voice_id": "female-yujie", "name": "御姐音色"},
	{"voice_id": "presenter_male", "name": "男性主持人"}, {"voice_id": "presenter_female", "name": "女性主持人"},
}

func (s *Server) miniMaxChannelFor(model string) *Candidate {
	if model != "" {
		if cands := s.candidates(model, "minimax"); len(cands) > 0 {
			return &cands[0]
		}
	}
	var id int64
	if s.db.QueryRow(`SELECT id FROM channels WHERE type='minimax' AND status=1 ORDER BY priority DESC, id LIMIT 1`).Scan(&id) != nil {
		return nil
	}
	ch, err := s.channelByID(id)
	if err != nil {
		return nil
	}
	return ch
}

func (s *Server) handleVoices(w http.ResponseWriter, r *http.Request) {
	ch := s.miniMaxChannelFor(r.URL.Query().Get("model"))
	custom := s.customVoices()
	if ch == nil {
		writeJSON(w, 200, map[string]any{"system": fallbackVoices, "custom": custom, "fallback": true, "message": "没有可用的 MiniMax 渠道，显示的是内置的常用音色"})
		return
	}
	if v, ok := s.voiceCache.Load(ch.ChannelID); ok && r.URL.Query().Get("refresh") == "" {
		if e := v.(*voiceCacheEntry); time.Since(e.at) < 10*time.Minute {
			writeJSON(w, 200, map[string]any{"system": e.system, "custom": custom, "fallback": false})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", mmURL(ch, "/v1/get_voice"), strings.NewReader(`{"voice_type":"system"}`))
	req.Header.Set("Content-Type", "application/json")
	ch.authorize(req)
	var system []map[string]any
	msg := ""
	resp, err := s.httpc.Do(req)
	if err != nil {
		msg = "获取系统音色失败：" + err.Error()
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		parsed, perr := decodeBody(body)
		if perr != nil || resp.StatusCode >= 400 {
			msg = fmt.Sprintf("获取系统音色失败：HTTP %d", resp.StatusCode)
		} else if code, m := mmError(parsed); code != 0 {
			msg = fmt.Sprintf("获取系统音色失败（%d）：%s", code, m)
		} else if list, ok := parsed["system_voice"].([]any); ok {
			for _, x := range list {
				if m, ok := x.(map[string]any); ok && str(m["voice_id"]) != "" {
					name := str(m["voice_name"])
					if name == "" {
						name = str(m["voice_id"])
					}
					system = append(system, map[string]any{"voice_id": str(m["voice_id"]), "name": name})
				}
			}
		}
	}
	if len(system) == 0 {
		if msg == "" {
			msg = "上游没有返回系统音色"
		}
		writeJSON(w, 200, map[string]any{"system": fallbackVoices, "custom": custom, "fallback": true, "message": msg + "，显示的是内置的常用音色"})
		return
	}
	s.voiceCache.Store(ch.ChannelID, &voiceCacheEntry{at: time.Now(), system: system})
	writeJSON(w, 200, map[string]any{"system": system, "custom": custom, "fallback": false})
}

func (s *Server) customVoices() []map[string]any {
	out := []map[string]any{}
	rows, err := s.db.Query(`SELECT id, voice_id, name, kind, description, preview_path != '', consent_note, created_at FROM voices ORDER BY id DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, created int64
		var voiceID, name, kind, desc, consent string
		var hasPreview bool
		rows.Scan(&id, &voiceID, &name, &kind, &desc, &hasPreview, &consent, &created)
		out = append(out, map[string]any{"id": id, "voice_id": voiceID, "name": name, "kind": kind, "description": desc, "has_preview": hasPreview, "consent_note": consent, "created_at": created})
	}
	return out
}

var voiceIDRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{6,254}[A-Za-z0-9]$`)

func (s *Server) saveVoice(c *call, voiceID, name, kind, desc, consent string, preview []byte) map[string]any {
	var path, mime string
	if len(preview) > 0 {
		mime = sniffAudioMime(preview)
		path, _ = s.saveMedia("voice_"+randHex(8)+mediaExt(mime), preview)
	}
	res, err := s.db.Exec(`INSERT INTO voices(voice_id, name, kind, provider, channel_id, description, preview_path, preview_mime, consent_note, key_id, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(voice_id) DO UPDATE SET name=excluded.name, description=excluded.description, preview_path=excluded.preview_path, preview_mime=excluded.preview_mime`,
		voiceID, name, kind, "minimax", c.used.ChannelID, desc, path, mime, consent, c.key.ID, nowMs())
	var id int64
	if err == nil {
		id, _ = res.LastInsertId()
	}
	s.db.QueryRow(`SELECT id FROM voices WHERE voice_id=?`, voiceID).Scan(&id)
	return map[string]any{"id": id, "voice_id": voiceID, "name": name, "kind": kind, "has_preview": path != ""}
}

// handleVoiceClone 音色复刻：上传已获授权的样音，创建专属音色并生成试听。
func (s *Server) handleVoiceClone(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 24<<20)
	if err := r.ParseMultipartForm(24 << 20); err != nil {
		writeGatewayError(w, 400, "invalid_form", "请以 multipart/form-data 上传样音，文件不超过 20MB")
		return
	}
	model := r.FormValue("model")
	c := s.begin(w, r, "/v1/audio/voices/clone", "tts", model, false, "minimax")
	if c == nil {
		return
	}
	defer c.abandon()
	c.billAs("voice-clone") // 按次计费的占位模型
	if r.FormValue("consent") != "true" {
		c.reject(400, "consent_required", "音色复刻必须确认样音已获得说话人授权")
		return
	}
	voiceID := strings.TrimSpace(r.FormValue("voice_id"))
	if voiceID == "" {
		voiceID = "relay" + randHex(6)
	}
	if !voiceIDRe.MatchString(voiceID) {
		c.reject(400, "invalid_voice_id", "音色 ID 需 8 位以上，以字母开头，只含字母、数字、- 和 _，且不能以 - 或 _ 结尾")
		return
	}
	previewText := strings.TrimSpace(r.FormValue("preview_text"))
	if previewText == "" {
		previewText = "你好，这是用复刻音色朗读的一段试听文本。"
	}
	if utf8.RuneCountInString(previewText) > 1000 {
		c.reject(400, "input_too_long", "试听文本最多 1000 字符")
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		c.reject(400, "file_required", "请上传样音文件（mp3 / m4a / wav，10 秒 ~ 5 分钟，不超过 20MB）")
		return
	}
	sample, _ := io.ReadAll(file)
	file.Close()
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if ext != ".mp3" && ext != ".m4a" && ext != ".wav" {
		c.reject(400, "invalid_file", "样音仅支持 mp3、m4a、wav 格式")
		return
	}
	if len(sample) == 0 || len(sample) > 20<<20 {
		c.reject(400, "invalid_file", "样音文件为空或超过 20MB")
		return
	}
	if !c.reserveFor(c.unitPrice(), "sync") {
		return
	}
	resp, t0 := c.tryChannels(func(ch *Candidate) (*http.Request, error) {
		fileID, err := s.mmUpload(c.ctx, ch, "voice_clone", "sample"+ext, sample)
		if err != nil {
			return nil, err
		}
		var fid any = fileID
		if n, e := strconv.ParseInt(fileID, 10, 64); e == nil {
			fid = n
		}
		return jsonRequest(c, ch, mmURL(ch, "/v1/voice_clone"), map[string]any{"file_id": fid, "voice_id": voiceID, "text": previewText, "model": ch.UpstreamModel})
	})
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	parsed := c.readUpstreamJSON(resp, t0)
	if parsed == nil || !c.mmCheck(parsed, resp, t0) {
		return
	}
	var preview []byte
	demo := str(parsed["demo_audio"])
	if strings.HasPrefix(demo, "http") {
		if p, _, _, err := s.download(c.ctx, demo, nil, "tmp_"+randHex(8)); err == nil {
			preview, _ = os.ReadFile(p)
			os.Remove(p)
		}
	} else if demo != "" {
		preview, _ = hex.DecodeString(demo)
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "复刻音色 " + voiceID
	}
	consent := "调用方确认样音已获说话人授权用于音色复刻；样音文件：" + hdr.Filename
	if n := strings.TrimSpace(r.FormValue("consent_note")); n != "" {
		consent += "；" + truncate(n, 300)
	}
	out := s.saveVoice(c, voiceID, name, "clone", "", consent, preview)
	out["demo_audio"] = demo
	o := outcome{status: "success", httpStatus: 200, units: 1, cost: c.unitPrice(), costStatus: costActual, usageSource: "upstream"}
	c.recordAttempt(c.used, "success", resp.StatusCode, 0, 0, o.cost, o.costStatus, "", t0)
	writeJSON(c.w, 200, out)
	c.finish(o)
}

// handleVoiceDesign 音色设计：根据文字描述生成音色并返回试听。
func (s *Server) handleVoiceDesign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model       string `json:"model"`
		Prompt      string `json:"prompt"`
		PreviewText string `json:"preview_text"`
		Name        string `json:"name"`
	}
	body, err := readBody(r, 1<<20)
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		writeGatewayError(w, 400, "invalid_json", "请求体不是合法的 JSON")
		return
	}
	c := s.begin(w, r, "/v1/audio/voices/design", "tts", req.Model, false, "minimax")
	if c == nil {
		return
	}
	defer c.abandon()
	c.billAs("voice-design")
	req.Prompt, req.PreviewText = strings.TrimSpace(req.Prompt), strings.TrimSpace(req.PreviewText)
	if req.Prompt == "" || req.PreviewText == "" {
		c.reject(400, "prompt_required", "请填写音色描述（prompt）和试听文本（preview_text）")
		return
	}
	if utf8.RuneCountInString(req.PreviewText) > 500 {
		c.reject(400, "input_too_long", "试听文本最多 500 字符")
		return
	}
	if !c.reserveFor(c.unitPrice(), "sync") {
		return
	}
	resp, t0 := c.tryChannels(func(ch *Candidate) (*http.Request, error) {
		return jsonRequest(c, ch, mmURL(ch, "/v1/voice_design"), map[string]any{"prompt": req.Prompt, "preview_text": req.PreviewText})
	})
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	parsed := c.readUpstreamJSON(resp, t0)
	if parsed == nil || !c.mmCheck(parsed, resp, t0) {
		return
	}
	voiceID := str(parsed["voice_id"])
	if voiceID == "" {
		c.recordAttempt(c.used, "unknown", resp.StatusCode, 0, 0, 0, costPending, "上游未返回音色 ID", t0)
		c.fail(502, "upstream_invalid_response", "上游未返回音色 ID，本次请求费用待核实", outcome{costStatus: costPending, pendingNote: "音色设计响应缺少音色 ID，费用待核实"})
		return
	}
	preview, _ := hex.DecodeString(str(parsed["trial_audio"]))
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "设计音色 " + truncate(req.Prompt, 24)
	}
	out := s.saveVoice(c, voiceID, name, "design", req.Prompt, "", preview)
	o := outcome{status: "success", httpStatus: 200, units: 1, cost: c.unitPrice(), costStatus: costActual, usageSource: "upstream"}
	c.recordAttempt(c.used, "success", resp.StatusCode, 0, 0, o.cost, o.costStatus, "", t0)
	writeJSON(c.w, 200, out)
	c.finish(o)
}

func (s *Server) mmUpload(ctx context.Context, ch *Candidate, purpose, filename string, data []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("purpose", purpose)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(data)
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", mmURL(ch, "/v1/files/upload"), &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ch.authorize(req)
	resp, err := s.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传样音失败：%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	parsed, perr := decodeBody(body)
	if perr != nil || resp.StatusCode >= 400 {
		return "", fmt.Errorf("上传样音失败：HTTP %d %s", resp.StatusCode, upstreamErrMessage(body))
	}
	if code, msg := mmError(parsed); code != 0 {
		return "", fmt.Errorf("上传样音失败（%d）：%s", code, msg)
	}
	f, _ := parsed["file"].(map[string]any)
	id := anyString(f["file_id"])
	if id == "" {
		return "", errors.New("上传样音失败：上游未返回文件 ID")
	}
	return id, nil
}

// ———————————————————— 任务查询与成品下载 ————————————————————

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request, keyID int64) {
	conds, args := []string{"1=1"}, []any{}
	if keyID > 0 {
		conds, args = append(conds, "key_id=?"), append(args, keyID)
	}
	if k := r.URL.Query().Get("kind"); k != "" {
		conds, args = append(conds, "kind=?"), append(args, k)
	}
	limit := qInt(r, "limit", 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+taskColumns+` FROM media_tasks WHERE `+strings.Join(conds, " AND ")+` ORDER BY created_at DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	items := []*MediaTask{}
	for rows.Next() {
		if t, e := scanTask(rows); e == nil {
			items = append(items, t)
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) serveTaskFile(w http.ResponseWriter, r *http.Request, t *MediaTask) {
	if t.FilePath == "" {
		if t.ResultURL != "" {
			http.Redirect(w, r, t.ResultURL, http.StatusFound)
			return
		}
		writeJSON(w, 404, map[string]any{"error": "成品尚未生成"})
		return
	}
	f, err := os.Open(t.FilePath)
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "成品文件不存在"})
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", t.FileMime)
	if r.URL.Query().Get("download") != "" {
		names := map[string]string{"video": "视频", "tts_async": "配音", "music": "音乐"}
		name := names[t.Kind] + "_" + time.UnixMilli(t.CreatedAt).Format("20060102_150405") + filepath.Ext(t.FilePath)
		w.Header().Set("Content-Disposition", "attachment; filename=\""+t.ID+filepath.Ext(t.FilePath)+"\"; filename*=UTF-8''"+url.PathEscape(name))
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) { s.listTasks(w, r, keyFrom(r).ID) }

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.taskByID(r.PathValue("id"))
	if err != nil || t.KeyID != keyFrom(r).ID {
		writeGatewayError(w, 404, "task_not_found", "任务不存在")
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) handleTaskContent(w http.ResponseWriter, r *http.Request) {
	t, err := s.taskByID(r.PathValue("id"))
	if err != nil || t.KeyID != keyFrom(r).ID {
		writeGatewayError(w, 404, "task_not_found", "任务不存在")
		return
	}
	s.serveTaskFile(w, r, t)
}

func (s *Server) handleAdminTaskList(w http.ResponseWriter, r *http.Request) { s.listTasks(w, r, 0) }

func (s *Server) handleAdminTaskGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.taskByID(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, 404, "任务不存在")
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) handleAdminTaskContent(w http.ResponseWriter, r *http.Request) {
	t, err := s.taskByID(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, 404, "任务不存在")
		return
	}
	s.serveTaskFile(w, r, t)
}

func (s *Server) handleAdminTaskDelete(w http.ResponseWriter, r *http.Request) {
	t, err := s.taskByID(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, 404, "任务不存在")
		return
	}
	if t.Status == "queued" || t.Status == "processing" {
		writeAdminError(w, 400, "任务仍在进行中，暂不能删除")
		return
	}
	if t.FilePath != "" {
		os.Remove(t.FilePath)
	}
	s.db.Exec(`DELETE FROM media_tasks WHERE id=?`, t.ID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminVoiceList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"items": s.customVoices()})
}

func (s *Server) handleAdminVoicePreview(w http.ResponseWriter, r *http.Request) {
	var path, mime string
	if s.db.QueryRow(`SELECT preview_path, preview_mime FROM voices WHERE id=?`, r.PathValue("id")).Scan(&path, &mime) != nil || path == "" {
		writeAdminError(w, 404, "该音色没有试听音频")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeAdminError(w, 404, "试听文件不存在")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", mime)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *Server) handleAdminVoiceDelete(w http.ResponseWriter, r *http.Request) {
	var path string
	s.db.QueryRow(`SELECT preview_path FROM voices WHERE id=?`, r.PathValue("id")).Scan(&path)
	if path != "" {
		os.Remove(path)
	}
	// 只删除网关侧的登记；上游的音色按其自身规则保留或过期
	s.db.Exec(`DELETE FROM voices WHERE id=?`, r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"ok": true})
}
