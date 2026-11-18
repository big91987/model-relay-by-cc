package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modelrelay/internal/mockupstream"
)

// 本文件的全部用例都使用受控假上游（mockupstream），用于验证确定性的行为与账务边界，
// 不代表真实上游验收。价格：mock-chat 输入 1 元/百万 Token、输出 2 元/百万 Token，
// 因此 1 个输入 Token = 1 微元，1 个输出 Token = 2 微元。

type env struct {
	t      *testing.T
	mock   *mockupstream.Mock
	up     *httptest.Server
	srv    *Server
	gw     *httptest.Server
	dbPath string
	cfg    Config
	cookie *http.Cookie
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, mock: mockupstream.New()}
	e.up = httptest.NewServer(e.mock)
	e.dbPath = filepath.Join(t.TempDir(), "test.sqlite")
	e.cfg = Config{SQLitePath: e.dbPath, UpstreamTimeout: 800 * time.Millisecond, StreamIdleTimeout: 2 * time.Second, TaskPollInterval: 50 * time.Millisecond}
	e.start()
	t.Cleanup(func() {
		e.gw.Close()
		e.srv.Close()
		e.up.Close()
	})
	e.addChannel("主渠道", "openai", e.up.URL, 10, "mock-chat", "mock-embedding")
	e.setModel(ModelConfig{Name: "mock-chat", Kind: "chat", BillingMode: "token", PriceIn: 1_000_000, PriceOut: 2_000_000, Priced: true, SupportsTemperature: true, TemperatureMax: 2})
	e.setModel(ModelConfig{Name: "mock-embedding", Kind: "embedding", BillingMode: "token", PriceIn: 1_000_000, Priced: true})
	return e
}

func (e *env) start() {
	srv, err := New(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv = srv
	e.gw = httptest.NewServer(srv.Handler())
	e.cookie = nil
}

func (e *env) login() {
	if e.cookie != nil {
		return
	}
	resp, err := http.Post(e.gw.URL+"/api/login", "application/json", strings.NewReader(`{"username":"admin","password":"admin"}`))
	if err != nil || resp.StatusCode != 200 {
		e.t.Fatalf("登录失败: %v", err)
	}
	e.cookie = resp.Cookies()[0]
	resp.Body.Close()
}

// admin 调用管理接口。
func (e *env) admin(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	e.login()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.gw.URL+path, rd)
	req.AddCookie(e.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) addChannel(name, typ, base string, priority int, models ...string) int64 {
	c := &Channel{Name: name, Type: typ, BaseURL: base, APIKey: "upstream-secret", Status: 1, Priority: priority}
	for _, m := range models {
		c.Models = append(c.Models, ChannelModel{Model: m, UpstreamModel: m})
	}
	if err := e.srv.saveChannel(c); err != nil {
		e.t.Fatal(err)
	}
	return c.ID
}

func (e *env) setModel(m ModelConfig) {
	if code, out := e.admin("PUT", "/api/models", m); code != 200 {
		e.t.Fatalf("保存模型失败: %v", out)
	}
}

// newKey 创建网关 Key。quota<0 表示无限额度。
func (e *env) newKey(name string, quota int64, rpm, conc int, models ...string) (string, int64) {
	code, out := e.admin("POST", "/api/keys", map[string]any{"name": name, "quota_limit": quota, "rpm_limit": rpm, "max_concurrency": conc, "models": models})
	if code != 200 {
		e.t.Fatalf("创建 Key 失败: %v", out)
	}
	return out["key"].(string), int64(out["id"].(float64))
}

type result struct {
	status int
	body   map[string]any
	raw    string
	reqID  string
	header http.Header
}

func (r result) errCode() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		return fmt.Sprint(e["code"])
	}
	return ""
}

func (e *env) call(ctx context.Context, key, path string, body any) result {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", e.gw.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return result{status: -1, raw: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := result{status: resp.StatusCode, raw: string(raw), reqID: resp.Header.Get("X-Request-Id"), header: resp.Header}
	json.Unmarshal(raw, &r.body)
	return r
}

