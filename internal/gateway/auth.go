package gateway

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxAPIKey
	ctxSource
	ctxReqID
)

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type APIKey struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Prefix         string   `json:"key_prefix"`
	Status         int      `json:"status"`
	QuotaLimit     int64    `json:"quota_limit"` // 微元；-1 表示无限额度
	UsedCost       int64    `json:"used_cost"`
	ReservedCost   int64    `json:"reserved_cost"`
	RPMLimit       int      `json:"rpm_limit"`
	MaxConcurrency int      `json:"max_concurrency"`
	Models         []string `json:"models"`
	ExpiresAt      int64    `json:"expires_at"`
	CreatedAt      int64    `json:"created_at"`
	LastUsedAt     int64    `json:"last_used_at"`
}

func (k *APIKey) allows(model string) bool {
	if len(k.Models) == 0 {
		return true
	}
	for _, m := range k.Models {
		if m == model {
			return true
		}
	}
	return false
}

const keyColumns = `id, name, key_prefix, status, quota_limit, used_cost, reserved_cost, rpm_limit, max_concurrency, models, expires_at, created_at, last_used_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanKey(row rowScanner) (*APIKey, error) {
	var k APIKey
	var models string
	if err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.Status, &k.QuotaLimit, &k.UsedCost, &k.ReservedCost, &k.RPMLimit, &k.MaxConcurrency, &models, &k.ExpiresAt, &k.CreatedAt, &k.LastUsedAt); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(models), &k.Models)
	if k.Models == nil {
		k.Models = []string{}
	}
	return &k, nil
}

func hashPassword(pw string) string {
	salt := randHex(16)
	const iter = 120000
	dk, _ := pbkdf2.Key(sha256.New, pw, []byte(salt), iter, 32)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", iter, salt, hex.EncodeToString(dk))
}

func checkPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	dk, err := pbkdf2.Key(sha256.New, pw, []byte(parts[2]), iter, 32)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(dk)), []byte(parts[3])) == 1
}

func (s *Server) ensureAdmin() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	user := envOr("ADMIN_USERNAME", "admin")
	pass := envOr("ADMIN_PASSWORD", "admin")
	_, err := s.db.Exec(`INSERT INTO users(username, password_hash, created_at) VALUES(?,?,?)`, user, hashPassword(pass), nowMs())
	return err
}

const sessionCookie = "relay_session"

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if err := decodeJSON(r, &req); err != nil {
		writeAdminError(w, 400, "请求格式错误")
		return
	}
	var id int64
	var hash string
	err := s.db.QueryRow(`SELECT id, password_hash FROM users WHERE username=?`, req.Username).Scan(&id, &hash)
	if err != nil || !checkPassword(hash, req.Password) {
		time.Sleep(300 * time.Millisecond)
		writeAdminError(w, 401, "用户名或密码错误")
		return
	}
	token := randHex(32)
	exp := time.Now().Add(7 * 24 * time.Hour)
	if _, err := s.db.Exec(`INSERT INTO sessions(token_hash, user_id, expires_at) VALUES(?,?,?)`, sha256Hex(token), id, exp.UnixMilli()); err != nil {
		writeAdminError(w, 500, "创建会话失败")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", Expires: exp, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]any{"user": User{ID: id, Username: req.Username}})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, sha256Hex(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) sessionUser(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	var u User
	err = s.db.QueryRow(`SELECT u.id, u.username FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`,
		sha256Hex(c.Value), nowMs()).Scan(&u.ID, &u.Username)
	if err != nil {
		return nil
	}
	return &u
}

func (s *Server) adminAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := s.sessionUser(r)
		if u == nil {
			writeAdminError(w, 401, "未登录或登录已过期")
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v\n%s", rec, debug.Stack())
				writeAdminError(w, 500, "服务内部错误")
			}
		}()
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUser, u)))
	})
}

func userFrom(r *http.Request) *User {
	u, _ := r.Context().Value(ctxUser).(*User)
	return u
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"user": userFrom(r)})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := decodeJSON(r, &req); err != nil || len(req.New) < 5 {
		writeAdminError(w, 400, "新密码至少 5 位")
		return
	}
	u := userFrom(r)
	var hash string
	s.db.QueryRow(`SELECT password_hash FROM users WHERE id=?`, u.ID).Scan(&hash)
	if !checkPassword(hash, req.Old) {
		writeAdminError(w, 400, "原密码不正确")
		return
	}
	s.db.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hashPassword(req.New), u.ID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) keyByHash(hash string) (*APIKey, error) {
	return scanKey(s.db.QueryRow(`SELECT `+keyColumns+` FROM api_keys WHERE key_hash=?`, hash))
}

func (s *Server) keyByID(id int64) (*APIKey, error) {
	return scanKey(s.db.QueryRow(`SELECT `+keyColumns+` FROM api_keys WHERE id=?`, id))
}

// gatewayAuth 解析调用身份：
//  1. Authorization: Bearer <网关 Key>（标准 API 调用）；
//  2. 管理员会话 + X-Console-Key-Id（控制台选择已有 Key 调用，密钥不经过浏览器）。
//
// 两种方式进入的是同一条网关处理链路：模型权限、额度、限流和请求日志完全一致。
func (s *Server) gatewayAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := newID("req_")
		w.Header().Set("X-Request-Id", reqID)
		sw := &statusWriter{ResponseWriter: w}
		var key *APIKey
		var err error
		source := "api"
		auth := r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(auth, "Bearer ") && strings.TrimSpace(auth[7:]) != "":
			key, err = s.keyByHash(sha256Hex(strings.TrimSpace(auth[7:])))
			if r.Header.Get("X-Console") != "" {
				source = "console"
			}
		case r.Header.Get("X-Console-Key-Id") != "":
			if s.sessionUser(r) == nil {
				writeGatewayError(sw, 401, "not_logged_in", "控制台登录已过期，请重新登录")
				return
			}
			id, _ := strconv.ParseInt(r.Header.Get("X-Console-Key-Id"), 10, 64)
			key, err = s.keyByID(id)
			source = "console"
		default:
			writeGatewayError(sw, 401, "missing_api_key", "缺少网关 Key：请在 Authorization 头中提供 Bearer Key")
			return
		}
		if err == sql.ErrNoRows {
			writeGatewayError(sw, 401, "invalid_api_key", "网关 Key 无效")
			return
		}
		if err != nil {
			writeGatewayError(sw, 500, "internal_error", "读取 Key 失败")
			return
		}
		if key.Status != 1 {
			writeGatewayError(sw, 401, "key_disabled", "该网关 Key 已被禁用")
			return
		}
		if key.ExpiresAt > 0 && key.ExpiresAt < nowMs() {
			writeGatewayError(sw, 401, "key_expired", "该网关 Key 已过期")
			return
		}
		ctx := context.WithValue(r.Context(), ctxAPIKey, key)
		ctx = context.WithValue(ctx, ctxSource, source)
		ctx = context.WithValue(ctx, ctxReqID, reqID)
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic [%s]: %v\n%s", reqID, rec, debug.Stack())
				if !sw.wrote {
					writeGatewayError(sw, 500, "internal_error", "网关内部错误")
				}
			}
		}()
		next(sw, r.WithContext(ctx))
	})
}

func keyFrom(r *http.Request) *APIKey {
	k, _ := r.Context().Value(ctxAPIKey).(*APIKey)
	return k
}

func reqIDFrom(r *http.Request) string {
	v, _ := r.Context().Value(ctxReqID).(string)
	return v
}

func sourceFrom(r *http.Request) string {
	v, _ := r.Context().Value(ctxSource).(string)
	return v
}
