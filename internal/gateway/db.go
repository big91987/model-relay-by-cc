package gateway

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// migrations 按顺序执行，已执行的版本记录在 schema_migrations 中。
// 新功能只追加新的条目，不修改已有条目，保证旧库可以原地升级。
var migrations = []string{
	// 1: 首版
	`
CREATE TABLE users(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE sessions(
  token_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE channels(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  base_url TEXT NOT NULL,
  api_key TEXT NOT NULL DEFAULT '',
  group_id TEXT NOT NULL DEFAULT '',
  status INTEGER NOT NULL DEFAULT 1,
  priority INTEGER NOT NULL DEFAULT 0,
  weight INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE channel_models(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id INTEGER NOT NULL,
  model TEXT NOT NULL,
  upstream_model TEXT NOT NULL,
  UNIQUE(channel_id, model)
);
CREATE INDEX idx_channel_models_model ON channel_models(model);
CREATE TABLE models(
  name TEXT PRIMARY KEY,
  kind TEXT NOT NULL DEFAULT 'chat',
  billing_mode TEXT NOT NULL DEFAULT 'token',
  price_in INTEGER NOT NULL DEFAULT 0,
  price_out INTEGER NOT NULL DEFAULT 0,
  price_unit INTEGER NOT NULL DEFAULT 0,
  priced INTEGER NOT NULL DEFAULT 0,
  max_output_tokens INTEGER NOT NULL DEFAULT 0,
  default_max_tokens INTEGER NOT NULL DEFAULT 0,
  supports_temperature INTEGER NOT NULL DEFAULT 1,
  temperature_max REAL NOT NULL DEFAULT 2,
  note TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);
CREATE TABLE api_keys(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  key_prefix TEXT NOT NULL,
  status INTEGER NOT NULL DEFAULT 1,
  quota_limit INTEGER NOT NULL DEFAULT -1,
  used_cost INTEGER NOT NULL DEFAULT 0,
  models TEXT NOT NULL DEFAULT '[]',
  expires_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE request_logs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL UNIQUE,
  key_id INTEGER NOT NULL DEFAULT 0,
  key_name TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT 'api',
  endpoint TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  channel_id INTEGER NOT NULL DEFAULT 0,
  channel_name TEXT NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT '',
  stream INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'in_progress',
  http_status INTEGER NOT NULL DEFAULT 0,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  units INTEGER NOT NULL DEFAULT 0,
  cost INTEGER NOT NULL DEFAULT 0,
  cost_status TEXT NOT NULL DEFAULT 'none',
  usage_source TEXT NOT NULL DEFAULT 'none',
  attempts INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  first_token_ms INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_logs_created ON request_logs(created_at);
CREATE INDEX idx_logs_key ON request_logs(key_id, created_at);
CREATE INDEX idx_logs_model ON request_logs(model, created_at);
CREATE TABLE settings(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`,
	// 2: 按 Key 限流（RPM）与在途请求数上限
	`
ALTER TABLE api_keys ADD COLUMN rpm_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN max_concurrency INTEGER NOT NULL DEFAULT 0;
`,
	// 3: 并发额度与记账：预留台账、上游尝试明细
	`
ALTER TABLE api_keys ADD COLUMN reserved_cost INTEGER NOT NULL DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN reserved INTEGER NOT NULL DEFAULT 0;
CREATE TABLE reservations(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL UNIQUE,
  key_id INTEGER NOT NULL,
  kind TEXT NOT NULL DEFAULT 'sync',
  amount INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'held',
  actual_cost INTEGER NOT NULL DEFAULT 0,
  cost_status TEXT NOT NULL DEFAULT '',
  dispatched INTEGER NOT NULL DEFAULT 0,
  instance_id TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  resolved_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_reservations_status ON reservations(status);
CREATE INDEX idx_reservations_key ON reservations(key_id, status);
CREATE TABLE upstream_attempts(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL,
  attempt INTEGER NOT NULL,
  channel_id INTEGER NOT NULL DEFAULT 0,
  channel_name TEXT NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  http_status INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost INTEGER NOT NULL DEFAULT 0,
  cost_status TEXT NOT NULL DEFAULT 'none',
  error TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_attempts_req ON upstream_attempts(request_id);
`,
	// 4: 模型收藏
	`
CREATE TABLE favorites(
  user_id INTEGER NOT NULL,
  model TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(user_id, model)
);
`,
	// 5: 媒体任务（视频、长文本配音、音乐）与自定义音色
	`
CREATE TABLE media_tasks(
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL,
  key_id INTEGER NOT NULL DEFAULT 0,
  key_name TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  channel_id INTEGER NOT NULL DEFAULT 0,
  model TEXT NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT '',
  mode TEXT NOT NULL DEFAULT '',
  prompt TEXT NOT NULL DEFAULT '',
  params TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL DEFAULT 'queued',
  upstream_status TEXT NOT NULL DEFAULT '',
  upstream_task_id TEXT NOT NULL DEFAULT '',
  upstream_file_id TEXT NOT NULL DEFAULT '',
  result_url TEXT NOT NULL DEFAULT '',
  file_path TEXT NOT NULL DEFAULT '',
  file_mime TEXT NOT NULL DEFAULT '',
  file_size INTEGER NOT NULL DEFAULT 0,
  units INTEGER NOT NULL DEFAULT 0,
  cost INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_tasks_kind ON media_tasks(kind, created_at);
CREATE INDEX idx_tasks_status ON media_tasks(status);
CREATE TABLE voices(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  voice_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT 'minimax',
  channel_id INTEGER NOT NULL DEFAULT 0,
  description TEXT NOT NULL DEFAULT '',
  preview_path TEXT NOT NULL DEFAULT '',
  preview_mime TEXT NOT NULL DEFAULT '',
  consent_note TEXT NOT NULL DEFAULT '',
  key_id INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
`,
}

func openDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var cur int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&cur); err != nil {
		return err
	}
	for i := cur; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("迁移 %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`, i+1, nowMs()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