func chatBody(mock map[string]any, extra ...any) map[string]any {
	b := map[string]any{"model": "mock-chat", "max_tokens": 100, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	if mock != nil {
		b["mock"] = mock
	}
	for i := 0; i+1 < len(extra); i += 2 {
		b[extra[i].(string)] = extra[i+1]
	}
	return b
}

func (e *env) keyState(id int64) (used, reserved int64) {
	e.srv.db.QueryRow(`SELECT used_cost, reserved_cost FROM api_keys WHERE id=?`, id).Scan(&used, &reserved)
	return
}

func (e *env) logOf(reqID string) *RequestLog {
	e.t.Helper()
	var l *RequestLog
	var err error
	for i := 0; i < 100; i++ { // 取消类请求的结算是异步完成的，稍等片刻
		l, err = scanLog(e.srv.db.QueryRow(`SELECT `+logColumns+` FROM request_logs WHERE request_id=?`, reqID))
		if err == nil && l.Status != "in_progress" {
			return l
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		e.t.Fatalf("找不到请求日志 %s: %v", reqID, err)
	}
	return l
}

func expect[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：得到 %v，期望 %v", what, got, want)
	}
}

// ———————————————————— 01 网关首版 / 02 向量 ————————————————————

func TestChatAndEmbeddingsBasic(t *testing.T) {
	e := newEnv(t)
	key, id := e.newKey("基础", -1, 0, 0)

	r := e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"prompt_tokens": 100, "completion_tokens": 50}))
	expect(t, "聊天状态码", r.status, 200)
	l := e.logOf(r.reqID)
	expect(t, "日志状态", l.Status, "success")
	expect(t, "输入 Token", l.PromptTokens, int64(100))
	expect(t, "输出 Token", l.CompletionTokens, int64(50))
	expect(t, "费用", l.Cost, int64(100*1+50*2))
	expect(t, "费用状态", l.CostStatus, costActual)

	r = e.call(context.Background(), key, "/v1/embeddings", map[string]any{"model": "mock-embedding", "input": []string{"你好", "世界"}})
	expect(t, "向量状态码", r.status, 200)
	if data, _ := r.body["data"].([]any); len(data) != 2 {
		t.Errorf("应返回 2 条向量，实际 %d", len(data))
	}
	l = e.logOf(r.reqID)
	expect(t, "向量用量", l.PromptTokens, int64(10))
	expect(t, "向量费用", l.Cost, int64(10))
	expect(t, "向量接口", l.Endpoint, "/v1/embeddings")

	used, reserved := e.keyState(id)
	expect(t, "累计已用", used, int64(210))
	expect(t, "剩余预留", reserved, int64(0))

	// 流式：逐块转发并取得上游用量
	r = e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"prompt_tokens": 30, "completion_tokens": 20}, "stream", true))
	if !strings.Contains(r.raw, "data: [DONE]") || strings.Count(r.raw, "chat.completion.chunk") < 4 {
		t.Errorf("流式响应不完整: %s", r.raw)
	}
	if strings.Contains(r.raw, `"usage"`) {
		t.Errorf("调用方未要求用量时不应转发用量块")
	}
	l = e.logOf(r.reqID)
	expect(t, "流式费用", l.Cost, int64(30+40))
	expect(t, "流式费用状态", l.CostStatus, costActual)

	// 上游不返回用量：标记为估算
	r = e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"no_usage": true}))
	l = e.logOf(r.reqID)
	expect(t, "无用量时的费用状态", l.CostStatus, costEstimated)
	expect(t, "无用量时的用量来源", r.header.Get("X-Usage-Source"), "estimated")
}

func TestAuthAndPermission(t *testing.T) {
	e := newEnv(t)
	key, _ := e.newKey("受限", -1, 0, 0, "mock-embedding")
	r := e.call(context.Background(), "sk-relay-wrong", "/v1/chat/completions", chatBody(nil))
	expect(t, "无效 Key", r.status, 401)
	expect(t, "无效 Key 错误码", r.errCode(), "invalid_api_key")
	r = e.call(context.Background(), key, "/v1/chat/completions", chatBody(nil))
	expect(t, "无权模型", r.status, 403)
	expect(t, "无权模型错误码", r.errCode(), "model_not_allowed")
	r = e.call(context.Background(), key, "/v1/embeddings", map[string]any{"model": "mock-embedding", "input": "ok"})
	expect(t, "有权模型", r.status, 200)
	expect(t, "未调用上游的拒绝不应到达上游", e.mock.Calls.Load(), int64(1))
}

// ———————————————————— 03 按 Key 限流 / 17 在途请求数 ————————————————————

func TestRPMLimit(t *testing.T) {
	e := newEnv(t)
	key, _ := e.newKey("限流", -1, 3, 0)
	other, _ := e.newKey("不限流", -1, 0, 0)
	for i := 0; i < 3; i++ {
		expect(t, "限额内请求", e.call(context.Background(), key, "/v1/chat/completions", chatBody(nil)).status, 200)
	}
	r := e.call(context.Background(), key, "/v1/chat/completions", chatBody(nil))
	expect(t, "超限状态码", r.status, 429)
	expect(t, "超限错误码", r.errCode(), "rate_limit_exceeded")
	if r.header.Get("Retry-After") == "" {
		t.Error("超限响应应带 Retry-After")
	}
	expect(t, "其他 Key 不受影响", e.call(context.Background(), other, "/v1/chat/completions", chatBody(nil)).status, 200)
	expect(t, "被限流的请求费用为 0", e.logOf(r.reqID).Cost, int64(0))
}

func TestConcurrencyLimit(t *testing.T) {
	e := newEnv(t)
	key, _ := e.newKey("并发", -1, 0, 2)
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"delay_ms": 400})).status
		}(i)
	}
	time.Sleep(150 * time.Millisecond)
	r := e.call(context.Background(), key, "/v1/chat/completions", chatBody(nil))
	expect(t, "超过在途上限", r.status, 429)
	expect(t, "错误码", r.errCode(), "concurrency_limit_exceeded")
	wg.Wait()
	expect(t, "在途请求 1", statuses[0], 200)
	expect(t, "在途请求 2", statuses[1], 200)
	expect(t, "结束后名额已释放", e.call(context.Background(), key, "/v1/chat/completions", chatBody(nil)).status, 200)
	expect(t, "在途计数归零", e.srv.limiter.inflightOf(1), 0)
}

// ———————————————————— 08 并发额度与记账 ————————————————————

// upperOf 用无限额度 Key 探测某个请求体的费用上界（即预留额）。
func (e *env) upperOf(body map[string]any) int64 {
	key, _ := e.newKey("探测", -1, 0, 0)
	r := e.call(context.Background(), key, "/v1/chat/completions", body)
	return e.logOf(r.reqID).Reserved
}

