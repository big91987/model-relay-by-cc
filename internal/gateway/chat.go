package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (ch *Candidate) url(path string) string {
	base := strings.TrimRight(ch.BaseURL, "/")
	if ch.Type == "minimax" {
		// MiniMax 的 Base URL 不含版本号，OpenAI 兼容接口位于 /v1 下
		return base + "/v1" + path
	}
	return base + path
}

func (ch *Candidate) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+ch.APIKey)
}

func jsonRequest(c *call, ch *Candidate, url string, payload any) (*http.Request, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(c.ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	ch.authorize(req)
	return req, nil
}

// messagesText 提取消息中的文本，用于在上游不返回用量时估算。
func messagesText(msgs []any) string {
	var sb strings.Builder
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		switch c := mm["content"].(type) {
		case string:
			sb.WriteString(c)
		case []any:
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					sb.WriteString(str(pm["text"]))
				}
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 32<<20)
	if err != nil {
		writeGatewayError(w, 400, "invalid_request", "读取请求失败: "+err.Error())
		return
	}
	req, err := decodeMap(body)
	if err != nil {
		writeGatewayError(w, 400, "invalid_json", "请求体不是合法的 JSON")
		return
	}
	model := str(req["model"])
	stream, _ := req["stream"].(bool)
	c := s.begin(w, r, "/v1/chat/completions", "chat", model, stream)
	if c == nil {
		return
	}
	defer c.abandon()

	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		c.reject(400, "messages_required", "messages 不能为空")
		return
	}

	// —— 费用上界 ——
	// 输入上界：每个 Token 至少占 1 个字节，因此 messages/tools 的 JSON 字节数加上每条消息的模板开销
	// 一定不小于实际输入 Token 数。输出上界：max_tokens × n；调用方未指定时由网关注入受控上限。
	maxOut, hasMax := num(req["max_tokens"])
	if !hasMax {
		maxOut, hasMax = num(req["max_completion_tokens"])
	}
	if hasMax && maxOut <= 0 {
		c.reject(400, "invalid_max_tokens", "max_tokens 必须为正整数")
		return
	}
	if c.mc != nil && c.mc.MaxOutputTokens > 0 && hasMax && maxOut > c.mc.MaxOutputTokens {
		c.reject(400, "max_tokens_exceeded", fmt.Sprintf("max_tokens 超过模型 %s 的上限 %d", model, c.mc.MaxOutputTokens))
		return
	}
	if !hasMax {
		maxOut = s.cfg.DefaultMaxTokens
		if c.mc != nil && c.mc.DefaultMaxTokens > 0 {
			maxOut = c.mc.DefaultMaxTokens
		}
		if c.mc != nil && c.mc.MaxOutputTokens > 0 && maxOut > c.mc.MaxOutputTokens {
			maxOut = c.mc.MaxOutputTokens
		}
		if c.key.QuotaLimit >= 0 {
			// 有限额度 Key：必须有受控的输出上限，才能得到可信的费用上界
			req["max_tokens"] = maxOut
		}
	}
	n := int64(1)
	if v, ok := num(req["n"]); ok && v > 1 {
		n = v
	}
	msgJSON, _ := json.Marshal(msgs)
	promptUpper := int64(len(msgJSON)) + int64(len(msgs))*16 + 64
	for _, k := range []string{"tools", "functions", "response_format"} {
		if v, ok := req[k]; ok {
			b, _ := json.Marshal(v)
			promptUpper += int64(len(b))
		}
	}
	var upper int64
	if c.mc != nil {
		upper = tokenCost(promptUpper, maxOut*n, c.mc.PriceIn, c.mc.PriceOut)
	}
	if !c.reserveFor(upper, "sync") {
		return
	}

	clientWantsUsage := false
	if so, ok := req["stream_options"].(map[string]any); ok {
		clientWantsUsage, _ = so["include_usage"].(bool)
	}
	resp, t0 := c.tryChannels(func(ch *Candidate) (*http.Request, error) {
		req["model"] = ch.UpstreamModel
		if stream {
			so, _ := req["stream_options"].(map[string]any)
			if so == nil {
				so = map[string]any{}
			}
			so["include_usage"] = true // 流式也要拿到上游的实际用量
			req["stream_options"] = so
		}
		return jsonRequest(c, ch, ch.url("/chat/completions"), req)
	})
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	promptText := messagesText(msgs)
	if stream {
		c.proxyChatStream(resp, t0, promptText, clientWantsUsage)
	} else {
		c.proxyChatJSON(resp, t0, promptText)
	}
}

type usageInfo struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func (c *call) priceTokens(pt, ct int64) int64 {
	if c.mc == nil {
		return 0
	}
	return tokenCost(pt, ct, c.mc.PriceIn, c.mc.PriceOut)
}

