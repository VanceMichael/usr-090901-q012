// Package store 负责 PostgreSQL 持久化。原始回执、当前工作单与期限重算
// 依据在同一个事务中写入，任何一步失败都会整体回滚。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.com/usr/090901/q012/internal/domain"
)

// Store 包装连接池。
type Store struct {
	pool *pgxpool.Pool
}

// ReceiptRecord 是已持久化的回执（含处理结果）。
type ReceiptRecord struct {
	ReceiptID     string    `json:"receipt_id"`
	CaseID        string    `json:"case_id"`
	Type          string    `json:"type"`
	ActorRole     string    `json:"actor_role"`
	OccurredAt    time.Time `json:"occurred_at"`
	RiskLevel     string    `json:"risk_level"`
	MaterialCodes []string  `json:"material_codes"`
	Status        string    `json:"status"`
	RejectReason  string    `json:"reject_reason,omitempty"`
	ReceivedAt    time.Time `json:"received_at"`
}

// ApplyTxResult 是 ApplyReceipt 的结果。
type ApplyTxResult struct {
	Duplicate bool
	Stored    *ReceiptRecord // Duplicate 为 true 时返回已存在的回执
	Applied   *domain.ApplyResult
}

// ErrInjectedRollback 是故障注入触发的事务回滚。
var ErrInjectedRollback = errors.New("故障注入：事务已回滚")