// 验收 1：额度只够一个有明确上界的请求，同时发起五个，同一笔余额不得被重复放行。
func TestQuotaNotDoubleSpentUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	body := chatBody(map[string]any{"delay_ms": 300, "prompt_tokens": 20, "completion_tokens": 30})
	upper := e.upperOf(body)
	if upper <= 0 {
		t.Fatal("费用上界应大于 0")
	}
	before := e.mock.Calls.Load()
	key, id := e.newKey("有限", upper+upper/2, 0, 0) // 够 1 个，不够 2 个
	var wg sync.WaitGroup
	results := make([]result, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = e.call(context.Background(), key, "/v1/chat/completions", body)
		}(i)
	}
	wg.Wait()
	okN, rejected := 0, 0
	for _, r := range results {
		switch {
		case r.status == 200:
			okN++
		case r.status == 402 && r.errCode() == "insufficient_quota":
			rejected++
		default:
			t.Errorf("意外的响应: %d %s", r.status, r.raw)
		}
	}
	expect(t, "放行数", okN, 1)
	expect(t, "拒绝数", rejected, 4)
	expect(t, "到达上游的调用数", e.mock.Calls.Load()-before, int64(1))
	used, reserved := e.keyState(id)
	expect(t, "已用 = 实际费用", used, int64(20+60))
	expect(t, "预留已全部释放", reserved, int64(0))
}

// 验收 2：多次允许的并发调用，最终费用等于各请求应计费用之和。
func TestConcurrentSettlementSum(t *testing.T) {
	e := newEnv(t)
	key, id := e.newKey("并发记账", 10_000_000, 0, 0)
	var wg sync.WaitGroup
	var want int64
	for i := 1; i <= 20; i++ {
		pt, ct := int64(10*i), int64(3*i)
		want += pt + 2*ct
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := chatBody(map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "delay_ms": 50}, "stream", i%2 == 0)
			if r := e.call(context.Background(), key, "/v1/chat/completions", body); r.status != 200 {
				t.Errorf("请求失败: %d %s", r.status, r.raw)
			}
		}(i)
	}
	wg.Wait()
	used, reserved := e.keyState(id)
	expect(t, "费用合计", used, want)
	expect(t, "剩余预留", reserved, int64(0))
	var logSum int64
	e.srv.db.QueryRow(`SELECT SUM(cost) FROM request_logs WHERE key_id=?`, id).Scan(&logSum)
	expect(t, "日志费用合计与 Key 一致", logSum, want)
}

// 验收 3：成功、调用前失败、超时、取消、断流、渠道切换，分别核对实际费用和剩余预留。
func TestSettlementScenarios(t *testing.T) {
	e := newEnv(t)
	key, id := e.newKey("场景", 50_000_000, 0, 0)
	check := func(name string, wantUsedDelta int64, wantReservedPositive bool, f func() string) *RequestLog {
		t.Helper()
		u0, r0 := e.keyState(id)
		reqID := f()
		l := e.logOf(reqID)
		u1, r1 := e.keyState(id)
		if wantUsedDelta >= 0 && u1-u0 != wantUsedDelta { // 负数表示只要求大于 0，由调用处另行断言
			t.Errorf("%s：已用变化 %d，期望 %d", name, u1-u0, wantUsedDelta)
		}
		if (r1-r0 > 0) != wantReservedPositive {
			t.Errorf("%s：预留变化 %d，是否应保留预留=%v", name, r1-r0, wantReservedPositive)
		}
		return l
	}

	l := check("上游返回 500（确认未产生用量）", 0, false, func() string {
		return e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"status": 500})).reqID
	})
	expect(t, "500 的费用状态", l.CostStatus, costNone)
	expect(t, "500 的日志状态", l.Status, "error")

	l = check("调用前失败（无效参数，未调用上游）", 0, false, func() string {
		r := e.call(context.Background(), key, "/v1/chat/completions", map[string]any{"model": "mock-chat", "messages": []any{}})
		expect(t, "空消息", r.status, 400)
		return r.reqID
	})
	expect(t, "调用前失败的日志状态", l.Status, "rejected")

	l = check("上游超时（费用未知）", 0, true, func() string {
		r := e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"hang": true}))
		expect(t, "超时状态码", r.status, 504)
		return r.reqID
	})
	expect(t, "超时的费用状态", l.CostStatus, costPending)

	// 取消：流式生成中途断开，按已收到内容估算
	l = check("流式中途取消（估算）", -1, false, func() string {
		ctx, cancel := context.WithCancel(context.Background())
		b, _ := json.Marshal(chatBody(map[string]any{"chunks": 20, "chunk_delay_ms": 60, "content": strings.Repeat("内容", 100)}, "stream", true))
		req, _ := http.NewRequestWithContext(ctx, "POST", e.gw.URL+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		resp.Body.Read(buf)
		cancel()
		resp.Body.Close()
		return resp.Header.Get("X-Request-Id")
	})
	expect(t, "取消的日志状态", l.Status, "canceled")
	expect(t, "取消的费用状态", l.CostStatus, costEstimated)
	if l.Cost <= 0 {
		t.Error("取消后已产生的用量不能视为免费")
	}

	l = check("上游断流（估算）", -1, false, func() string {
		return e.call(context.Background(), key, "/v1/chat/completions",
			chatBody(map[string]any{"chunks": 6, "drop_after": 3, "content": strings.Repeat("断流", 60)}, "stream", true)).reqID
	})
	expect(t, "断流的日志状态", l.Status, "error")
	expect(t, "断流的费用状态", l.CostStatus, costEstimated)
	if l.Cost <= 0 {
		t.Error("断流前已产生的用量不能视为免费")
	}

	// 渠道切换：高优先级渠道不可达，切到主渠道；只记一次费用
	dead := e.addChannel("不可达渠道", "openai", "http://127.0.0.1:1", 100, "mock-chat")
	l = check("渠道切换", 40+2*10, false, func() string {
		r := e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"prompt_tokens": 40, "completion_tokens": 10}))
		expect(t, "切换后成功", r.status, 200)
		return r.reqID
	})
	expect(t, "尝试次数", l.Attempts, 2)
	expect(t, "最终渠道", l.ChannelName, "主渠道")
	var n, attemptCost int64
	e.srv.db.QueryRow(`SELECT COUNT(*), SUM(cost) FROM upstream_attempts WHERE request_id=?`, l.RequestID).Scan(&n, &attemptCost)
	expect(t, "上游尝试记录数", n, int64(2))
	expect(t, "各次尝试费用之和 = 请求费用", attemptCost, l.Cost)
	e.srv.db.Exec(`UPDATE channels SET status=0 WHERE id=?`, dead)

	// 重复的完成通知不会重复记账
	u0, _ := e.keyState(id)
	tx, _ := e.srv.db.Begin()
	applied, _ := settleTx(tx, l.RequestID, 9999, costActual, "重复通知")
	tx.Commit()
	u1, _ := e.keyState(id)
	if applied || u0 != u1 {
		t.Error("重复结算不应生效")
	}

	// 待核实预留：管理员核实后结算
	code, out := e.admin("GET", "/api/reservations", nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("应有 1 笔待核实预留，实际 %v", out)
	}
	resv := items[0].(map[string]any)
	u0, r0 := e.keyState(id)
	code, _ = e.admin("POST", fmt.Sprintf("/api/reservations/%.0f/resolve", resv["id"]), map[string]any{"action": "settle", "amount": 7})
	expect(t, "核实接口", code, 200)
	u1, r1 := e.keyState(id)
	expect(t, "核实后已用增加", u1-u0, int64(7))
	expect(t, "核实后预留释放", r0-r1, int64(resv["amount"].(float64)))
	expect(t, "全部预留归零", r1, int64(0))
}