func (c *call) proxyChatJSON(resp *http.Response, t0 time.Time, promptText string) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		cls := classifyErr(err, c.r.Context())
		c.recordAttempt(c.used, "unknown", resp.StatusCode, 0, 0, 0, costPending, "读取上游响应中断: "+err.Error(), t0)
		o := outcome{costStatus: costPending, pendingNote: "上游已开始响应但读取中断，没有用量信息，费用待核实"}
		if cls == errCanceled {
			o.status = "canceled"
			c.fail(499, "client_canceled", "调用方已取消请求", o)
		} else {
			c.fail(502, "upstream_interrupted", "读取上游响应时连接中断，本次请求费用待核实", o)
		}
		return
	}
	var parsed map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		c.recordAttempt(c.used, "unknown", resp.StatusCode, 0, 0, 0, costPending, "上游返回了无法解析的响应", t0)
		c.fail(502, "upstream_invalid_response", "上游返回了无法解析的响应，本次请求费用待核实",
			outcome{costStatus: costPending, pendingNote: "上游返回 2xx 但响应无法解析，费用待核实"})
		return
	}
	o := outcome{status: "success", httpStatus: 200}
	if u, ok := parsed["usage"].(map[string]any); ok {
		o.pt, _ = num(u["prompt_tokens"])
		o.ct, _ = num(u["completion_tokens"])
		o.tt, _ = num(u["total_tokens"])
	}
	if o.pt > 0 || o.ct > 0 {
		o.costStatus, o.usageSource = costActual, "upstream"
	} else {
		// 上游没有返回用量：按内容估算，并明确标记为估算
		var out strings.Builder
		if chs, ok := parsed["choices"].([]any); ok {
			for _, x := range chs {
				if m, ok := x.(map[string]any)["message"].(map[string]any); ok {
					out.WriteString(str(m["content"]))
					out.WriteString(str(m["reasoning_content"]))
				}
			}
		}
		o.pt, o.ct = estimateTokens(promptText), estimateTokens(out.String())
		o.tt = o.pt + o.ct
		o.costStatus, o.usageSource = costEstimated, "estimated"
		parsed["usage"] = map[string]any{"prompt_tokens": o.pt, "completion_tokens": o.ct, "total_tokens": o.tt, "estimated": true}
	}
	o.cost = c.priceTokens(o.pt, o.ct)
	parsed["model"] = c.model
	c.recordAttempt(c.used, "success", resp.StatusCode, o.pt, o.ct, o.cost, o.costStatus, "", t0)
	c.w.Header().Set("X-Usage-Source", o.usageSource)
	writeJSON(c.w, 200, parsed)
	c.finish(o)
}

