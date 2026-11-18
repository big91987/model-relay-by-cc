package gateway

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ModelConfig struct {
	Name                string  `json:"name"`
	Kind                string  `json:"kind"`         // chat | embedding | video | tts | music | voice
	BillingMode         string  `json:"billing_mode"` // token | char | request
	PriceIn             int64   `json:"price_in"`     // 微元 / 百万 Token（或百万字符）
	PriceOut            int64   `json:"price_out"`    // 微元 / 百万 Token
	PriceUnit           int64   `json:"price_unit"`   // 微元 / 次
	Priced              bool    `json:"priced"`
	MaxOutputTokens     int64   `json:"max_output_tokens"`
	DefaultMaxTokens    int64   `json:"default_max_tokens"`
	SupportsTemperature bool    `json:"supports_temperature"`
	TemperatureMax      float64 `json:"temperature_max"`
	Note                string  `json:"note"`
	UpdatedAt           int64   `json:"updated_at"`
}

const modelColumns = `name, kind, billing_mode, price_in, price_out, price_unit, priced, max_output_tokens, default_max_tokens, supports_temperature, temperature_max, note, updated_at`

func scanModel(row rowScanner) (*ModelConfig, error) {
	var m ModelConfig
	var priced, st int
	if err := row.Scan(&m.Name, &m.Kind, &m.BillingMode, &m.PriceIn, &m.PriceOut, &m.PriceUnit, &priced, &m.MaxOutputTokens, &m.DefaultMaxTokens, &st, &m.TemperatureMax, &m.Note, &m.UpdatedAt); err != nil {
		return nil, err
	}
	m.Priced, m.SupportsTemperature = priced == 1, st == 1
	return &m, nil
}

func (s *Server) modelConfig(name string) *ModelConfig {
	m, err := scanModel(s.db.QueryRow(`SELECT `+modelColumns+` FROM models WHERE name=?`, name))
	if err != nil {
		return nil
	}
	return m
}

// Candidate 是某个模型的一个可用渠道。
type Candidate struct {
	ChannelID     int64
	ChannelName   string
	Type          string
	BaseURL       string
	APIKey        string
	GroupID       string
	Priority      int
	Weight        int
	UpstreamModel string
}

func (s *Server) candidates(model string, types ...string) []Candidate {
	rows, err := s.db.Query(`SELECT c.id, c.name, c.type, c.base_url, c.api_key, c.group_id, c.priority, c.weight, cm.upstream_model
		FROM channel_models cm JOIN channels c ON c.id = cm.channel_id WHERE cm.model = ? AND c.status = 1`, model)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []Candidate
	for rows.Next() {
		var c Candidate
		rows.Scan(&c.ChannelID, &c.ChannelName, &c.Type, &c.BaseURL, &c.APIKey, &c.GroupID, &c.Priority, &c.Weight, &c.UpstreamModel)
		if len(types) > 0 {
			ok := false
			for _, t := range types {
				ok = ok || t == c.Type
			}
			if !ok {
				continue
			}
		}
		list = append(list, c)
	}
	// 优先级高的在前；同优先级内按权重随机
	keys := make([]float64, len(list))
	for i, c := range list {
		keys[i] = rand.Float64() * float64(max(c.Weight, 1))
	}
	idx := make([]int, len(list))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ca, cb := list[idx[a]], list[idx[b]]
		if ca.Priority != cb.Priority {
			return ca.Priority > cb.Priority
		}
		return keys[idx[a]] > keys[idx[b]]
	})
	out := make([]Candidate, len(list))
	for i, j := range idx {
		out[i] = list[j]
	}
	return out
}

// call 表示一次经过网关的调用，贯穿权限、限流、预留、上游尝试、结算与日志。
type call struct {
	s        *Server
	w        http.ResponseWriter
	r        *http.Request
	ctx      context.Context
	cancel   context.CancelFunc
	reqID    string
	key      *APIKey
	endpoint string
	kind     string
	model    string
	stream   bool
	start    time.Time
	mc       *ModelConfig
	cands    []Candidate
	release  func()
	reserved bool
	amount   int64
	attempts int
	used     *Candidate
	finished bool
}

type outcome struct {
	status       string // success | error | canceled | rejected
	httpStatus   int
	errCode      string
	errMsg       string
	pt, ct, tt   int64
	units        int64
	cost         int64
	costStatus   string
	usageSource  string // upstream | estimated | none
	firstTokenMs int64
	keepHold     bool   // 异步任务：预留保持到任务结束
	pendingNote  string // costStatus=pending 时的说明
}