// 验收 4：有在途请求时重启，通过正式恢复路径核对账务。
func TestRestartRecovery(t *testing.T) {
	e := newEnv(t)
	key, id := e.newKey("重启", 50_000_000, 0, 0)
	r := e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"prompt_tokens": 100, "completion_tokens": 100}))
	settled, _ := e.keyState(id)
	expect(t, "重启前已结算", settled, int64(300))
	_ = r

	// 一个已发往上游、尚未返回的在途请求
	e.cfg.UpstreamTimeout = 5 * time.Second
	e.gw.Close()
	e.srv.Close()
	e.start()
	done := make(chan result, 1)
	go func() {
		done <- e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"hang": true}))
	}()
	// 另造一笔“已预留但尚未发往上游”的在途记录
	time.Sleep(300 * time.Millisecond)
	k, _ := e.srv.keyByID(id)
	e.srv.db.Exec(`INSERT INTO request_logs(request_id, key_id, key_name, endpoint, kind, model, status, created_at) VALUES('req_undispatched', ?, '重启', '/v1/chat/completions', 'chat', 'mock-chat', 'in_progress', ?)`, id, nowMs())
	if _, err := e.srv.reserve(k, "req_undispatched", 500, "sync"); err != nil {
		t.Fatal(err)
	}
	_, reservedBefore := e.keyState(id)
	if reservedBefore <= 500 {
		t.Fatalf("应有两笔在途预留，当前预留 %d", reservedBefore)
	}

	// 模拟异常退出后重启：旧进程不做任何收尾，新实例打开同一个数据库文件
	old := e.srv
	oldGW := e.gw
	e.start()
	used, reserved := e.keyState(id)
	expect(t, "已结算费用不丢失", used, int64(300))
	expect(t, "未发往上游的预留已释放、已发往上游的保留", reserved, reservedBefore-500)
	l := e.logOf("req_undispatched")
	expect(t, "未发往上游的请求", l.Status, "interrupted")
	expect(t, "未发往上游的费用状态", l.CostStatus, costNone)

	_, out := e.admin("GET", "/api/reservations", nil)
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("应有 1 笔待核实预留: %v", out)
	}
	resv := items[0].(map[string]any)
	expect(t, "待核实预留已发往上游", resv["dispatched"].(bool), true)

	// 额度仍被占用：不能清零放行
	tight, tid := e.newKey("紧", 0, 0, 0)
	_ = tid
	expect(t, "零额度 Key 被拒绝", e.call(context.Background(), tight, "/v1/chat/completions", chatBody(nil)).status, 402)

	// 旧进程里那个请求稍后才结束：它的完成通知不得再次改动账务
	oldGW.CloseClientConnections()
	<-done
	oldGW.Close()
	old.Close()
	used2, reserved2 := e.keyState(id)
	expect(t, "旧进程的迟到通知不改变已用", used2, used)
	expect(t, "旧进程的迟到通知不改变预留", reserved2, reserved)

	// 正式核实路径：管理员确认未产生费用并释放
	code, _ := e.admin("POST", fmt.Sprintf("/api/reservations/%.0f/resolve", resv["id"]), map[string]any{"action": "release"})
	expect(t, "释放接口", code, 200)
	used3, reserved3 := e.keyState(id)
	expect(t, "释放后已用不变", used3, int64(300))
	expect(t, "释放后预留归零", reserved3, int64(0))
}