func (c *call) proxyChatStream(resp *http.Response, t0 time.Time, promptText string, clientWantsUsage bool) {
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(200)
	flusher, _ := c.w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	// 空闲超时：上游长时间没有数据则取消
	idle := time.AfterFunc(c.s.cfg.StreamIdleTimeout, c.cancel)
	defer idle.Stop()

	var (
		usage        *usageInfo
		content      strings.Builder
		sawDone      bool
		sawFinish    bool
		gotData      bool
		firstTokenMs int64
		clientGone   bool
		readErr      error
		rewriteModel = c.used.UpstreamModel != c.model
	)
	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			idle.Reset(c.s.cfg.StreamIdleTimeout)
			out := line
			skip := false
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[5:])
				if bytes.Equal(payload, []byte("[DONE]")) {
					sawDone = true
				} else if len(payload) > 0 {
					gotData = true
					var chunk struct {
						Choices []struct {
							Delta struct {
								Content          string `json:"content"`
								ReasoningContent string `json:"reasoning_content"`
							} `json:"delta"`
							FinishReason *string `json:"finish_reason"`
						} `json:"choices"`
						Usage *usageInfo `json:"usage"`
					}
					if json.Unmarshal(payload, &chunk) == nil {
						for _, ch := range chunk.Choices {
							if ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" {
								if firstTokenMs == 0 {
									firstTokenMs = time.Since(c.start).Milliseconds()
								}
								content.WriteString(ch.Delta.Content)
								content.WriteString(ch.Delta.ReasoningContent)
							}
							if ch.FinishReason != nil && *ch.FinishReason != "" {
								sawFinish = true
							}
						}
						if chunk.Usage != nil && (chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0) {
							usage = chunk.Usage
						}
						// 调用方没有要求用量时，不转发仅含用量的附加块，保持与直连上游一致
						if chunk.Usage != nil && len(chunk.Choices) == 0 && !clientWantsUsage {
							skip = true
						}
					}
					if !skip && rewriteModel {
						var m map[string]any
						d := json.NewDecoder(bytes.NewReader(payload))
						d.UseNumber()
						if d.Decode(&m) == nil {
							m["model"] = c.model
							if b, e := json.Marshal(m); e == nil {
								out = append(append([]byte("data: "), b...), '\n')
							}
						}
					}
				}
			}
			if !skip && !clientGone {
				if _, werr := c.w.Write(out); werr != nil {
					clientGone = true
					c.cancel() // 调用方断开：尽力取消上游
				} else if flusher != nil && len(trimmed) == 0 {
					flusher.Flush()
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
		if clientGone {
			break
		}
	}
	if c.r.Context().Err() != nil {
		clientGone = true
	}

	o := outcome{httpStatus: 200, firstTokenMs: firstTokenMs}
	completed := sawDone || (sawFinish && readErr == nil)
	switch {
	case completed:
		o.status = "success"
	case clientGone:
		o.status, o.httpStatus, o.errCode, o.errMsg = "canceled", 499, "client_canceled", "调用方中途断开或停止生成"
	default:
		o.status, o.errCode, o.errMsg = "error", "upstream_interrupted", "上游流式响应中断"
		if readErr != nil {
			o.errMsg += ": " + readErr.Error()
		}
		// 响应头已发出，用 SSE 事件告知调用方
		ev, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "上游流式响应中断，内容可能不完整", "type": "upstream_error", "code": "upstream_interrupted", "request_id": c.reqID}})
		fmt.Fprintf(c.w, "data: %s\n\n", ev)
		if flusher != nil {
			flusher.Flush()
		}
	}
	attemptStatus := "success"
	switch {
	case usage != nil:
		// 拿到了上游的实际用量（即便之后被取消，也按实际结算）
		o.pt, o.ct, o.tt = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
		o.costStatus, o.usageSource = costActual, "upstream"
		o.cost = c.priceTokens(o.pt, o.ct)
	case gotData || completed:
		// 已产生内容但没有用量（取消、断流或上游不支持）：按已收到的内容估算，不视为免费
		o.pt, o.ct = estimateTokens(promptText), estimateTokens(content.String())
		o.tt = o.pt + o.ct
		o.costStatus, o.usageSource = costEstimated, "estimated"
		o.cost = c.priceTokens(o.pt, o.ct)
	default:
		// 没有收到任何数据就中断：费用未知
		o.costStatus = costPending
		o.pendingNote = "流式响应在收到任何数据前中断，上游可能已产生用量，费用待核实"
		attemptStatus = "unknown"
	}
	if o.status != "success" && attemptStatus == "success" {
		attemptStatus = "interrupted"
	}
	c.recordAttempt(c.used, attemptStatus, resp.StatusCode, o.pt, o.ct, o.cost, o.costStatus, o.errMsg, t0)
	c.finish(o)
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 32<<20)
	if err != nil {
		writeGatewayError(w, 400, "invalid_request", "读取请求失败: "+err.Error())
		return
	}
	req, err := decodeMap(body)
	if err != nil {
		writeGatewayError(w, 400, "invalid_json", "请求体不是合法的 JSON")
		return
	}
	model := str(req["model"])
	c := s.begin(w, r, "/v1/embeddings", "embedding", model, false)
	if c == nil {
		return
	}
	defer c.abandon()

	var inputText strings.Builder
	var inputBytes, items int64
	switch in := req["input"].(type) {
	case string:
		inputText.WriteString(in)
		inputBytes, items = int64(len(in)), 1
	case []any:
		for _, x := range in {
			switch v := x.(type) {
			case string:
				inputText.WriteString(v)
				inputText.WriteString("\n")
				inputBytes += int64(len(v))
			default: // Token 数组等其他形式：按 JSON 字节数计上界
				b, _ := json.Marshal(v)
				inputBytes += int64(len(b))
			}
			items++
		}
	}
	if items == 0 || inputBytes == 0 {
		c.reject(400, "input_required", "input 不能为空")
		return
	}
	// 上界：每个 Token 至少 1 字节，每条输入另加少量特殊 Token
	promptUpper := inputBytes + items*8
	var upper int64
	if c.mc != nil {
		upper = tokenCost(promptUpper, 0, c.mc.PriceIn, 0)
	}
	if !c.reserveFor(upper, "sync") {
		return
	}
	resp, t0 := c.tryChannels(func(ch *Candidate) (*http.Request, error) {
		req["model"] = ch.UpstreamModel
		return jsonRequest(c, ch, ch.url("/embeddings"), req)
	})
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	var parsed map[string]any
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(respBody))
		dec.UseNumber()
		err = dec.Decode(&parsed)
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
		return
	}
	o := outcome{status: "success", httpStatus: 200}
	if u, ok := parsed["usage"].(map[string]any); ok {
		o.pt, _ = num(u["prompt_tokens"])
		o.tt, _ = num(u["total_tokens"])
		if o.pt == 0 {
			o.pt = o.tt
		}
	}
	if o.pt > 0 {
		o.costStatus, o.usageSource = costActual, "upstream"
	} else {
		o.pt = estimateTokens(inputText.String())
		if o.pt == 0 {
			o.pt = promptUpper
		}
		o.costStatus, o.usageSource = costEstimated, "estimated"
		parsed["usage"] = map[string]any{"prompt_tokens": o.pt, "total_tokens": o.pt, "estimated": true}
	}
	o.tt = o.pt
	o.cost = c.priceTokens(o.pt, 0)
	parsed["model"] = c.model
	c.recordAttempt(c.used, "success", resp.StatusCode, o.pt, 0, o.cost, o.costStatus, "", t0)
	c.w.Header().Set("X-Usage-Source", o.usageSource)
	writeJSON(c.w, 200, parsed)
	c.finish(o)
}