// Connect 建立连接池并验证连通性。
func Connect(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PostgreSQL 不可达: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close 关闭连接池。
func (s *Store) Close() { s.pool.Close() }

// Migrate 幂等建表。
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("迁移失败: %w", err)
	}
	return nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS receipts (
  receipt_id     TEXT PRIMARY KEY,
  case_id        TEXT NOT NULL,
  type           TEXT NOT NULL,
  actor_role     TEXT NOT NULL,
  occurred_at    TIMESTAMPTZ NOT NULL,
  risk_level     TEXT NOT NULL,
  material_codes JSONB NOT NULL,
  status         TEXT NOT NULL,
  reject_reason  TEXT,
  received_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS cases (
  case_id          TEXT PRIMARY KEY,
  risk_level       TEXT NOT NULL,
  stage            TEXT NOT NULL,
  decision         TEXT,
  amount_cents     BIGINT,
  currency         TEXT,
  customer_contact TEXT,
  materials        JSONB NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL,
  updated_at       TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS worklist (
  case_id            TEXT PRIMARY KEY REFERENCES cases(case_id),
  stage              TEXT NOT NULL,
  next_action        TEXT NOT NULL,
  due_at             TIMESTAMPTZ NOT NULL,
  responsible_org    TEXT NOT NULL,
  blocking_materials JSONB NOT NULL,
  notices            JSONB NOT NULL,
  updated_at         TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS recalc_log (
  id               BIGSERIAL PRIMARY KEY,
  case_id          TEXT NOT NULL,
  receipt_id       TEXT NOT NULL,
  stage            TEXT NOT NULL,
  sla_hours        INTEGER NOT NULL,
  base_time        TIMESTAMPTZ NOT NULL,
  computed_due     TIMESTAMPTZ NOT NULL,
  skipped_days     JSONB NOT NULL,
  segments         JSONB NOT NULL,
  holiday_rollover BOOLEAN NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_worklist_due ON worklist (due_at);
CREATE INDEX IF NOT EXISTS idx_receipts_case ON receipts (case_id);
CREATE INDEX IF NOT EXISTS idx_recalc_case ON recalc_log (case_id);
`

// ApplyReceipt 在单个事务中：写入原始回执、应用状态机变更（案件、工作单、
// 期限重算依据）。injectRollback 为 true 时在全部写入之后、提交之前强制
// 回滚（仅用于验收原子性）。回执去重以 receipt_id 主键为准：重复送达直接
// 返回已存储的处理结果，不产生第二项工作。
func (s *Store) ApplyReceipt(
	ctx context.Context,
	rec domain.Receipt,
	injectRollback bool,
	applyFn func(existing *domain.Case) (*domain.ApplyResult, error),
) (*ApplyTxResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	mats, _ := json.Marshal(domain.SortedSet(rec.MaterialCodes))
	tag, err := tx.Exec(ctx, `
		INSERT INTO receipts (receipt_id, case_id, type, actor_role, occurred_at, risk_level, material_codes, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,'received')
		ON CONFLICT (receipt_id) DO NOTHING`,
		rec.ReceiptID, rec.CaseID, rec.Type, rec.ActorRole, rec.OccurredAt.UTC(), rec.RiskLevel, string(mats))
	if err != nil {
		return nil, fmt.Errorf("写入回执: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 同一 receipt_id 已存在：幂等返回，不产生第二项工作。
		_ = tx.Rollback(ctx)
		stored, err := s.GetReceipt(ctx, rec.ReceiptID)
		if err != nil {
			return nil, err
		}
		return &ApplyTxResult{Duplicate: true, Stored: stored}, nil
	}

	existing, err := getCaseForUpdate(ctx, tx, rec.CaseID)
	if err != nil {
		return nil, err
	}
	res, err := applyFn(existing)
	if err != nil {
		return nil, err
	}

	var rejectReason any
	if res.RejectReason != "" {
		rejectReason = res.RejectReason
	}
	if _, err := tx.Exec(ctx, `UPDATE receipts SET status=$2, reject_reason=$3 WHERE receipt_id=$1`,
		rec.ReceiptID, res.ReceiptStatus, rejectReason); err != nil {
		return nil, fmt.Errorf("更新回执状态: %w", err)
	}

	if res.Case != nil {
		if err := upsertCase(ctx, tx, res.Case, rec.OccurredAt); err != nil {
			return nil, err
		}
	}
	switch {
	case res.WorklistDeleted:
		if _, err := tx.Exec(ctx, `DELETE FROM worklist WHERE case_id=$1`, rec.CaseID); err != nil {
			return nil, fmt.Errorf("删除工作单: %w", err)
		}
	case res.Worklist != nil:
		if err := upsertWorklist(ctx, tx, res.Worklist); err != nil {
			return nil, err
		}
	}
	if res.Recalc != nil {
		if err := insertRecalc(ctx, tx, res.Recalc); err != nil {
			return nil, err
		}
	}

	if injectRollback {
		return nil, ErrInjectedRollback
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交事务: %w", err)
	}
	return &ApplyTxResult{Applied: res}, nil
}

func getCaseForUpdate(ctx context.Context, tx pgx.Tx, caseID string) (*domain.Case, error) {
	row := tx.QueryRow(ctx, `
		SELECT case_id, risk_level, stage, COALESCE(decision,''), amount_cents,
		       COALESCE(currency,''), COALESCE(customer_contact,''), materials
		FROM cases WHERE case_id=$1 FOR UPDATE`, caseID)
	c, err := scanCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func scanCase(row pgx.Row) (*domain.Case, error) {
	var c domain.Case
	var mats []byte
	err := row.Scan(&c.CaseID, &c.RiskLevel, &c.Stage, &c.Decision, &c.AmountCents,
		&c.Currency, &c.CustomerContact, &mats)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(mats, &c.Materials); err != nil {
		return nil, fmt.Errorf("解析材料列表: %w", err)
	}
	return &c, nil
}

func upsertCase(ctx context.Context, tx pgx.Tx, c *domain.Case, now time.Time) error {
	mats, _ := json.Marshal(domain.SortedSet(c.Materials))
	var decision any
	if c.Decision != "" {
		decision = c.Decision
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO cases (case_id, risk_level, stage, decision, amount_cents, currency, customer_contact, materials, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$9)
		ON CONFLICT (case_id) DO UPDATE SET
		  risk_level=EXCLUDED.risk_level, stage=EXCLUDED.stage, decision=EXCLUDED.decision,
		  amount_cents=EXCLUDED.amount_cents, currency=EXCLUDED.currency,
		  customer_contact=EXCLUDED.customer_contact, materials=EXCLUDED.materials, updated_at=EXCLUDED.updated_at`,
		c.CaseID, c.RiskLevel, c.Stage, decision, c.AmountCents, c.Currency, c.CustomerContact, string(mats), now.UTC())
	if err != nil {
		return fmt.Errorf("写入案件: %w", err)
	}
	return nil
}

func upsertWorklist(ctx context.Context, tx pgx.Tx, w *domain.WorklistItem) error {
	blocking, _ := json.Marshal(w.BlockingMaterials)
	notices, _ := json.Marshal(w.Notices)
	_, err := tx.Exec(ctx, `
		INSERT INTO worklist (case_id, stage, next_action, due_at, responsible_org, blocking_materials, notices, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,now())
		ON CONFLICT (case_id) DO UPDATE SET
		  stage=EXCLUDED.stage, next_action=EXCLUDED.next_action, due_at=EXCLUDED.due_at,
		  responsible_org=EXCLUDED.responsible_org, blocking_materials=EXCLUDED.blocking_materials,
		  notices=EXCLUDED.notices, updated_at=EXCLUDED.updated_at`,
		w.CaseID, w.Stage, w.NextAction, w.DueAt.UTC(), w.ResponsibleOrg, string(blocking), string(notices))
	if err != nil {
		return fmt.Errorf("写入工作单: %w", err)
	}
	return nil
}

func insertRecalc(ctx context.Context, tx pgx.Tx, b *domain.RecalcBasis) error {
	skipped, _ := json.Marshal(b.SkippedDays)
	segments, _ := json.Marshal(b.Segments)
	_, err := tx.Exec(ctx, `
		INSERT INTO recalc_log (case_id, receipt_id, stage, sla_hours, base_time, computed_due, skipped_days, segments, holiday_rollover)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9)`,
		b.CaseID, b.ReceiptID, b.Stage, b.SLAHours, b.BaseTime.UTC(), b.ComputedDue.UTC(),
		string(skipped), string(segments), b.HolidayRollover)
	if err != nil {
		return fmt.Errorf("写入期限重算依据: %w", err)
	}
	return nil
}

// GetReceipt 读取单条回执。
func (s *Store) GetReceipt(ctx context.Context, receiptID string) (*ReceiptRecord, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT receipt_id, case_id, type, actor_role, occurred_at, risk_level, material_codes,
		       status, COALESCE(reject_reason,''), received_at
		FROM receipts WHERE receipt_id=$1`, receiptID)
	r, err := scanReceipt(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func scanReceipt(row pgx.Row) (*ReceiptRecord, error) {
	var r ReceiptRecord
	var mats []byte
	err := row.Scan(&r.ReceiptID, &r.CaseID, &r.Type, &r.ActorRole, &r.OccurredAt, &r.RiskLevel,
		&mats, &r.Status, &r.RejectReason, &r.ReceivedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(mats, &r.MaterialCodes); err != nil {
		return nil, err
	}
	return &r, nil
}

// CaseBundle 是案件及其关联数据。
type CaseBundle struct {
	Case     *domain.Case
	Worklist *domain.WorklistItem
	Receipts []ReceiptRecord
}

// GetCase 读取案件、当前工作单与全部回执。
func (s *Store) GetCase(ctx context.Context, caseID string) (*CaseBundle, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT case_id, risk_level, stage, COALESCE(decision,''), amount_cents,
		       COALESCE(currency,''), COALESCE(customer_contact,''), materials
		FROM cases WHERE case_id=$1`, caseID)
	c, err := scanCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b := &CaseBundle{Case: c}

	wrow := s.pool.QueryRow(ctx, `
		SELECT case_id, stage, next_action, due_at, responsible_org, blocking_materials, notices
		FROM worklist WHERE case_id=$1`, caseID)
	w, err := scanWorklist(wrow)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if w != nil {
		w.RiskLevel = c.RiskLevel
	}
	b.Worklist = w

	rows, err := s.pool.Query(ctx, `
		SELECT receipt_id, case_id, type, actor_role, occurred_at, risk_level, material_codes,
		       status, COALESCE(reject_reason,''), received_at
		FROM receipts WHERE case_id=$1 ORDER BY received_at, receipt_id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, err
		}
		b.Receipts = append(b.Receipts, *r)
	}
	return b, rows.Err()
}

func scanWorklist(row pgx.Row) (*domain.WorklistItem, error) {
	var w domain.WorklistItem
	var blocking, notices []byte
	err := row.Scan(&w.CaseID, &w.Stage, &w.NextAction, &w.DueAt, &w.ResponsibleOrg, &blocking, &notices)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(blocking, &w.BlockingMaterials); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(notices, &w.Notices); err != nil {
		return nil, err
	}
	return &w, nil
}

// ListWorklist 返回按紧迫度排序的开放工作单（due_at 升序，风险高者优先，
// 案件号字典序兜底，保证重启后顺序稳定）。org 为空表示全部机构。
func (s *Store) ListWorklist(ctx context.Context, org string, limit int) ([]domain.WorklistItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.case_id, c.risk_level, w.stage, w.next_action, w.due_at, w.responsible_org,
		       w.blocking_materials, w.notices
		FROM worklist w JOIN cases c ON c.case_id = w.case_id
		WHERE ($1 = '' OR w.responsible_org = $1)
		ORDER BY w.due_at ASC,
		         CASE c.risk_level WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END,
		         w.case_id ASC
		LIMIT $2`, org, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.WorklistItem
	for rows.Next() {
		var w domain.WorklistItem
		var blocking, notices []byte
		if err := rows.Scan(&w.CaseID, &w.RiskLevel, &w.Stage, &w.NextAction, &w.DueAt,
			&w.ResponsibleOrg, &blocking, &notices); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(blocking, &w.BlockingMaterials); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(notices, &w.Notices); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// OrgBacklog 是机构积压汇总。
type OrgBacklog struct {
	Org          string         `json:"org"`
	Open         int            `json:"open"`
	Overdue      int            `json:"overdue"`
	ByStage      map[string]int `json:"by_stage"`
	NearestDueAt *time.Time     `json:"nearest_due_at,omitempty"`
}

// Backlog 按负责机构汇总开放工作单。
func (s *Store) Backlog(ctx context.Context, now time.Time) ([]OrgBacklog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT responsible_org, stage, count(*), count(*) FILTER (WHERE due_at < $1), min(due_at)
		FROM worklist GROUP BY responsible_org, stage
		ORDER BY responsible_org, stage`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byOrg := map[string]*OrgBacklog{}
	var order []string
	for rows.Next() {
		var org, stage string
		var open, overdue int
		var minDue time.Time
		if err := rows.Scan(&org, &stage, &open, &overdue, &minDue); err != nil {
			return nil, err
		}
		b, ok := byOrg[org]
		if !ok {
			b = &OrgBacklog{Org: org, ByStage: map[string]int{}}
			byOrg[org] = b
			order = append(order, org)
		}
		b.Open += open
		b.Overdue += overdue
		b.ByStage[stage] += open
		if b.NearestDueAt == nil || minDue.Before(*b.NearestDueAt) {
			t := minDue
			b.NearestDueAt = &t
		}
	}
	out := make([]OrgBacklog, 0, len(order))
	for _, org := range order {
		out = append(out, *byOrg[org])
	}
	return out, rows.Err()
}

// RecalcHistory 返回案件的全部期限重算依据（按时间升序）。
func (s *Store) RecalcHistory(ctx context.Context, caseID string) ([]domain.RecalcBasis, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT case_id, receipt_id, stage, sla_hours, base_time, computed_due, skipped_days, segments, holiday_rollover
		FROM recalc_log WHERE case_id=$1 ORDER BY id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RecalcBasis
	for rows.Next() {
		var b domain.RecalcBasis
		var skipped, segments []byte
		if err := rows.Scan(&b.CaseID, &b.ReceiptID, &b.Stage, &b.SLAHours, &b.BaseTime,
			&b.ComputedDue, &skipped, &segments, &b.HolidayRollover); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(skipped, &b.SkippedDays); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(segments, &b.Segments); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
