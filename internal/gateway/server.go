package gateway

import (
	"bufio"
	"context"
	"database/sql"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Addr              string
	SQLitePath        string
	DataDir           string        // 媒体文件目录
	UpstreamTimeout   time.Duration // 等待上游响应头的最长时间
	StreamIdleTimeout time.Duration // 流式响应两次数据之间的最长间隔
	DefaultMaxTokens  int64         // 请求未指定 max_tokens 且模型未配置默认值时使用的受控上限
	MaxAttempts       int           // 单次请求最多尝试的渠道数
	PendingReviewTTL  time.Duration // 待核实预留自动按上界结算的期限
	TaskPollInterval  time.Duration
	WebFS             fs.FS // 前端静态文件，可为空
}

func ConfigFromEnv() Config {
	c := Config{
		Addr:              envOr("ADDR", ":3000"),
		SQLitePath:        envOr("SQLITE_PATH", "data/gateway.sqlite"),
		UpstreamTimeout:   envDur("UPSTREAM_TIMEOUT_SECONDS", 300),
		StreamIdleTimeout: envDur("STREAM_IDLE_TIMEOUT_SECONDS", 120),
		DefaultMaxTokens:  int64(envInt("DEFAULT_MAX_TOKENS", 4096)),
		MaxAttempts:       envInt("MAX_ATTEMPTS", 3),
		PendingReviewTTL:  envDur("PENDING_REVIEW_TTL_SECONDS", 24*3600),
		TaskPollInterval:  envDur("TASK_POLL_INTERVAL_SECONDS", 5),
	}
	c.DataDir = os.Getenv("DATA_DIR")
	return c
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envDur(k string, defSeconds int) time.Duration {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil && v > 0 {
		return time.Duration(v * float64(time.Second))
	}
	return time.Duration(defSeconds) * time.Second
}

type Server struct {
	cfg        Config
	db         *sql.DB
	instanceID string
	httpc      *http.Client // 调用上游：只限制响应头等待时间，不限制总时长（流式）
	limiter    *limiter
	mux        *http.ServeMux
	bg         context.Context
	bgCancel   context.CancelFunc
	wg         sync.WaitGroup
	voiceCache sync.Map // channelID -> *voiceCacheEntry
	taskKick   chan struct{}
}

func New(cfg Config) (*Server, error) {
	if cfg.DataDir == "" {
		base := strings.TrimSuffix(filepath.Base(cfg.SQLitePath), filepath.Ext(cfg.SQLitePath))
		cfg.DataDir = filepath.Join(filepath.Dir(cfg.SQLitePath), base+"_media")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.DefaultMaxTokens <= 0 {
		cfg.DefaultMaxTokens = 4096
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 300 * time.Second
	}
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = 120 * time.Second
	}
	if cfg.PendingReviewTTL <= 0 {
		cfg.PendingReviewTTL = 24 * time.Hour
	}
	if cfg.TaskPollInterval <= 0 {
		cfg.TaskPollInterval = 5 * time.Second
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	db, err := openDB(cfg.SQLitePath)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.UpstreamTimeout,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	bg, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:        cfg,
		db:         db,
		instanceID: newID("inst_"),
		httpc:      &http.Client{Transport: tr},
		limiter:    newLimiter(),
		bg:         bg,
		bgCancel:   cancel,
		taskKick:   make(chan struct{}, 1),
	}
	if err := s.ensureAdmin(); err != nil {
		return nil, err
	}
	if err := s.seedModels(); err != nil {
		return nil, err
	}
	// 正式恢复路径：处理上一个进程遗留的在途预留。
	if err := s.recoverOnStartup(); err != nil {
		return nil, err
	}
	s.bootstrapFromEnv()
	s.routes()
	s.wg.Add(2)
	go s.taskLoop()
	go s.housekeepingLoop()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) DB() *sql.DB { return s.db }

// Close 停止后台任务并关闭数据库。HTTP 的优雅退出由调用方通过 http.Server.Shutdown 负责。
func (s *Server) Close() {
	s.bgCancel()
	s.wg.Wait()
	s.db.Close()
}

func (s *Server) routes() {
	mux := http.NewServeMux()
	s.mux = mux

	// —— 网关接口（Bearer 网关 Key，或控制台会话 + X-Console-Key-Id）——
	gw := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.gatewayAuth(h)) }
	gw("GET /v1/models", s.handleListModels)
	gw("POST /v1/chat/completions", s.handleChat)
	gw("POST /v1/embeddings", s.handleEmbeddings)
	gw("POST /v1/video/generations", s.handleVideoCreate)
	gw("GET /v1/video/generations/{id}", s.handleTaskGet)
	gw("POST /v1/audio/speech", s.handleSpeech)
	gw("POST /v1/audio/speech/async", s.handleSpeechAsync)
	gw("GET /v1/audio/voices", s.handleVoices)
	gw("POST /v1/audio/voices/clone", s.handleVoiceClone)
	gw("POST /v1/audio/voices/design", s.handleVoiceDesign)
	gw("POST /v1/music/generations", s.handleMusicCreate)
	gw("GET /v1/tasks", s.handleTaskList)
	gw("GET /v1/tasks/{id}", s.handleTaskGet)
	gw("GET /v1/tasks/{id}/content", s.handleTaskContent)

	// —— 控制台管理接口（管理员会话）——
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	ad := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.adminAuth(h)) }
	ad("GET /api/me", s.handleMe)
	ad("PUT /api/password", s.handleChangePassword)
	ad("GET /api/dashboard", s.handleDashboard)
	ad("GET /api/channels", s.handleChannelList)
	ad("POST /api/channels", s.handleChannelSave)
	ad("PUT /api/channels/{id}", s.handleChannelSave)
	ad("DELETE /api/channels/{id}", s.handleChannelDelete)
	ad("POST /api/channels/fetch-models", s.handleChannelFetchModels)
	ad("POST /api/channels/{id}/test", s.handleChannelTest)
	ad("GET /api/models", s.handleModelList)
	ad("PUT /api/models", s.handleModelSave)
	ad("DELETE /api/models", s.handleModelDelete)
	ad("PUT /api/favorites", s.handleFavorite)
	ad("GET /api/keys", s.handleKeyList)
	ad("POST /api/keys", s.handleKeySave)
	ad("PUT /api/keys/{id}", s.handleKeySave)
	ad("DELETE /api/keys/{id}", s.handleKeyDelete)
	ad("GET /api/logs", s.handleLogList)
	ad("GET /api/logs/export", s.handleLogExport)
	ad("GET /api/logs/{request_id}", s.handleLogDetail)
	ad("GET /api/usage", s.handleUsage)
	ad("GET /api/reservations", s.handleReservationList)
	ad("POST /api/reservations/{id}/resolve", s.handleReservationResolve)
	ad("GET /api/tasks", s.handleAdminTaskList)
	ad("GET /api/tasks/{id}", s.handleAdminTaskGet)
	ad("GET /api/tasks/{id}/content", s.handleAdminTaskContent)
	ad("DELETE /api/tasks/{id}", s.handleAdminTaskDelete)
	ad("GET /api/voices", s.handleAdminVoiceList)
	ad("GET /api/voices/{id}/preview", s.handleAdminVoicePreview)
	ad("DELETE /api/voices/{id}", s.handleAdminVoiceDelete)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok", "instance": s.instanceID})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeAdminError(w, 404, "接口不存在") })
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeGatewayError(w, 404, "not_found", "接口不存在: "+r.Method+" "+r.URL.Path)
	})
	mux.HandleFunc("/", s.handleWeb)
}

// handleWeb 提供内嵌的 React 控制台；未知路径回退到 index.html（前端路由）。
func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	if s.cfg.WebFS == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(200)
		w.Write([]byte("模型网关已启动。控制台前端尚未构建：请在 web/ 目录执行 npm run build 后重新编译。\n"))
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	if f, err := s.cfg.WebFS.Open(p); err == nil {
		st, _ := f.Stat()
		f.Close()
		if st != nil && !st.IsDir() {
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, s.cfg.WebFS, p)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, s.cfg.WebFS, "index.html")
}

// statusWriter 记录是否已写出响应头，并透传 Flush。
type statusWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *statusWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) housekeepingLoop() {
	defer s.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.bg.Done():
			return
		case <-t.C:
			if n, err := s.autoSettleExpiredPending(); err != nil {
				log.Printf("自动结算待核实预留失败: %v", err)
			} else if n > 0 {
				log.Printf("已按预留上界自动结算 %d 笔超期待核实预留", n)
			}
			s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, nowMs())
			s.limiter.gc()
		}
	}
}