func TestPendingAutoSettle(t *testing.T) {
	e := newEnv(t)
	key, id := e.newKey("超期", 50_000_000, 0, 0)
	r := e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"hang": true}))
	l := e.logOf(r.reqID)
	_, reserved := e.keyState(id)
	expect(t, "待核实占用额度", reserved, l.Reserved)
	e.srv.cfg.PendingReviewTTL = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	n, err := e.srv.autoSettleExpiredPending()
	if err != nil || n != 1 {
		t.Fatalf("自动结算: n=%d err=%v", n, err)
	}
	used, reserved := e.keyState(id)
	expect(t, "超期按上界结算", used, l.Reserved)
	expect(t, "预留归零", reserved, int64(0))
	expect(t, "日志费用状态", e.logOf(r.reqID).CostStatus, costEstimated)
}

// 验收 5：不同 Key 余额独立；无限额度 Key 统计用量但不因余额被拒；未定价模型的处理。
func TestKeysIndependentAndUnlimited(t *testing.T) {
	e := newEnv(t)
	body := chatBody(map[string]any{"prompt_tokens": 10, "completion_tokens": 10})
	upper := e.upperOf(body)
	a, aid := e.newKey("A", upper, 0, 0)
	b, bid := e.newKey("B", upper, 0, 0)
	unlimited, uid := e.newKey("无限", -1, 0, 0)
	expect(t, "A 首次", e.call(context.Background(), a, "/v1/chat/completions", body).status, 200)
	expect(t, "A 额度用尽", e.call(context.Background(), a, "/v1/chat/completions", body).status, 402)
	expect(t, "B 不受 A 影响", e.call(context.Background(), b, "/v1/chat/completions", body).status, 200)
	for i := 0; i < 5; i++ {
		expect(t, "无限额度", e.call(context.Background(), unlimited, "/v1/chat/completions", body).status, 200)
	}
	ua, _ := e.keyState(aid)
	ub, _ := e.keyState(bid)
	uu, _ := e.keyState(uid)
	expect(t, "A 已用", ua, int64(30))
	expect(t, "B 已用", ub, int64(30))
	expect(t, "无限额度仍统计用量", uu, int64(150))

	// 未定价模型：有限额度 Key 明确拒绝，无限额度 Key 可调用且标记未定价
	e.setModel(ModelConfig{Name: "mock-chat", Kind: "chat", BillingMode: "token", Priced: false, SupportsTemperature: true})
	r := e.call(context.Background(), b, "/v1/chat/completions", body)
	expect(t, "未定价 + 有限额度", r.status, 403)
	expect(t, "未定价错误码", r.errCode(), "price_not_configured")
	r = e.call(context.Background(), unlimited, "/v1/chat/completions", body)
	expect(t, "未定价 + 无限额度", r.status, 200)
	expect(t, "未定价费用状态", e.logOf(r.reqID).CostStatus, costUnpriced)
}

func TestMaxTokensInjectedForLimitedKey(t *testing.T) {
	e := newEnv(t)
	key, _ := e.newKey("有限", 1_000_000_000, 0, 0)
	body := chatBody(nil)
	delete(body, "max_tokens")
	r := e.call(context.Background(), key, "/v1/chat/completions", body)
	expect(t, "状态码", r.status, 200)
	l := e.logOf(r.reqID)
	if l.Reserved < 4096*2 {
		t.Errorf("未指定 max_tokens 时应按受控上限 4096 预留，实际预留 %d", l.Reserved)
	}
}

// ———————————————————— 07 渠道模型导入 / 10 收藏 ————————————————————

