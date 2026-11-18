package gateway

import (
	"database/sql"
	"errors"
	"fmt"
)

// 额度与记账规则（单位：微元，1 元 = 1,000,000 微元）
//
//   可用额度 = quota_limit − used_cost（已结算）− reserved_cost（在途预留 + 待核实预留）
//
// 1. 请求发往上游前，先按“费用上界”原子地预留额度（单条 UPDATE 带条件，SQLite 写事务串行，
//    同一笔余额不可能被两个并发请求同时占用）。预留不足则拒绝，不调用上游。
// 2. 请求结束后按实际用量结算：used_cost += 实际费用，reserved_cost −= 预留额。
//    结算以 reservations.status='held' 为前提，重复的完成通知不会重复记账。
// 3. 费用未知（已发往上游但超时/断连，没有任何用量信息）时，预留转为“待核实”(pending_review)，
//    继续占用额度，直到管理员核实，或超过期限后按预留上界自动结算。
// 4. 进程重启后，遗留的在途预留走 recoverOnStartup：未发往上游的直接释放，已发往上游的转待核实。

const (
	costActual    = "actual"    // 上游返回的实际用量
	costEstimated = "estimated" // 上游未返回用量，按内容估算
	costPending   = "pending"   // 费用未知，待核实
	costNone      = "none"      // 确认未产生用量，不计费
	costUnpriced  = "unpriced"  // 模型未定价（仅无限额度 Key 可调用），费用记 0
)

var errInsufficientQuota = errors.New("额度不足")

// reserve 为请求原子预留额度。返回失败时的可用额度。
func (s *Server) reserve(key *APIKey, reqID string, amount int64, kind string) (available int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE api_keys SET reserved_cost = reserved_cost + ?1
		WHERE id = ?2 AND (quota_limit < 0 OR used_cost + reserved_cost + ?1 <= quota_limit)`, amount, key.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var limit, used, reserved int64
		if err := tx.QueryRow(`SELECT quota_limit, used_cost, reserved_cost FROM api_keys WHERE id=?`, key.ID).Scan(&limit, &used, &reserved); err != nil {
			return 0, err
		}
		return max(limit-used-reserved, 0), errInsufficientQuota
	}
	now := nowMs()
	if _, err := tx.Exec(`INSERT INTO reservations(request_id, key_id, kind, amount, status, instance_id, created_at, updated_at)
		VALUES(?,?,?,?,'held',?,?,?)`, reqID, key.ID, kind, amount, s.instanceID, now, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE request_logs SET reserved=? WHERE request_id=?`, amount, reqID); err != nil {
		return 0, err
	}
	return 0, tx.Commit()
}

func (s *Server) markDispatched(reqID string) {
	s.db.Exec(`UPDATE reservations SET dispatched=1, updated_at=? WHERE request_id=? AND dispatched=0`, nowMs(), reqID)
}

// settleTx 结算一笔仍为 held 的预留。applied=false 表示该请求已结算过（幂等）。
func settleTx(tx *sql.Tx, reqID string, actual int64, costStatus, note string) (applied bool, err error) {
	var id, keyID, amount int64
	err = tx.QueryRow(`SELECT id, key_id, amount FROM reservations WHERE request_id=? AND status='held'`, reqID).Scan(&id, &keyID, &amount)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	status := "settled"
	if actual == 0 && costStatus == costNone {
		status = "released"
	}
	if _, err = tx.Exec(`UPDATE reservations SET status=?, actual_cost=?, cost_status=?, note=?, updated_at=? WHERE id=?`,
		status, actual, costStatus, note, nowMs(), id); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`UPDATE api_keys SET reserved_cost = reserved_cost - ?, used_cost = used_cost + ? WHERE id=?`, amount, actual, keyID); err != nil {
		return false, err
	}
	return true, nil
}

