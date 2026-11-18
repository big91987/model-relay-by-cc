package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ———————————————————— 渠道 ————————————————————

type ChannelModel struct {
	Model         string `json:"model"`          // 对外的模型名（别名）
	UpstreamModel string `json:"upstream_model"` // 上游真实模型名
}

type Channel struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	BaseURL   string         `json:"base_url"`
	APIKey    string         `json:"api_key,omitempty"` // 仅写入；读取时只返回掩码
	KeyMasked string         `json:"api_key_masked"`
	GroupID   string         `json:"group_id"`
	Status    int            `json:"status"`
	Priority  int            `json:"priority"`
	Weight    int            `json:"weight"`
	Models    []ChannelModel `json:"models"`
	CreatedAt int64          `json:"created_at"`
	UpdatedAt int64          `json:"updated_at"`
}

var channelTypes = map[string]string{
	"openai":     "",
	"deepseek":   "https://api.deepseek.com/v1",
	"zhipu":      "https://open.bigmodel.cn/api/paas/v4",
	"minimax":    "https://api.minimaxi.com",
	"volcengine": "https://ark.cn-beijing.volces.com/api/v3",
}

func (s *Server) handleChannelList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id, name, type, base_url, api_key, group_id, status, priority, weight, created_at, updated_at FROM channels ORDER BY priority DESC, id`)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	list := []*Channel{}
	byID := map[int64]*Channel{}
	for rows.Next() {
		var c Channel
		var key string
		rows.Scan(&c.ID, &c.Name, &c.Type, &c.BaseURL, &key, &c.GroupID, &c.Status, &c.Priority, &c.Weight, &c.CreatedAt, &c.UpdatedAt)
		c.KeyMasked = maskSecret(key) // 渠道密钥不下发到浏览器
		c.Models = []ChannelModel{}
		list = append(list, &c)
		byID[c.ID] = &c
	}
	rows.Close()
	mrows, err := s.db.Query(`SELECT channel_id, model, upstream_model FROM channel_models ORDER BY id`)
	if err == nil {
		for mrows.Next() {
			var id int64
			var m ChannelModel
			mrows.Scan(&id, &m.Model, &m.UpstreamModel)
			if c := byID[id]; c != nil {
				c.Models = append(c.Models, m)
			}
		}
		mrows.Close()
	}
	writeJSON(w, 200, map[string]any{"items": list})
}

func guessKind(name string) string {
	n := strings.ToLower(name)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(n, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("embedding", "embed", "bge-"):
		return "embedding"
	case has("speech", "tts"):
		return "tts"
	case has("music"):
		return "music"
	case has("seedance", "hailuo", "video", "t2v", "i2v", "s2v", "minimax-h3"):
		return "video"
	}
	return "chat"
}

func defaultBilling(kind string) string {
	switch kind {
	case "video", "music", "voice":
		return "request"
	case "tts":
		return "char"
	}
	return "token"
}

// ensureModelRows 为渠道中出现的新模型创建未定价的模型条目，已有条目不动。
func ensureModelRows(tx *sql.Tx, names []string) error {
	for _, n := range names {
		kind := guessKind(n)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO models(name, kind, billing_mode, updated_at) VALUES(?,?,?,?)`, n, kind, defaultBilling(kind), nowMs()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) saveChannel(c *Channel) error {
	c.Name, c.BaseURL = strings.TrimSpace(c.Name), strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.Name == "" {
		return errors.New("渠道名称不能为空")
	}
	def, ok := channelTypes[c.Type]
	if !ok {
		return errors.New("不支持的渠道类型")
	}
	if c.BaseURL == "" {
		c.BaseURL = def
	}
	if u, err := url.Parse(c.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("Base URL 必须是 http(s) 地址")
	}
	if c.Weight <= 0 {
		c.Weight = 1
	}
	seen := map[string]bool{}
	var names []string
	for i := range c.Models {
		m := &c.Models[i]
		m.Model, m.UpstreamModel = strings.TrimSpace(m.Model), strings.TrimSpace(m.UpstreamModel)
		if m.Model == "" {
			m.Model = m.UpstreamModel
		}
		if m.UpstreamModel == "" {
			m.UpstreamModel = m.Model
		}
		if m.Model == "" {
			return errors.New("模型名称不能为空")
		}
		if seen[m.Model] {
			return fmt.Errorf("模型 %s 重复", m.Model)
		}
		seen[m.Model] = true
		names = append(names, m.Model)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowMs()
	if c.ID == 0 {
		res, err := tx.Exec(`INSERT INTO channels(name, type, base_url, api_key, group_id, status, priority, weight, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			c.Name, c.Type, c.BaseURL, strings.TrimSpace(c.APIKey), strings.TrimSpace(c.GroupID), c.Status, c.Priority, c.Weight, now, now)
		if err != nil {
			return err
		}
		c.ID, _ = res.LastInsertId()
	} else {
		res, err := tx.Exec(`UPDATE channels SET name=?, type=?, base_url=?, group_id=?, status=?, priority=?, weight=?, updated_at=? WHERE id=?`,
			c.Name, c.Type, c.BaseURL, strings.TrimSpace(c.GroupID), c.Status, c.Priority, c.Weight, now, c.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("渠道不存在")
		}
		if k := strings.TrimSpace(c.APIKey); k != "" { // 留空表示保留原密钥
			if _, err := tx.Exec(`UPDATE channels SET api_key=? WHERE id=?`, k, c.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM channel_models WHERE channel_id=?`, c.ID); err != nil {
			return err
		}
	}
	for _, m := range c.Models {
		if _, err := tx.Exec(`INSERT INTO channel_models(channel_id, model, upstream_model) VALUES(?,?,?)`, c.ID, m.Model, m.UpstreamModel); err != nil {
			return err
		}
	}
	if err := ensureModelRows(tx, names); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) handleChannelSave(w http.ResponseWriter, r *http.Request) {
	var c Channel
	if err := decodeJSON(r, &c); err != nil {
		writeAdminError(w, 400, "请求格式错误")
		return
	}
	c.ID = 0
	if idStr := r.PathValue("id"); idStr != "" {
		c.ID, _ = strconv.ParseInt(idStr, 10, 64)
		if c.ID == 0 {
			writeAdminError(w, 400, "渠道 ID 无效")
			return
		}
	}
	if err := s.saveChannel(&c); err != nil {
		writeAdminError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": c.ID})
}

func (s *Server) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	tx, err := s.db.Begin()
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM channel_models WHERE channel_id=?`, id)
	tx.Exec(`DELETE FROM channels WHERE id=?`, id)
	tx.Commit()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// fetchUpstreamModels 从上游拉取模型列表。
func (s *Server) fetchUpstreamModels(ctx context.Context, typ, baseURL, apiKey string) ([]string, error) {
	ch := &Candidate{Type: typ, BaseURL: baseURL, APIKey: apiKey}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ch.url("/models"), nil)
	if err != nil {
		return nil, err
	}
	ch.authorize(req)
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接上游: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("上游返回 HTTP %d：%s", resp.StatusCode, upstreamErrMessage(body))
	}
	var parsed struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("上游返回的模型列表无法解析")
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range append(parsed.Data, parsed.Models...) {
		id := str(m["id"])
		if id == "" {
			id = str(m["name"])
		}
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("上游没有返回任何模型（该上游可能不提供模型列表接口，请手动填写）")
	}
	sort.Strings(out)
	return out, nil
}

func (s *Server) handleChannelFetchModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChannelID int64  `json:"channel_id"`
		Type      string `json:"type"`
		BaseURL   string `json:"base_url"`
		APIKey    string `json:"api_key"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeAdminError(w, 400, "请求格式错误")
		return
	}
	req.APIKey = strings.TrimSpace(req.APIKey)
	if req.APIKey == "" && req.ChannelID != 0 {
		// 编辑已有渠道且未重新填写密钥：使用已保存的密钥
		s.db.QueryRow(`SELECT api_key FROM channels WHERE id=?`, req.ChannelID).Scan(&req.APIKey)
	}
	if _, ok := channelTypes[req.Type]; !ok {
		writeAdminError(w, 400, "不支持的渠道类型")
		return
	}
	if strings.TrimSpace(req.BaseURL) == "" {
		req.BaseURL = channelTypes[req.Type]
	}
	if req.BaseURL == "" {
		writeAdminError(w, 400, "请先填写 Base URL")
		return
	}
	models, err := s.fetchUpstreamModels(r.Context(), req.Type, req.BaseURL, req.APIKey)
	if err != nil {
		// 获取失败只返回错误，不改动任何已保存的配置
		writeAdminError(w, 502, "获取模型列表失败："+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

func (s *Server) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct {
		Model string `json:"model"`
	}
	decodeJSON(r, &req)
	var ch Candidate
	err := s.db.QueryRow(`SELECT c.id, c.name, c.type, c.base_url, c.api_key, cm.upstream_model FROM channels c JOIN channel_models cm ON cm.channel_id=c.id
		WHERE c.id=? AND cm.model=?`, id, req.Model).Scan(&ch.ChannelID, &ch.ChannelName, &ch.Type, &ch.BaseURL, &ch.APIKey, &ch.UpstreamModel)
	if err != nil {
		writeAdminError(w, 404, "渠道或模型不存在")
		return
	}
	kind := guessKind(req.Model)
	if mc := s.modelConfig(req.Model); mc != nil {
		kind = mc.Kind
	}
	var path string
	var payload map[string]any
	switch kind {
	case "chat":
		path, payload = "/chat/completions", map[string]any{"model": ch.UpstreamModel, "messages": []map[string]string{{"role": "user", "content": "你好"}}, "max_tokens": 8}
	case "embedding":
		path, payload = "/embeddings", map[string]any{"model": ch.UpstreamModel, "input": "测试"}
	default:
		writeJSON(w, 200, map[string]any{"ok": false, "message": kindLabel(kind) + "模型不支持一键测试，请到对应功能页实际调用验证"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	b, _ := json.Marshal(payload)
	hreq, _ := http.NewRequestWithContext(ctx, "POST", ch.url(path), strings.NewReader(string(b)))
	hreq.Header.Set("Content-Type", "application/json")
	ch.authorize(hreq)
	t0 := time.Now()
	resp, err := s.httpc.Do(hreq)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "message": "连接失败：" + err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ms := time.Since(t0).Milliseconds()
	if resp.StatusCode >= 400 {
		writeJSON(w, 200, map[string]any{"ok": false, "latency_ms": ms, "message": fmt.Sprintf("上游返回 HTTP %d：%s", resp.StatusCode, upstreamErrMessage(body))})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "latency_ms": ms, "message": "测试通过（该测试直接调用上游，会产生极少量上游费用，不计入网关 Key）"})
}

// ———————————————————— 模型 ————————————————————

func (s *Server) seedModels() error {
	// 音色复刻/音色设计按次计费的占位条目，价格需管理员按上游账单确认
	for _, n := range []string{"voice-clone", "voice-design"} {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO models(name, kind, billing_mode, note, updated_at) VALUES(?, 'voice', 'request', ?, ?)`,
			n, "按次计费的占位模型，用于记录音色创建费用", nowMs()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleModelList(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	type item struct {
		*ModelConfig
		Channels  []map[string]any `json:"channels"`
		Available bool             `json:"available"`
		Favorite  bool             `json:"favorite"`
	}
	byName := map[string]*item{}
	var order []string
	rows, err := s.db.Query(`SELECT ` + modelColumns + ` FROM models ORDER BY name`)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			continue
		}
		byName[m.Name] = &item{ModelConfig: m, Channels: []map[string]any{}}
		order = append(order, m.Name)
	}
	rows.Close()
	crows, err := s.db.Query(`SELECT cm.model, cm.upstream_model, c.id, c.name, c.type, c.status FROM channel_models cm JOIN channels c ON c.id=cm.channel_id ORDER BY c.priority DESC, c.id`)
	if err == nil {
		for crows.Next() {
			var model, up, cname, ctype string
			var cid int64
			var status int
			crows.Scan(&model, &up, &cid, &cname, &ctype, &status)
			it := byName[model]
			if it == nil {
				kind := guessKind(model)
				it = &item{ModelConfig: &ModelConfig{Name: model, Kind: kind, BillingMode: defaultBilling(kind), SupportsTemperature: true, TemperatureMax: 2}, Channels: []map[string]any{}}
				byName[model] = it
				order = append(order, model)
			}
			it.Channels = append(it.Channels, map[string]any{"id": cid, "name": cname, "type": ctype, "status": status, "upstream_model": up})
			it.Available = it.Available || status == 1
		}
		crows.Close()
	}
	frows, err := s.db.Query(`SELECT model FROM favorites WHERE user_id=?`, u.ID)
	if err == nil {
		for frows.Next() {
			var m string
			frows.Scan(&m)
			if it := byName[m]; it != nil {
				it.Favorite = true
			}
		}
		frows.Close()
	}
	sort.Strings(order)
	onlyFav := r.URL.Query().Get("favorite") == "1"
	out := []*item{}
	for _, n := range order {
		if onlyFav && !byName[n].Favorite {
			continue
		}
		out = append(out, byName[n])
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) handleModelSave(w http.ResponseWriter, r *http.Request) {
	var m ModelConfig
	if err := decodeJSON(r, &m); err != nil {
		writeAdminError(w, 400, "请求格式错误")
		return
	}
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		writeAdminError(w, 400, "模型名称不能为空")
		return
	}
	switch m.Kind {
	case "chat", "embedding", "video", "tts", "music", "voice":
	default:
		writeAdminError(w, 400, "模型类型无效")
		return
	}
	switch m.BillingMode {
	case "token", "char", "request":
	default:
		writeAdminError(w, 400, "计费方式无效")
		return
	}
	if m.PriceIn < 0 || m.PriceOut < 0 || m.PriceUnit < 0 || m.MaxOutputTokens < 0 || m.DefaultMaxTokens < 0 {
		writeAdminError(w, 400, "价格和 Token 上限不能为负数")
		return
	}
	if m.TemperatureMax <= 0 {
		m.TemperatureMax = 2
	}
	_, err := s.db.Exec(`INSERT INTO models(name, kind, billing_mode, price_in, price_out, price_unit, priced, max_output_tokens, default_max_tokens, supports_temperature, temperature_max, note, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET kind=excluded.kind, billing_mode=excluded.billing_mode, price_in=excluded.price_in, price_out=excluded.price_out,
		price_unit=excluded.price_unit, priced=excluded.priced, max_output_tokens=excluded.max_output_tokens, default_max_tokens=excluded.default_max_tokens,
		supports_temperature=excluded.supports_temperature, temperature_max=excluded.temperature_max, note=excluded.note, updated_at=excluded.updated_at`,
		m.Name, m.Kind, m.BillingMode, m.PriceIn, m.PriceOut, m.PriceUnit, boolInt(m.Priced), m.MaxOutputTokens, m.DefaultMaxTokens,
		boolInt(m.SupportsTemperature), m.TemperatureMax, m.Note, nowMs())
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM channel_models WHERE model=?`, name).Scan(&n)
	if n > 0 {
		writeAdminError(w, 400, "仍有渠道在使用该模型，请先从渠道中移除")
		return
	}
	s.db.Exec(`DELETE FROM models WHERE name=?`, name)
	s.db.Exec(`DELETE FROM favorites WHERE model=?`, name)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleFavorite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Favorite bool   `json:"favorite"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Model == "" {
		writeAdminError(w, 400, "请求格式错误")
		return
	}
	u := userFrom(r)
	var err error
	if req.Favorite {
		_, err = s.db.Exec(`INSERT OR IGNORE INTO favorites(user_id, model, created_at) VALUES(?,?,?)`, u.ID, req.Model, nowMs())
	} else {
		_, err = s.db.Exec(`DELETE FROM favorites WHERE user_id=? AND model=?`, u.ID, req.Model)
	}
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "favorite": req.Favorite})
}

// ———————————————————— 网关 Key ————————————————————

func (s *Server) handleKeyList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT ` + keyColumns + ` FROM api_keys ORDER BY id DESC`)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			continue
		}
		b, _ := json.Marshal(k)
		var m map[string]any
		json.Unmarshal(b, &m)
		m["inflight"] = s.limiter.inflightOf(k.ID)
		list = append(list, m)
	}
	writeJSON(w, 200, map[string]any{"items": list})
}

func (s *Server) createKey(k *APIKey) (string, error) {
	secret := "sk-relay-" + randHex(24)
	models, _ := json.Marshal(k.Models)
	res, err := s.db.Exec(`INSERT INTO api_keys(name, key_hash, key_prefix, status, quota_limit, rpm_limit, max_concurrency, models, expires_at, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, k.Name, sha256Hex(secret), secret[:12]+"…"+secret[len(secret)-4:], k.Status, k.QuotaLimit, k.RPMLimit, k.MaxConcurrency, string(models), k.ExpiresAt, nowMs())
	if err != nil {
		return "", err
	}
	k.ID, _ = res.LastInsertId()
	return secret, nil
}

func (s *Server) handleKeySave(w http.ResponseWriter, r *http.Request) {
	var k APIKey
	if err := decodeJSON(r, &k); err != nil {
		writeAdminError(w, 400, "请求格式错误")
		return
	}
	k.Name = strings.TrimSpace(k.Name)
	if k.Name == "" {
		writeAdminError(w, 400, "Key 名称不能为空")
		return
	}
	if k.QuotaLimit < 0 {
		k.QuotaLimit = -1
	}
	if k.RPMLimit < 0 || k.MaxConcurrency < 0 {
		writeAdminError(w, 400, "限流参数不能为负数")
		return
	}
	if k.Models == nil {
		k.Models = []string{}
	}
	idStr := r.PathValue("id")
	if idStr == "" {
		k.Status = 1
		secret, err := s.createKey(&k)
		if err != nil {
			writeAdminError(w, 500, err.Error())
			return
		}
		// 完整密钥只在创建时返回一次，服务端仅保存哈希
		writeJSON(w, 200, map[string]any{"id": k.ID, "key": secret})
		return
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)
	models, _ := json.Marshal(k.Models)
	res, err := s.db.Exec(`UPDATE api_keys SET name=?, status=?, quota_limit=?, rpm_limit=?, max_concurrency=?, models=?, expires_at=? WHERE id=?`,
		k.Name, k.Status, k.QuotaLimit, k.RPMLimit, k.MaxConcurrency, string(models), k.ExpiresAt, id)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeAdminError(w, 404, "Key 不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM reservations WHERE key_id=? AND status IN ('held','pending_review')`, id).Scan(&n)
	if n > 0 {
		writeAdminError(w, 400, "该 Key 仍有在途或待核实的预留，请处理后再删除（可先禁用）")
		return
	}
	// 历史日志保留 key_name，财务对账不受删除影响
	s.db.Exec(`DELETE FROM api_keys WHERE id=?`, id)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ———————————————————— 调用日志 / 用量 ————————————————————

type RequestLog struct {
	ID               int64  `json:"id"`
	RequestID        string `json:"request_id"`
	KeyID            int64  `json:"key_id"`
	KeyName          string `json:"key_name"`
	Source           string `json:"source"`
	Endpoint         string `json:"endpoint"`
	Kind             string `json:"kind"`
	Model            string `json:"model"`
	ChannelID        int64  `json:"channel_id"`
	ChannelName      string `json:"channel_name"`
	UpstreamModel    string `json:"upstream_model"`
	Stream           bool   `json:"stream"`
	Status           string `json:"status"`
	HTTPStatus       int    `json:"http_status"`
	ErrorCode        string `json:"error_code"`
	ErrorMessage     string `json:"error_message"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	Units            int64  `json:"units"`
	Cost             int64  `json:"cost"`
	CostStatus       string `json:"cost_status"`
	UsageSource      string `json:"usage_source"`
	Reserved         int64  `json:"reserved"`
	Attempts         int    `json:"attempts"`
	DurationMs       int64  `json:"duration_ms"`
	FirstTokenMs     int64  `json:"first_token_ms"`
	CreatedAt        int64  `json:"created_at"`
	FinishedAt       int64  `json:"finished_at"`
}

const logColumns = `id, request_id, key_id, key_name, source, endpoint, kind, model, channel_id, channel_name, upstream_model, stream, status, http_status,
	error_code, error_message, prompt_tokens, completion_tokens, total_tokens, units, cost, cost_status, usage_source, reserved, attempts, duration_ms, first_token_ms, created_at, finished_at`

func scanLog(row rowScanner) (*RequestLog, error) {
	var l RequestLog
	var stream int
	err := row.Scan(&l.ID, &l.RequestID, &l.KeyID, &l.KeyName, &l.Source, &l.Endpoint, &l.Kind, &l.Model, &l.ChannelID, &l.ChannelName, &l.UpstreamModel, &stream, &l.Status, &l.HTTPStatus,
		&l.ErrorCode, &l.ErrorMessage, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.Units, &l.Cost, &l.CostStatus, &l.UsageSource, &l.Reserved, &l.Attempts, &l.DurationMs, &l.FirstTokenMs, &l.CreatedAt, &l.FinishedAt)
	l.Stream = stream == 1
	return &l, err
}

// logFilter 根据查询参数构造 WHERE 子句，日志列表、导出和用量统计共用。
func logFilter(r *http.Request) (string, []any) {
	q := r.URL.Query()
	var conds []string
	var args []any
	add := func(cond string, v any) {
		conds = append(conds, cond)
		args = append(args, v)
	}
	if v := strings.TrimSpace(q.Get("request_id")); v != "" {
		add("request_id = ?", v) // 精确匹配
	}
	if v := q.Get("key_id"); v != "" {
		id, _ := strconv.ParseInt(v, 10, 64)
		add("key_id = ?", id)
	}
	if v := q.Get("model"); v != "" {
		add("model = ?", v)
	}
	if v := q.Get("status"); v != "" {
		add("status = ?", v)
	}
	if v := q.Get("kind"); v != "" {
		add("kind = ?", v)
	}
	if v := q.Get("cost_status"); v != "" {
		add("cost_status = ?", v)
	}
	if v := qInt(r, "start", 0); v > 0 {
		add("created_at >= ?", v)
	}
	if v := qInt(r, "end", 0); v > 0 {
		add("created_at < ?", v)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func paging(r *http.Request) (limit, offset int64) {
	page := max(qInt(r, "page", 1), 1)
	size := qInt(r, "page_size", 20)
	if size <= 0 || size > 200 {
		size = 20
	}
	return size, (page - 1) * size
}

func (s *Server) queryLogs(where string, args []any, limit, offset int64) ([]*RequestLog, int64, error) {
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT `+logColumns+` FROM request_logs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []*RequestLog{}
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, l)
	}
	return items, total, nil
}

func (s *Server) handleLogList(w http.ResponseWriter, r *http.Request) {
	where, args := logFilter(r)
	limit, offset := paging(r)
	items, total, err := s.queryLogs(where, args, limit, offset)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total})
}

func (s *Server) handleLogDetail(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("request_id")
	l, err := scanLog(s.db.QueryRow(`SELECT `+logColumns+` FROM request_logs WHERE request_id=?`, reqID))
	if err != nil {
		writeAdminError(w, 404, "未找到该请求 ID 对应的日志")
		return
	}
	attempts := []map[string]any{}
	rows, err := s.db.Query(`SELECT attempt, channel_id, channel_name, upstream_model, status, http_status, prompt_tokens, completion_tokens, cost, cost_status, error, duration_ms, created_at
		FROM upstream_attempts WHERE request_id=? ORDER BY id`, reqID)
	if err == nil {
		for rows.Next() {
			var attempt, httpStatus int
			var chID, pt, ct, cost, dur, created int64
			var chName, up, status, cs, e string
			rows.Scan(&attempt, &chID, &chName, &up, &status, &httpStatus, &pt, &ct, &cost, &cs, &e, &dur, &created)
			attempts = append(attempts, map[string]any{"attempt": attempt, "channel_id": chID, "channel_name": chName, "upstream_model": up, "status": status,
				"http_status": httpStatus, "prompt_tokens": pt, "completion_tokens": ct, "cost": cost, "cost_status": cs, "error": e, "duration_ms": dur, "created_at": created})
		}
		rows.Close()
	}
	var resv any
	var id, amount, actual, created, updated int64
	var status, cs, note, by string
	var dispatched int
	if s.db.QueryRow(`SELECT id, amount, status, actual_cost, cost_status, dispatched, note, resolved_by, created_at, updated_at FROM reservations WHERE request_id=?`, reqID).
		Scan(&id, &amount, &status, &actual, &cs, &dispatched, &note, &by, &created, &updated) == nil {
		resv = map[string]any{"id": id, "amount": amount, "status": status, "actual_cost": actual, "cost_status": cs, "dispatched": dispatched == 1,
			"note": note, "resolved_by": by, "created_at": created, "updated_at": updated}
	}
	writeJSON(w, 200, map[string]any{"log": l, "attempts": attempts, "reservation": resv})
}

func csvCell(s string) string {
	// 防止表格软件把内容当作公式执行
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		s = "'" + s
	}
	if strings.ContainsAny(s, ",\"\r\n") {
		s = `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

var statusLabels = map[string]string{"success": "成功", "error": "失败", "canceled": "已取消", "rejected": "已拒绝", "in_progress": "进行中", "interrupted": "中断"}
var costStatusLabels = map[string]string{costActual: "实际", costEstimated: "估算", costPending: "待核实", costNone: "未计费", costUnpriced: "未定价"}

// handleLogExport 导出调用明细 CSV（UTF-8 带 BOM，可直接用 Excel 打开），供财务对账。
// 金额以“元”为单位、保留 6 位小数；末尾附合计行。
func (s *Server) handleLogExport(w http.ResponseWriter, r *http.Request) {
	where, args := logFilter(r)
	rows, err := s.db.Query(`SELECT `+logColumns+` FROM request_logs`+where+` ORDER BY id`, args...)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	loc := time.Local
	if tz := r.URL.Query().Get("tz_offset"); tz != "" { // 浏览器时区偏移（分钟，UTC 以东为正）
		if m, err := strconv.Atoi(tz); err == nil && m >= -14*60 && m <= 14*60 {
			loc = time.FixedZone("", m*60)
		}
	}
	name := "调用明细_" + time.Now().In(loc).Format("20060102_150405") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"call_details.csv\"; filename*=UTF-8''"+url.PathEscape(name))
	w.Write([]byte("\xEF\xBB\xBF"))
	fmt.Fprintln(w, "时间,请求ID,Key名称,Key ID,来源,接口,模型,渠道,上游模型,流式,状态,HTTP状态,输入Token,输出Token,总Token,计费数量,费用(元),费用状态,用量来源,耗时(ms),错误信息")
	var total, count int64
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			continue
		}
		total += l.Cost
		count++
		stream := "否"
		if l.Stream {
			stream = "是"
		}
		src := "API"
		if l.Source == "console" {
			src = "控制台"
		}
		cells := []string{
			time.UnixMilli(l.CreatedAt).In(loc).Format("2006-01-02 15:04:05"), l.RequestID, l.KeyName, strconv.FormatInt(l.KeyID, 10), src, l.Endpoint, l.Model,
			l.ChannelName, l.UpstreamModel, stream, statusLabels[l.Status], strconv.Itoa(l.HTTPStatus),
			strconv.FormatInt(l.PromptTokens, 10), strconv.FormatInt(l.CompletionTokens, 10), strconv.FormatInt(l.TotalTokens, 10), strconv.FormatInt(l.Units, 10),
			yuan(l.Cost), costStatusLabels[l.CostStatus], l.UsageSource, strconv.FormatInt(l.DurationMs, 10), l.ErrorMessage,
		}
		for i := range cells {
			cells[i] = csvCell(cells[i])
		}
		fmt.Fprintln(w, strings.Join(cells, ","))
	}
	fmt.Fprintf(w, "合计,共 %d 条,,,,,,,,,,,,,,,%s,,,,\n", count, yuan(total))
}

// handleUsage 返回所选时间段的费用合计、按模型汇总与分页明细。
// 合计与按模型汇总始终针对整个时间段计算，与分页无关。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	where, args := logFilter(r)
	limit, offset := paging(r)
	summary := map[string]any{}
	var reqs, okReqs, pt, ct, cost, estCost, pendingCnt int64
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(status='success'),0), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cost),0),
		COALESCE(SUM(CASE WHEN cost_status='estimated' THEN cost ELSE 0 END),0), COALESCE(SUM(cost_status='pending'),0) FROM request_logs`+where, args...).
		Scan(&reqs, &okReqs, &pt, &ct, &cost, &estCost, &pendingCnt)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	summary["requests"], summary["success_requests"] = reqs, okReqs
	summary["prompt_tokens"], summary["completion_tokens"] = pt, ct
	summary["cost"], summary["estimated_cost"], summary["pending_count"] = cost, estCost, pendingCnt

	byModel := []map[string]any{}
	rows, err := s.db.Query(`SELECT model, MAX(kind), COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(units),0), COALESCE(SUM(cost),0)
		FROM request_logs`+where+` GROUP BY model ORDER BY SUM(cost) DESC, COUNT(*) DESC, model`, args...)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	for rows.Next() {
		var model, kind string
		var n, mpt, mct, units, mcost int64
		rows.Scan(&model, &kind, &n, &mpt, &mct, &units, &mcost)
		byModel = append(byModel, map[string]any{"model": model, "kind": kind, "requests": n, "prompt_tokens": mpt, "completion_tokens": mct, "units": units, "cost": mcost})
	}
	rows.Close()
	items, total, err := s.queryLogs(where, args, limit, offset)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"summary": summary, "by_model": byModel, "items": items, "total": total})
}

// ———————————————————— 账务核实 ————————————————————

func (s *Server) handleReservationList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending_review"
	}
	rows, err := s.db.Query(`SELECT r.id, r.request_id, r.key_id, COALESCE(k.name, l.key_name, ''), r.kind, r.amount, r.status, r.actual_cost, r.cost_status, r.dispatched, r.note, r.resolved_by, r.created_at, r.updated_at,
		COALESCE(l.model,''), COALESCE(l.endpoint,''), COALESCE(l.status,''), COALESCE(l.error_message,'')
		FROM reservations r LEFT JOIN api_keys k ON k.id=r.key_id LEFT JOIN request_logs l ON l.request_id=r.request_id
		WHERE r.status=? ORDER BY r.id DESC LIMIT 500`, status)
	if err != nil {
		writeAdminError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, keyID, amount, actual, created, updated int64
		var reqID, keyName, kind, st, cs, note, by, model, endpoint, lstatus, lerr string
		var dispatched int
		rows.Scan(&id, &reqID, &keyID, &keyName, &kind, &amount, &st, &actual, &cs, &dispatched, &note, &by, &created, &updated, &model, &endpoint, &lstatus, &lerr)
		items = append(items, map[string]any{"id": id, "request_id": reqID, "key_id": keyID, "key_name": keyName, "kind": kind, "amount": amount, "status": st,
			"actual_cost": actual, "cost_status": cs, "dispatched": dispatched == 1, "note": note, "resolved_by": by, "created_at": created, "updated_at": updated,
			"model": model, "endpoint": endpoint, "log_status": lstatus, "log_error": lerr,
			"auto_settle_at": updated + s.cfg.PendingReviewTTL.Milliseconds()})
	}
	writeJSON(w, 200, map[string]any{"items": items, "ttl_seconds": int64(s.cfg.PendingReviewTTL.Seconds())})
}

func (s *Server) handleReservationResolve(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct {
		Action string `json:"action"`
		Amount int64  `json:"amount"`
		Note   string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeAdminError(w, 400, "请求格式错误")
		return
	}
	note := "管理员核实"
	if req.Action == "release" {
		note = "管理员确认未产生费用，释放预留"
	}
	if strings.TrimSpace(req.Note) != "" {
		note += "：" + strings.TrimSpace(req.Note)
	}
	if err := s.resolvePending(id, req.Action, req.Amount, costActual, userFrom(r).Username, note); err != nil {
		writeAdminError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ———————————————————— 概览 ————————————————————

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	count := func(q string, args ...any) int64 {
		var n int64
		s.db.QueryRow(q, args...).Scan(&n)
		return n
	}
	dayStart := qInt(r, "day_start", time.Now().Truncate(24*time.Hour).UnixMilli())
	out["channels"] = count(`SELECT COUNT(*) FROM channels WHERE status=1`)
	out["keys"] = count(`SELECT COUNT(*) FROM api_keys WHERE status=1`)
	out["models"] = count(`SELECT COUNT(DISTINCT cm.model) FROM channel_models cm JOIN channels c ON c.id=cm.channel_id WHERE c.status=1`)
	out["today_requests"] = count(`SELECT COUNT(*) FROM request_logs WHERE created_at>=?`, dayStart)
	out["today_cost"] = count(`SELECT COALESCE(SUM(cost),0) FROM request_logs WHERE created_at>=?`, dayStart)
	out["today_tokens"] = count(`SELECT COALESCE(SUM(total_tokens),0) FROM request_logs WHERE created_at>=?`, dayStart)
	out["total_cost"] = count(`SELECT COALESCE(SUM(cost),0) FROM request_logs`)
	out["pending_review"] = count(`SELECT COUNT(*) FROM reservations WHERE status='pending_review'`)
	out["active_tasks"] = count(`SELECT COUNT(*) FROM media_tasks WHERE status IN ('queued','processing')`)
	series := []map[string]any{}
	const day = int64(24 * 3600 * 1000)
	for i := int64(6); i >= 0; i-- {
		from := dayStart - i*day
		var n, c int64
		s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost),0) FROM request_logs WHERE created_at>=? AND created_at<?`, from, from+day).Scan(&n, &c)
		series = append(series, map[string]any{"day": from, "requests": n, "cost": c})
	}
	out["series"] = series
	writeJSON(w, 200, out)
}