// begin 完成调用前检查。返回 nil 表示已拒绝并写出响应。
func (s *Server) begin(w http.ResponseWriter, r *http.Request, endpoint, kind, model string, stream bool, types ...string) *call {
	ctx, cancel := context.WithCancel(r.Context())
	c := &call{s: s, w: w, r: r, ctx: ctx, cancel: cancel, reqID: reqIDFrom(r), key: keyFrom(r),
		endpoint: endpoint, kind: kind, model: model, stream: stream, start: time.Now()}
	if _, err := s.db.Exec(`INSERT INTO request_logs(request_id, key_id, key_name, source, endpoint, kind, model, stream, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,'in_progress',?)`, c.reqID, c.key.ID, c.key.Name, sourceFrom(r), endpoint, kind, model, boolInt(stream), c.start.UnixMilli()); err != nil {
		log.Printf("[%s] 写入请求日志失败: %v", c.reqID, err)
	}
	go s.db.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=?`, c.start.UnixMilli(), c.key.ID)

	if model == "" {
		c.reject(400, "model_required", "缺少 model 参数")
		return nil
	}
	if !c.key.allows(model) {
		c.reject(403, "model_not_allowed", fmt.Sprintf("该网关 Key 无权访问模型 %s", model))
		return nil
	}
	c.mc = s.modelConfig(model)
	if c.mc != nil && c.mc.Kind != kind {
		c.reject(400, "model_kind_mismatch", fmt.Sprintf("模型 %s 的类型为 %s，不能用于该接口", model, kindLabel(c.mc.Kind)))
		return nil
	}
	c.cands = s.candidates(model, types...)
	if len(c.cands) == 0 {
		c.reject(404, "model_not_found", fmt.Sprintf("模型 %s 不存在或当前没有可用渠道", model))
		return nil
	}
	if ok, remaining, retry := s.limiter.allowRPM(c.key.ID, c.key.RPMLimit); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		w.Header().Set("X-RateLimit-Limit-Requests", strconv.Itoa(c.key.RPMLimit))
		w.Header().Set("X-RateLimit-Remaining-Requests", "0")
		c.reject(429, "rate_limit_exceeded", fmt.Sprintf("请求过于频繁：该 Key 每分钟最多 %d 次请求，请约 %d 秒后重试", c.key.RPMLimit, retry))
		return nil
	} else if c.key.RPMLimit > 0 {
		w.Header().Set("X-RateLimit-Limit-Requests", strconv.Itoa(c.key.RPMLimit))
		w.Header().Set("X-RateLimit-Remaining-Requests", strconv.Itoa(remaining))
	}
	release, ok := s.limiter.acquire(c.key.ID, c.key.MaxConcurrency)
	if !ok {
		w.Header().Set("Retry-After", "1")
		c.reject(429, "concurrency_limit_exceeded", fmt.Sprintf("该 Key 的在途请求数已达上限 %d，请稍后重试", c.key.MaxConcurrency))
		return nil
	}
	c.release = release
	return c
}

func kindLabel(k string) string {
	switch k {
	case "chat":
		return "聊天"
	case "embedding":
		return "向量"
	case "video":
		return "视频生成"
	case "tts":
		return "语音合成"
	case "music":
		return "音乐生成"
	case "voice":
		return "音色"
	}
	return k
}

// reject 拒绝请求：未调用上游，不产生费用。
func (c *call) reject(status int, code, msg string) {
	writeGatewayError(c.w, status, code, msg)
	c.finish(outcome{status: "rejected", httpStatus: status, errCode: code, errMsg: msg, costStatus: costNone, usageSource: "none"})
}

// fail 在上游调用后失败时使用：写出错误并按 outcome 结算。
func (c *call) fail(status int, code, msg string, o outcome) {
	writeGatewayError(c.w, status, code, msg)
	o.httpStatus, o.errCode, o.errMsg = status, code, msg
	if o.status == "" {
		o.status = "error"
	}
	c.finish(o)
}

// reserveFor 按费用上界预留额度。priced=false 表示无法得到可信上界。
func (c *call) reserveFor(upper int64, kind string) bool {
	priced := c.mc != nil && c.mc.Priced
	if !priced {
		if c.key.QuotaLimit >= 0 {
			c.reject(403, "price_not_configured", fmt.Sprintf("模型 %s 尚未配置价格，无法计算费用上界；有限额度的 Key 不能调用，请先在“模型”页配置并确认价格", c.billingModel()))
			return false
		}
		upper = 0
	}
	avail, err := c.s.reserve(c.key, c.reqID, upper, kind)
	if errors.Is(err, errInsufficientQuota) {
		c.reject(402, "insufficient_quota", fmt.Sprintf("额度不足：本次请求的费用上界为 ¥%s，该 Key 当前可用额度为 ¥%s（已扣除在途请求占用）", yuan(upper), yuan(avail)))
		return false
	}
	if err != nil {
		log.Printf("[%s] 预留额度失败: %v", c.reqID, err)
		c.reject(500, "internal_error", "预留额度失败，请稍后重试")
		return false
	}
	c.reserved, c.amount = true, upper
	return true
}

func (c *call) billingModel() string {
	if c.mc != nil {
		return c.mc.Name
	}
	return c.model
}

func yuan(micro int64) string {
	return strconv.FormatFloat(float64(micro)/1e6, 'f', 6, 64)
}

func (c *call) recordAttempt(ch *Candidate, status string, httpStatus int, pt, ct, cost int64, costStatus, errMsg string, started time.Time) {
	c.s.db.Exec(`INSERT INTO upstream_attempts(request_id, attempt, channel_id, channel_name, upstream_model, status, http_status, prompt_tokens, completion_tokens, cost, cost_status, error, duration_ms, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.reqID, c.attempts, ch.ChannelID, ch.ChannelName, ch.UpstreamModel, status, httpStatus, pt, ct, cost, costStatus,
		truncate(errMsg, 500), time.Since(started).Milliseconds(), started.UnixMilli())
}

