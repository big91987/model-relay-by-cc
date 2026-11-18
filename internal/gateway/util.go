package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func nowMs() int64 { return time.Now().UnixMilli() }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newID(prefix string) string { return prefix + randHex(12) }

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeGatewayError 输出 OpenAI 风格的错误体，并附带请求标识。
func writeGatewayError(w http.ResponseWriter, status int, code, msg string) {
	typ := "invalid_request_error"
	switch {
	case status == 401:
		typ = "authentication_error"
	case status == 402:
		typ = "insufficient_quota"
	case status == 403:
		typ = "permission_error"
	case status == 429:
		typ = "rate_limit_error"
	case status >= 500:
		typ = "upstream_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "code": code, "request_id": w.Header().Get("X-Request-Id"),
	}})
}

func writeAdminError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

var errBodyTooLarge = errors.New("请求体过大")

func readBody(r *http.Request, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errBodyTooLarge
	}
	return b, nil
}

func decodeJSON(r *http.Request, v any) error {
	b, err := readBody(r, 4<<20)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return errors.New("请求体为空")
	}
	return json.Unmarshal(b, v)
}

func decodeMap(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("请求体必须是 JSON 对象")
	}
	return m, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(t), true
	case int:
		return int64(t), true
	case int64:
		return t, true
	}
	return 0, false
}

func fnum(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

func qInt(r *http.Request, name string, def int64) int64 {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return i
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// estimateTokens 是在上游没有返回用量时使用的启发式估算（不是上界）：
// ASCII 约 4 字符一个 Token，其他字符（中日韩等）约 1 字符 0.7 个 Token。
func estimateTokens(s string) int64 {
	var ascii, other int64
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	n := (ascii+3)/4 + (other*7+9)/10
	if n == 0 && len(s) > 0 {
		n = 1
	}
	return n
}

// tokenCost 计算费用（微元）。价格单位：微元 / 百万 Token（即 元/百万Token × 1e6）。向上取整。
func tokenCost(promptTokens, completionTokens, priceIn, priceOut int64) int64 {
	total := promptTokens*priceIn + completionTokens*priceOut
	if total <= 0 {
		return 0
	}
	return (total + 999_999) / 1_000_000
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}