// pendingTx 把预留转为待核实：费用未知，额度继续占用。
func pendingTx(tx *sql.Tx, reqID, note string) (bool, error) {
	res, err := tx.Exec(`UPDATE reservations SET status='pending_review', cost_status='pending', note=?, updated_at=? WHERE request_id=? AND status='held'`,
		note, nowMs(), reqID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// settle 供异步任务结束时结算，并同步更新请求日志的费用。
func (s *Server) settle(reqID string, actual int64, costStatus, note string, units int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	applied, err := settleTx(tx, reqID, actual, costStatus, note)
	if err != nil {
		return err
	}
	if applied {
		if _, err := tx.Exec(`UPDATE request_logs SET cost=?, cost_status=?, units=CASE WHEN ?>0 THEN ? ELSE units END WHERE request_id=?`,
			actual, costStatus, units, units, reqID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) markPending(reqID, note string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	applied, err := pendingTx(tx, reqID, note)
	if err != nil {
		return err
	}
	if applied {
		if _, err := tx.Exec(`UPDATE request_logs SET cost_status='pending' WHERE request_id=?`, reqID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// resolvePending 是待核实预留的正式核实路径（控制台“账务核实”或超期自动结算）。
// action: settle 按 amount 结算；release 确认未产生费用并释放。
func (s *Server) resolvePending(id int64, action string, amount int64, costStatus, by, note string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var reqID string
	var keyID, reserved int64
	err = tx.QueryRow(`SELECT request_id, key_id, amount FROM reservations WHERE id=? AND status='pending_review'`, id).Scan(&reqID, &keyID, &reserved)
	if err == sql.ErrNoRows {
		return errors.New("该预留不存在或已处理")
	}
	if err != nil {
		return err
	}
	status := "settled"
	switch action {
	case "release":
		amount, status, costStatus = 0, "released", costNone
	case "settle":
		if amount < 0 {
			return errors.New("结算金额不能为负数")
		}
	default:
		return fmt.Errorf("未知操作: %s", action)
	}
	now := nowMs()
	if _, err = tx.Exec(`UPDATE reservations SET status=?, actual_cost=?, cost_status=?, resolved_by=?, note=CASE WHEN ?='' THEN note ELSE note || '；' || ? END, updated_at=? WHERE id=?`,
		status, amount, costStatus, by, note, note, now, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE api_keys SET reserved_cost = reserved_cost - ?, used_cost = used_cost + ? WHERE id=?`, reserved, amount, keyID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE request_logs SET cost=?, cost_status=? WHERE request_id=?`, amount, costStatus, reqID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) autoSettleExpiredPending() (int, error) {
	cutoff := nowMs() - s.cfg.PendingReviewTTL.Milliseconds()
	rows, err := s.db.Query(`SELECT id, amount FROM reservations WHERE status='pending_review' AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	type item struct{ id, amount int64 }
	var items []item
	for rows.Next() {
		var it item
		rows.Scan(&it.id, &it.amount)
		items = append(items, it)
	}
	rows.Close()
	n := 0
	for _, it := range items {
		if err := s.resolvePending(it.id, "settle", it.amount, costEstimated, "system", "超过核实期限，按预留上界自动结算"); err == nil {
			n++
		}
	}
	return n, nil
}

// recoverOnStartup 在开始对外服务前执行。此时所有 held 的同步预留和 in_progress 的日志
// 都属于已经退出的进程：
//   - 尚未发往上游（dispatched=0）：确认没有产生用量，释放预留；
//   - 已发往上游（dispatched=1）：费用未知，转为待核实，额度继续占用，等待核实或超期自动结算。
//
// 已结算的费用在各自的事务中早已落盘，不受重启影响。
func (s *Server) recoverOnStartup() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT request_id, dispatched FROM reservations WHERE status='held' AND kind='sync'`)
	if err != nil {
		return err
	}
	type item struct {
		reqID      string
		dispatched int
	}
	var items []item
	for rows.Next() {
		var it item
		rows.Scan(&it.reqID, &it.dispatched)
		items = append(items, it)
	}
	rows.Close()
	now := nowMs()
	for _, it := range items {
		if it.dispatched == 0 {
			if _, err := settleTx(tx, it.reqID, 0, costNone, "服务重启恢复：请求尚未发往上游，释放预留"); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE request_logs SET status='interrupted', cost=0, cost_status='none', error_code='gateway_restarted',
				error_message='网关重启时请求尚未发往上游，未产生费用', finished_at=? WHERE request_id=?`, now, it.reqID); err != nil {
				return err
			}
		} else {
			if _, err := pendingTx(tx, it.reqID, "服务重启恢复：请求已发往上游但未收到结果，费用待核实"); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE request_logs SET status='interrupted', cost_status='pending', error_code='gateway_restarted',
				error_message='网关重启时请求已发往上游，费用待核实', finished_at=? WHERE request_id=?`, now, it.reqID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE request_logs SET status='interrupted', error_code='gateway_restarted',
		error_message='网关重启导致请求中断', finished_at=? WHERE status='in_progress'`, now); err != nil {
		return err
	}
	// 以预留台账为准校正各 Key 的在途占用，防止异常退出造成的漂移。
	if _, err := tx.Exec(`UPDATE api_keys SET reserved_cost = COALESCE((SELECT SUM(amount) FROM reservations r
		WHERE r.key_id = api_keys.id AND r.status IN ('held','pending_review')), 0)`); err != nil {
		return err
	}
	return tx.Commit()
}