func TestFetchModelsAndFavorites(t *testing.T) {
	e := newEnv(t)
	code, out := e.admin("POST", "/api/channels/fetch-models", map[string]any{"channel_id": 1, "type": "openai", "base_url": e.up.URL})
	expect(t, "获取模型", code, 200)
	if list, _ := out["models"].([]any); len(list) != 3 {
		t.Errorf("应返回 3 个上游模型: %v", out)
	}
	// 勾选导入 + 别名 + 手动填写
	code, _ = e.admin("PUT", "/api/channels/1", map[string]any{"name": "主渠道", "type": "openai", "base_url": e.up.URL, "status": 1, "priority": 10,
		"models": []map[string]string{{"model": "mock-chat", "upstream_model": "mock-chat"}, {"model": "我的别名", "upstream_model": "mock-chat-pro"}, {"model": "mock-embedding", "upstream_model": "mock-embedding"}}})
	expect(t, "保存渠道", code, 200)
	key, _ := e.newKey("别名", -1, 0, 0)
	r := e.call(context.Background(), key, "/v1/chat/completions", map[string]any{"model": "我的别名", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	expect(t, "通过别名调用", r.status, 200)
	expect(t, "响应中的模型名为别名", fmt.Sprint(r.body["model"]), "我的别名")
	expect(t, "日志记录上游模型", e.logOf(r.reqID).UpstreamModel, "mock-chat-pro")

	// 获取失败：返回错误，已有配置保持不变
	e.mock.FailModel.Store(true)
	code, out = e.admin("POST", "/api/channels/fetch-models", map[string]any{"channel_id": 1, "type": "openai", "base_url": e.up.URL})
	expect(t, "获取失败状态码", code, 502)
	_, out = e.admin("GET", "/api/channels", nil)
	ch := out["items"].([]any)[0].(map[string]any)
	if n := len(ch["models"].([]any)); n != 3 {
		t.Errorf("获取失败后渠道模型应保持 3 个，实际 %d", n)
	}
	if _, leaked := ch["api_key"]; leaked {
		t.Error("渠道密钥不应下发")
	}

	// 收藏 / 取消收藏 / 仅看收藏；重启后保留
	e.admin("PUT", "/api/favorites", map[string]any{"model": "mock-chat", "favorite": true})
	e.admin("PUT", "/api/favorites", map[string]any{"model": "我的别名", "favorite": true})
	e.admin("PUT", "/api/favorites", map[string]any{"model": "我的别名", "favorite": false})
	e.gw.Close()
	e.srv.Close()
	e.start()
	_, out = e.admin("GET", "/api/models?favorite=1", nil)
	items := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "mock-chat" {
		t.Errorf("仅看收藏应只有 mock-chat: %v", items)
	}
}

// ———————————————————— 11 请求日志查询 / 12 导出 / 13 14 费用合计 ————————————————————

func TestLogsExportUsage(t *testing.T) {
	e := newEnv(t)
	key, _ := e.newKey("账务", -1, 0, 0)
	var ids []string
	for i := 1; i <= 5; i++ {
		ids = append(ids, e.call(context.Background(), key, "/v1/chat/completions", chatBody(map[string]any{"prompt_tokens": 100 * i, "completion_tokens": 10})).reqID)
	}
	for i := 0; i < 3; i++ {
		e.call(context.Background(), key, "/v1/embeddings", map[string]any{"model": "mock-embedding", "input": "x", "mock": map[string]any{"prompt_tokens": 7}})
	}
	for _, id := range ids {
		e.logOf(id)
	}
	chatCost := int64(100+200+300+400+500) + 5*20
	embCost := int64(21)

	_, out := e.admin("GET", "/api/logs?request_id="+ids[2], nil)
	expect(t, "按请求 ID 精确查询", out["total"].(float64), 1)
	_, out = e.admin("GET", "/api/logs?request_id="+ids[2][:10], nil)
	expect(t, "前缀不应匹配（精确查询）", out["total"].(float64), 0)
	_, out = e.admin("GET", "/api/logs/"+ids[2], nil)
	if out["log"] == nil || len(out["attempts"].([]any)) != 1 {
		t.Errorf("日志详情应包含上游尝试: %v", out)
	}

	// 费用合计与翻页无关
	for _, page := range []int{1, 2, 3} {
		_, out = e.admin("GET", fmt.Sprintf("/api/usage?page=%d&page_size=3&start=1", page), nil)
		sum := out["summary"].(map[string]any)
		expect(t, fmt.Sprintf("第 %d 页的时间段合计", page), int64(sum["cost"].(float64)), chatCost+embCost)
		expect(t, "明细总数", out["total"].(float64), 8)
	}
	byModel := out["by_model"].([]any)
	expect(t, "按模型分组数", len(byModel), 2)
	first := byModel[0].(map[string]any)
	expect(t, "费用最高的模型", first["model"].(string), "mock-chat")
	expect(t, "mock-chat 费用", int64(first["cost"].(float64)), chatCost)
	expect(t, "mock-embedding 费用", int64(byModel[1].(map[string]any)["cost"].(float64)), embCost)
	// 时间段之外没有数据
	_, out = e.admin("GET", fmt.Sprintf("/api/usage?start=%d", nowMs()+3600_000), nil)
	expect(t, "未来时间段合计为 0", out["summary"].(map[string]any)["cost"].(float64), 0)

	// 导出
	e.login()
	req, _ := http.NewRequest("GET", e.gw.URL+"/api/logs/export?model=mock-chat", nil)
	req.AddCookie(e.cookie)
	resp, _ := http.DefaultClient.Do(req)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csv := string(raw)
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	expect(t, "导出行数（表头 + 5 条 + 合计）", len(lines), 7)
	if !strings.HasPrefix(csv, "\xEF\xBB\xBF时间,请求ID") {
		t.Error("导出应为带 BOM 的中文表头 CSV")
	}
	if !strings.Contains(csv, ids[0]) || !strings.Contains(lines[6], yuan(chatCost)) {
		t.Errorf("导出内容或合计不正确: %s", lines[6])
	}
}

// ———————————————————— 媒体：视频 / 语音 / 音乐 / 音色（假上游） ————————————————————

func (e *env) setupMedia() {
	e.addChannel("假 MiniMax", "minimax", e.up.URL, 0, "MiniMax-H3", "speech-2.8-hd", "music-3.0")
	e.addChannel("假方舟", "volcengine", e.up.URL, 0, "doubao-seedance-2-0-260128")
	e.setModel(ModelConfig{Name: "MiniMax-H3", Kind: "video", BillingMode: "request", PriceUnit: 3_000_000, Priced: true})
	e.setModel(ModelConfig{Name: "doubao-seedance-2-0-260128", Kind: "video", BillingMode: "token", PriceOut: 46_000_000, Priced: true})
	e.setModel(ModelConfig{Name: "speech-2.8-hd", Kind: "tts", BillingMode: "char", PriceIn: 350_000_000, Priced: true})
	e.setModel(ModelConfig{Name: "music-3.0", Kind: "music", BillingMode: "request", PriceUnit: 1_000_000, Priced: true})
	e.setModel(ModelConfig{Name: "voice-clone", Kind: "voice", BillingMode: "request", PriceUnit: 9_900_000, Priced: true})
	e.setModel(ModelConfig{Name: "voice-design", Kind: "voice", BillingMode: "request", PriceUnit: 9_900_000, Priced: true})
}

func (e *env) waitTask(key, id string) map[string]any {
	e.t.Helper()
	for i := 0; i < 200; i++ {
		req, _ := http.NewRequest("GET", e.gw.URL+"/v1/tasks/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			e.t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if s := out["status"]; s == "succeeded" || s == "failed" {
			return out
		}
		time.Sleep(30 * time.Millisecond)
	}
	e.t.Fatal("任务超时未结束")
	return nil
}

func (e *env) get(key, path string) (int, []byte, http.Header) {
	req, _ := http.NewRequest("GET", e.gw.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func TestVideoTasks(t *testing.T) {
	e := newEnv(t)
	e.setupMedia()
	key, id := e.newKey("视频", 100_000_000, 0, 0)

	// MiniMax 文生视频：按次计费
	r := e.call(context.Background(), key, "/v1/video/generations", map[string]any{"model": "MiniMax-H3", "prompt": "一只猫在草地上奔跑", "duration": 5})
	expect(t, "创建任务", r.status, 200)
	_, reserved := e.keyState(id)
	expect(t, "任务进行中保留预留", reserved, int64(3_000_000))
	task := e.waitTask(key, r.body["id"].(string))
	expect(t, "任务状态", task["status"].(string), "succeeded")
	expect(t, "模式", task["mode"].(string), "t2v")
	code, data, hdr := e.get(key, "/v1/tasks/"+r.body["id"].(string)+"/content?download=1")
	expect(t, "下载成品", code, 200)
	expect(t, "成品大小", len(data), 5000)
	if !strings.Contains(hdr.Get("Content-Disposition"), "attachment") {
		t.Error("下载应带 attachment")
	}
	used, reserved := e.keyState(id)
	expect(t, "成功后按单价结算", used, int64(3_000_000))
	expect(t, "预留释放", reserved, int64(0))
	expect(t, "日志费用", e.logOf(r.reqID).Cost, int64(3_000_000))

	// Seedance 首帧图生视频：按上游返回的 Token 计费
	r = e.call(context.Background(), key, "/v1/video/generations", map[string]any{"model": "doubao-seedance-2-0-260128", "prompt": "镜头缓慢推进",
		"first_frame_image": "data:image/png;base64,iVBORw0KGgo=", "duration": 5, "resolution": "720p"})
	expect(t, "创建 Seedance 任务", r.status, 200)
	task = e.waitTask(key, r.body["id"].(string))
	expect(t, "Seedance 状态", task["status"].(string), "succeeded")
	expect(t, "Seedance 模式", task["mode"].(string), "i2v")
	l := e.logOf(r.reqID)
	expect(t, "Seedance 按 Token 结算", l.Cost, int64(50000*46))
	expect(t, "Seedance 费用状态", l.CostStatus, costActual)

	// 上游任务失败：不扣费
	used0, _ := e.keyState(id)
	r = e.call(context.Background(), key, "/v1/video/generations", map[string]any{"model": "MiniMax-H3", "prompt": "[mock-fail] 测试失败"})
	task = e.waitTask(key, r.body["id"].(string))
	expect(t, "失败任务状态", task["status"].(string), "failed")
	used1, reserved := e.keyState(id)
	expect(t, "失败不扣费", used1, used0)
	expect(t, "失败后预留释放", reserved, int64(0))

	// 其他 Key 看不到该任务
	other, _ := e.newKey("他人", -1, 0, 0)
	code, _, _ = e.get(other, "/v1/tasks/"+r.body["id"].(string))
	expect(t, "跨 Key 访问任务", code, 404)
}

func TestSpeechMusicVoices(t *testing.T) {
	e := newEnv(t)
	e.setupMedia()
	key, id := e.newKey("音频", 1_000_000_000, 0, 0)
	post := func(path string, body any) (*http.Response, []byte) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", e.gw.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp, data
	}

	// 普通朗读："你好abc" = 2×2 + 3 = 7 计费字符，350 元/百万字符
	resp, audio := post("/v1/audio/speech", map[string]any{"model": "speech-2.8-hd", "input": "你好abc", "voice": "mock-voice-1"})
	expect(t, "朗读状态码", resp.StatusCode, 200)
	expect(t, "音频类型", resp.Header.Get("Content-Type"), "audio/mpeg")
	expect(t, "音频大小", len(audio), 4000)
	l := e.logOf(resp.Header.Get("X-Request-Id"))
	expect(t, "计费字符数", l.Units, int64(7))
	expect(t, "朗读费用", l.Cost, int64(7*350))

	// 流式朗读：边生成边下发，结束块里重复的整段音频不重复下发
	resp, audio = post("/v1/audio/speech", map[string]any{"model": "speech-2.8-hd", "input": "流式朗读测试", "voice": "mock-voice-1", "stream": true})
	expect(t, "流式状态码", resp.StatusCode, 200)
	expect(t, "流式音频大小（不含重复的整段）", len(audio), 4000)
	l = e.logOf(resp.Header.Get("X-Request-Id"))
	expect(t, "流式费用状态", l.CostStatus, costActual)
	expect(t, "流式计费字符数", l.Units, int64(12))

	// 上游业务错误：不扣费
	u0, _ := e.keyState(id)
	resp, _ = post("/v1/audio/speech", map[string]any{"model": "speech-2.8-hd", "input": "[mock-error]", "voice": "mock-voice-1"})
	expect(t, "业务错误状态码", resp.StatusCode, 400)
	u1, r1 := e.keyState(id)
	expect(t, "业务错误不扣费", u1, u0)
	expect(t, "业务错误后预留释放", r1, int64(0))

	// 系统音色
	code, data, _ := e.get(key, "/v1/audio/voices?model=speech-2.8-hd")
	expect(t, "音色列表", code, 200)
	if !strings.Contains(string(data), "mock-voice-2") {
		t.Errorf("应返回上游系统音色: %s", data)
	}

	// 长文本配音：后台任务
	long := strings.Repeat("这是一段很长的文本。", 200)
	resp, data = post("/v1/audio/speech/async", map[string]any{"model": "speech-2.8-hd", "input": long, "voice": "mock-voice-1"})
	expect(t, "创建配音任务", resp.StatusCode, 200)
	var task map[string]any
	json.Unmarshal(data, &task)
	done := e.waitTask(key, task["id"].(string))
	expect(t, "配音任务状态", done["status"].(string), "succeeded")
	code, data, _ = e.get(key, "/v1/tasks/"+task["id"].(string)+"/content")
	expect(t, "下载配音", code, 200)
	expect(t, "配音大小", len(data), 5000)
	expect(t, "配音费用（按上游字符数）", e.logOf(resp.Header.Get("X-Request-Id")).Cost, int64(len([]rune(long))*2*350))

	// 音乐
	resp, data = post("/v1/music/generations", map[string]any{"model": "music-3.0", "prompt": "轻快的流行乐", "lyrics": "[Verse]\n阳光洒在窗台"})
	expect(t, "创建音乐任务", resp.StatusCode, 200)
	json.Unmarshal(data, &task)
	done = e.waitTask(key, task["id"].(string))
	expect(t, "音乐任务状态", done["status"].(string), "succeeded")
	expect(t, "音乐费用", e.logOf(resp.Header.Get("X-Request-Id")).Cost, int64(1_000_000))
	resp, _ = post("/v1/music/generations", map[string]any{"model": "music-3.0", "prompt": "只有风格"})
	expect(t, "缺少歌词", resp.StatusCode, 400)

	// 音色设计
	resp, data = post("/v1/audio/voices/design", map[string]any{"model": "speech-2.8-hd", "prompt": "沉稳的中年男声", "preview_text": "欢迎收听今天的节目。", "name": "主播"})
	expect(t, "音色设计", resp.StatusCode, 200)
	var voice map[string]any
	json.Unmarshal(data, &voice)
	if voice["has_preview"] != true || voice["voice_id"] == "" {
		t.Errorf("音色设计应返回音色与试听: %s", data)
	}
	expect(t, "音色设计费用", e.logOf(resp.Header.Get("X-Request-Id")).Cost, int64(9_900_000))
	// 用设计出的音色朗读另一段文案
	resp, _ = post("/v1/audio/speech", map[string]any{"model": "speech-2.8-hd", "input": "另一段文案", "voice": voice["voice_id"]})
	expect(t, "用设计音色朗读", resp.StatusCode, 200)

	// 音色复刻：必须确认授权
	clone := func(consent string) (*http.Response, []byte) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("model", "speech-2.8-hd")
		mw.WriteField("name", "我的声音")
		mw.WriteField("consent", consent)
		fw, _ := mw.CreateFormFile("file", "sample.m4a")
		fw.Write(make([]byte, 2048))
		mw.Close()
		req, _ := http.NewRequest("POST", e.gw.URL+"/v1/audio/voices/clone", &buf)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, _ = clone("false")
	expect(t, "未确认授权", resp.StatusCode, 400)
	resp, data = clone("true")
	expect(t, "音色复刻", resp.StatusCode, 200)
	json.Unmarshal(data, &voice)
	if voice["has_preview"] != true {
		t.Errorf("复刻应生成试听: %s", data)
	}
	_, out := e.admin("GET", "/api/voices", nil)
	expect(t, "自定义音色数", len(out["items"].([]any)), 2)
	_, reserved := e.keyState(id)
	expect(t, "全部结束后预留归零", reserved, int64(0))
}

// 控制台选择已有 Key 调用：密钥不经过浏览器，仍走同一条网关链路。
func TestConsoleKeySelection(t *testing.T) {
	e := newEnv(t)
	_, id := e.newKey("控制台", -1, 0, 0, "mock-chat")
	b, _ := json.Marshal(chatBody(nil))
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("X-Console-Key-Id", fmt.Sprint(id))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	expect(t, "未登录时不可用", resp.StatusCode, 401)
	e.login()
	req, _ = http.NewRequest("POST", e.gw.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("X-Console-Key-Id", fmt.Sprint(id))
	req.AddCookie(e.cookie)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	expect(t, "登录后可用", resp.StatusCode, 200)
	l := e.logOf(resp.Header.Get("X-Request-Id"))
	expect(t, "日志来源", l.Source, "console")
	expect(t, "日志归属 Key", l.KeyID, id)
	// 该 Key 可用的模型
	req, _ = http.NewRequest("GET", e.gw.URL+"/v1/models?kind=chat", nil)
	req.Header.Set("X-Console-Key-Id", fmt.Sprint(id))
	req.AddCookie(e.cookie)
	resp, _ = http.DefaultClient.Do(req)
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	expect(t, "Key 可用的聊天模型数", len(out["data"].([]any)), 1)
}