// finish 结算并写日志。只生效一次：重复的完成通知不会重复记账。
func (c *call) finish(o outcome) {
	if c.finished {
		return
	}
	c.finished = true
	if c.release != nil {
		c.release()
	}
	c.cancel()
	if o.costStatus == "" {
		o.costStatus = costNone
	}
	if o.usageSource == "" {
		o.usageSource = "none"
	}
	if o.tt == 0 {
		o.tt = o.pt + o.ct
	}
	if c.reserved && !(c.mc != nil && c.mc.Priced) && o.costStatus != costPending && o.costStatus != costNone {
		o.cost, o.costStatus = 0, costUnpriced
	}
	var chID int64
	var chName, upModel string
	if c.used != nil {
		chID, chName, upModel = c.used.ChannelID, c.used.ChannelName, c.used.UpstreamModel
	}
	now := time.Now()
	err := func() error {
		tx, err := c.s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if c.reserved && !o.keepHold {
			if o.costStatus == costPending {
				if _, err := pendingTx(tx, c.reqID, o.pendingNote); err != nil {
					return err
				}
				o.cost = 0
			} else if _, err := settleTx(tx, c.reqID, o.cost, o.costStatus, ""); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE request_logs SET status=?, http_status=?, error_code=?, error_message=?, channel_id=?, channel_name=?, upstream_model=?,
			prompt_tokens=?, completion_tokens=?, total_tokens=?, units=?, cost=?, cost_status=?, usage_source=?, attempts=?, duration_ms=?, first_token_ms=?, finished_at=?
			WHERE request_id=?`, o.status, o.httpStatus, o.errCode, truncate(o.errMsg, 1000), chID, chName, upModel,
			o.pt, o.ct, o.tt, o.units, o.cost, o.costStatus, o.usageSource, c.attempts, now.Sub(c.start).Milliseconds(), o.firstTokenMs, now.UnixMilli(), c.reqID); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		// 结算失败时预留仍为 held，重启恢复路径会把它转为待核实，不会丢账也不会放行。
		log.Printf("[%s] 结算失败（预留保持，待恢复路径处理）: %v", c.reqID, err)
	}
}

// abandon 作为 defer 兜底：处理函数异常退出时把在途预留转为待核实。
func (c *call) abandon() {
	if !c.finished {
		c.finish(outcome{status: "error", httpStatus: 500, errCode: "internal_error", errMsg: "网关内部错误", costStatus: costPending, pendingNote: "网关内部错误，费用待核实"})
	}
}

type errClass int

const (
	errNoUsage errClass = iota // 请求确认未到达上游（连接失败等）
	errCanceled                // 调用方取消
	errUnknown                 // 请求可能已到达上游，结果未知
)

func classifyErr(err error, clientCtx context.Context) errClass {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &dnsErr) || errors.As(err, &certErr) || (errors.As(err, &opErr) && opErr.Op == "dial") {
		return errNoUsage
	}
	if clientCtx.Err() != nil {
		return errCanceled
	}
	return errUnknown
}

func upstreamErrMessage(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if e, ok := m["error"].(map[string]any); ok {
			if s := str(e["message"]); s != "" {
				return s
			}
		}
		if s := str(m["error"]); s != "" {
			return s
		}
		if s := str(m["message"]); s != "" {
			return s
		}
		if s := str(m["msg"]); s != "" {
			return s
		}
		if b, ok := m["base_resp"].(map[string]any); ok {
			if s := str(b["status_msg"]); s != "" {
				return s
			}
		}
	}
	return truncate(strings.TrimSpace(string(body)), 300)
}

func retryableStatus(code int) bool {
	return code == 401 || code == 403 || code == 404 || code == 408 || code == 429 || code >= 500
}

// tryChannels 依次尝试候选渠道，直到拿到 2xx 响应。
// 只有“确认未产生用量”的失败（连接失败、上游明确返回错误状态）才会切换渠道，
// 因此一次网关请求最多只有一次可能计费的上游调用，不会因重试产生重复费用；
// 结果未知的失败（已发出请求后超时/断连）不重试，直接转待核实。
// 失败时已写出错误并完成结算，返回 nil。
func (c *call) tryChannels(build func(ch *Candidate) (*http.Request, error)) (*http.Response, time.Time) {
	var lastStatus int
	var lastMsg string
	limit := min(len(c.cands), c.s.cfg.MaxAttempts)
	for i := 0; i < limit; i++ {
		ch := &c.cands[i]
		req, err := build(ch)
		if err != nil {
			c.fail(500, "internal_error", "构造上游请求失败: "+err.Error(), outcome{costStatus: costNone})
			return nil, time.Time{}
		}
		c.attempts++
		c.used = ch
		if c.reserved {
			c.s.markDispatched(c.reqID)
		}
		t0 := time.Now()
		resp, err := c.s.httpc.Do(req)
		if err != nil {
			switch classifyErr(err, c.r.Context()) {
			case errNoUsage:
				c.recordAttempt(ch, "failed", 0, 0, 0, 0, costNone, "连接上游失败: "+err.Error(), t0)
				lastStatus, lastMsg = 502, "连接上游失败"
				continue
			case errCanceled:
				c.recordAttempt(ch, "canceled", 0, 0, 0, 0, costPending, "调用方取消，上游结果未知", t0)
				c.fail(499, "client_canceled", "调用方已取消请求", outcome{status: "canceled", costStatus: costPending,
					pendingNote: "调用方在上游返回前取消，上游可能已产生用量，费用待核实"})
				return nil, time.Time{}
			default:
				c.recordAttempt(ch, "unknown", 0, 0, 0, 0, costPending, "上游超时或连接中断: "+err.Error(), t0)
				c.fail(504, "upstream_timeout", "上游超时或连接中断，本次请求费用待核实", outcome{costStatus: costPending,
					pendingNote: "请求已发往上游但超时/断连，没有用量信息，费用待核实"})
				return nil, time.Time{}
			}
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			msg := upstreamErrMessage(body)
			c.recordAttempt(ch, "failed", resp.StatusCode, 0, 0, 0, costNone, msg, t0)
			lastStatus, lastMsg = resp.StatusCode, msg
			if retryableStatus(resp.StatusCode) {
				continue
			}
			break
		}
		return resp, t0
	}
	status, code, msg := 502, "upstream_error", "上游服务失败"
	switch {
	case lastStatus == 400 || lastStatus == 413 || lastStatus == 422:
		status, code, msg = lastStatus, "upstream_bad_request", "上游拒绝了请求"
	case lastStatus == 429:
		status, code, msg = 429, "upstream_rate_limited", "上游限流，请稍后重试"
	case lastStatus == 401 || lastStatus == 403:
		msg = "上游渠道鉴权失败或无权限，请联系管理员检查渠道配置"
	}
	if lastMsg != "" {
		msg += "：" + truncate(lastMsg, 300)
	}
	// 所有尝试都确认未产生用量：不扣费，释放预留。
	c.fail(status, code, msg, outcome{costStatus: costNone})
	return nil, time.Time{}
}

// handleListModels 返回该 Key 可用的模型（OpenAI 兼容），并附带类型与参数范围，供 Playground 约束参数。
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	kind := r.URL.Query().Get("kind")
	rows, err := s.db.Query(`SELECT cm.model, GROUP_CONCAT(DISTINCT c.type) FROM channel_models cm JOIN channels c ON c.id=cm.channel_id WHERE c.status=1 GROUP BY cm.model ORDER BY cm.model`)
	if err != nil {
		writeGatewayError(w, 500, "internal_error", "读取模型失败")
		return
	}
	var names []string
	providers := map[string][]string{}
	for rows.Next() {
		var name, types string
		rows.Scan(&name, &types)
		names = append(names, name)
		providers[name] = strings.Split(types, ",")
	}
	rows.Close()
	data := []map[string]any{}
	for _, name := range names {
		if !key.allows(name) {
			continue
		}
		mc := s.modelConfig(name)
		if mc == nil {
			k := guessKind(name)
			mc = &ModelConfig{Name: name, Kind: k, BillingMode: defaultBilling(k), SupportsTemperature: true, TemperatureMax: 2}
		}
		if kind != "" && kind != mc.Kind {
			continue
		}
		data = append(data, map[string]any{"id": name, "object": "model", "owned_by": "model-relay", "kind": mc.Kind, "priced": mc.Priced, "providers": providers[name],
			"supports_temperature": mc.SupportsTemperature, "temperature_max": mc.TemperatureMax,
			"max_output_tokens": mc.MaxOutputTokens, "default_max_tokens": mc.DefaultMaxTokens})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

var _ = sql.ErrNoRows
