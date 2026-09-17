// Package httpapi 提供协同止付工作单的 HTTP 接口。
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"example.com/usr/090901/q012/internal/config"
	"example.com/usr/090901/q012/internal/domain"
	"example.com/usr/090901/q012/internal/store"
)

// Server 持有路由与依赖。
type Server struct {
	fixtures       *config.Fixtures
	store          *store.Store
	faultInjection bool
	mux            *http.ServeMux
}

// New 构建 HTTP 服务。faultInjection 开启后，POST /receipts 识别
// X-Fault-Inject: rollback 头并在写入后强制回滚（仅供验收使用）。
func New(f *config.Fixtures, st *store.Store, faultInjection bool) *Server {
	s := &Server{fixtures: f, store: st, faultInjection: faultInjection, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("POST /receipts", s.postReceipt)
	s.mux.HandleFunc("GET /worklist", s.getWorklist)
	s.mux.HandleFunc("GET /backlog", s.getBacklog)
	s.mux.HandleFunc("GET /cases/{id}", s.getCase)
	s.mux.HandleFunc("GET /cases/{id}/deadline-explanation", s.getDeadlineExplanation)
	s.mux.HandleFunc("POST /cases/{id}/simulate-material", s.simulateMaterial)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ---- 视图 ----

type worklistView struct {
	CaseID            string    `json:"case_id"`
	RiskLevel         string    `json:"risk_level"`
	Stage             string    `json:"stage"`
	NextAction        string    `json:"next_action"`
	DueAt             time.Time `json:"due_at"`
	ResponsibleOrg    string    `json:"responsible_org"`
	BlockingMaterials []string  `json:"blocking_materials"`
	Reasons           []string  `json:"reasons"`
	Notices           []string  `json:"notices"`
	Overdue           bool      `json:"overdue"`
}

func (s *Server) toWorklistView(w *domain.WorklistItem, now time.Time) *worklistView {
	if w == nil {
		return nil
	}
	reasons := domain.ReasonsFor(w, now)
	notices := w.Notices
	if notices == nil {
		notices = []string{}
	}
	blocking := w.BlockingMaterials
	if blocking == nil {
		blocking = []string{}
	}
	if reasons == nil {
		reasons = []string{}
	}
	return &worklistView{
		CaseID:            w.CaseID,
		RiskLevel:         w.RiskLevel,
		Stage:             w.Stage,
		NextAction:        w.NextAction,
		DueAt:             w.DueAt.In(s.fixtures.Rules.Location()),
		ResponsibleOrg:    w.ResponsibleOrg,
		BlockingMaterials: blocking,
		Reasons:           reasons,
		Notices:           notices,
		Overdue:           now.After(w.DueAt),
	}
}

type receiptView struct {
	ReceiptID    string    `json:"receipt_id"`
	Type         string    `json:"type"`
	ActorRole    string    `json:"actor_role"`
	OccurredAt   time.Time `json:"occurred_at"`
	Status       string    `json:"status"`
	RejectReason string    `json:"reject_reason,omitempty"`
}

type caseView struct {
	CaseID            string        `json:"case_id"`
	RiskLevel         string        `json:"risk_level"`
	Stage             string        `json:"stage"`
	Decision          string        `json:"decision,omitempty"`
	AmountCents       *int64        `json:"amount_cents,omitempty"`
	AmountBand        string        `json:"amount_band,omitempty"`
	Currency          string        `json:"currency,omitempty"`
	CustomerContact   string        `json:"customer_contact,omitempty"`
	Materials         []string      `json:"materials"`
	RequiredMaterials []string      `json:"required_materials"`
	Receipts          []receiptView `json:"receipts"`
	Worklist          *worklistView `json:"worklist"`
}

func (s *Server) toCaseView(b *store.CaseBundle, role string, now time.Time) *caseView {
	rules := s.fixtures.Rules
	mask := rules.MaskFor(role)
	v := &caseView{
		CaseID:            b.Case.CaseID,
		RiskLevel:         b.Case.RiskLevel,
		Stage:             b.Case.Stage,
		Decision:          b.Case.Decision,
		Materials:         b.Case.Materials,
		RequiredMaterials: rules.RequiredMaterials(b.Case.RiskLevel),
		Worklist:          s.toWorklistView(b.Worklist, now),
	}
	if v.Materials == nil {
		v.Materials = []string{}
	}
	if b.Case.AmountCents != nil {
		switch mask.Amount {
		case "full":
			amt := *b.Case.AmountCents
			v.AmountCents = &amt
			v.Currency = b.Case.Currency
		case "band":
			v.AmountBand = rules.AmountBandFor(*b.Case.AmountCents)
			v.Currency = b.Case.Currency
		}
	}
	if contact := domain.MaskContact(mask.Contact, b.Case.CustomerContact); contact != "" {
		v.CustomerContact = contact
	}
	for _, r := range b.Receipts {
		v.Receipts = append(v.Receipts, receiptView{
			ReceiptID:    r.ReceiptID,
			Type:         r.Type,
			ActorRole:    r.ActorRole,
			OccurredAt:   r.OccurredAt.In(rules.Location()),
			Status:       r.Status,
			RejectReason: r.RejectReason,
		})
	}
	if v.Receipts == nil {
		v.Receipts = []receiptView{}
	}
	return v
}

// ---- POST /receipts ----

type receiptRequest struct {
	ReceiptID     *string   `json:"receipt_id"`
	CaseID        *string   `json:"case_id"`
	Type          *string   `json:"type"`
	ActorRole     *string   `json:"actor_role"`
	OccurredAt    *string   `json:"occurred_at"`
	RiskLevel     *string   `json:"risk_level"`
	MaterialCodes *[]string `json:"material_codes"`
}

func (s *Server) postReceipt(w http.ResponseWriter, r *http.Request) {
	rules := s.fixtures.Rules
	var req receiptRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"details": []string{fmt.Sprintf("请求体不符合契约: %v", err)},
		})
		return
	}
	var problems []string
	requireStr := func(name string, p *string, max int) string {
		if p == nil {
			problems = append(problems, name+" 缺失")
			return ""
		}
		if *p == "" {
			problems = append(problems, name+" 不能为空")
		}
		if len(*p) > max {
			problems = append(problems, fmt.Sprintf("%s 长度超过 %d", name, max))
		}
		return *p
	}
	receiptID := requireStr("receipt_id", req.ReceiptID, 128)
	caseID := requireStr("case_id", req.CaseID, 128)
	typ := requireStr("type", req.Type, 64)
	role := requireStr("actor_role", req.ActorRole, 64)
	occurredStr := requireStr("occurred_at", req.OccurredAt, 64)
	risk := requireStr("risk_level", req.RiskLevel, 32)
	if req.MaterialCodes == nil {
		problems = append(problems, "material_codes 缺失")
	}
	var materials []string
	if req.MaterialCodes != nil {
		materials = *req.MaterialCodes
		if len(materials) > 32 {
			problems = append(problems, "material_codes 超过 32 项")
		}
		for _, m := range materials {
			if m == "" || len(m) > 64 {
				problems = append(problems, "material_codes 含非法材料编码")
				break
			}
		}
	}
	var occurredAt time.Time
	if occurredStr != "" {
		t, err := time.Parse(time.RFC3339, occurredStr)
		if err != nil {
			problems = append(problems, "occurred_at 不是合法 RFC3339 时间")
		} else {
			occurredAt = t
		}
	}
	if typ != "" && !domain.Known(rules.ReceiptTypes, typ) {
		problems = append(problems, "type 不在夹具 receipt_types 中")
	}
	if role != "" && !domain.Known(rules.Roles, role) {
		problems = append(problems, "actor_role 不在夹具 roles 中")
	}
	if risk != "" && !domain.Known(rules.RiskLevels, risk) {
		problems = append(problems, "risk_level 不在夹具 risk_levels 中")
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "details": problems})
		return
	}

	rec := domain.Receipt{
		ReceiptID:     receiptID,
		CaseID:        caseID,
		Type:          typ,
		ActorRole:     role,
		OccurredAt:    occurredAt,
		RiskLevel:     risk,
		MaterialCodes: materials,
	}
	inject := s.faultInjection && r.Header.Get("X-Fault-Inject") == "rollback"

	outcome, err := s.store.ApplyReceipt(r.Context(), rec, inject, func(existing *domain.Case) (*domain.ApplyResult, error) {
		var txn *domain.Transaction
		if t, ok := s.fixtures.Transactions[caseID]; ok {
			t := t
			txn = &t
		}
		return domain.Apply(rules, txn, existing, rec)
	})
	if err != nil {
		if errors.Is(err, store.ErrInjectedRollback) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":      "injected_rollback",
				"receipt_id": receiptID,
				"message":    "故障注入：原始回执、工作单与期限重算依据已整体回滚",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal", "message": err.Error()})
		return
	}

	now := time.Now()
	if outcome.Duplicate {
		writeJSON(w, http.StatusOK, map[string]any{
			"receipt_id":    receiptID,
			"duplicate":     true,
			"status":        "duplicate",
			"stored_status": outcome.Stored.Status,
			"case":          s.caseViewOrNil(w, r, caseID, now),
		})
		return
	}
	res := outcome.Applied
	body := map[string]any{
		"receipt_id": receiptID,
		"duplicate":  false,
		"status":     res.ReceiptStatus,
		"case":       s.caseViewOrNil(w, r, caseID, now),
	}
	switch res.ReceiptStatus {
	case domain.ReceiptApplied:
		writeJSON(w, http.StatusOK, body)
	case domain.ReceiptRejectedRole:
		body["reason"] = res.RejectReason
		writeJSON(w, http.StatusUnprocessableEntity, body)
	default:
		body["reason"] = res.RejectReason
		writeJSON(w, http.StatusConflict, body)
	}
}

func (s *Server) caseViewOrNil(w http.ResponseWriter, r *http.Request, caseID string, now time.Time) *caseView {
	b, err := s.store.GetCase(r.Context(), caseID)
	if err != nil || b == nil {
		return nil
	}
	return s.toCaseView(b, actorRole(r), now)
}

// ---- GET /worklist ----

func (s *Server) getWorklist(w http.ResponseWriter, r *http.Request) {
	now, err := s.parseNow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_now", "message": err.Error()})
		return
	}
	org := r.URL.Query().Get("org")
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_limit"})
			return
		}
		limit = n
	}
	items, err := s.store.ListWorklist(r.Context(), org, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal", "message": err.Error()})
		return
	}
	views := make([]*worklistView, 0, len(items))
	for i := range items {
		views = append(views, s.toWorklistView(&items[i], now))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":   now.In(s.fixtures.Rules.Location()),
		"count": len(views),
		"items": views,
	})
}

// ---- GET /backlog ----

func (s *Server) getBacklog(w http.ResponseWriter, r *http.Request) {
	now, err := s.parseNow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_now", "message": err.Error()})
		return
	}
	orgs, err := s.store.Backlog(r.Context(), now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal", "message": err.Error()})
		return
	}
	if orgs == nil {
		orgs = []store.OrgBacklog{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":  now.In(s.fixtures.Rules.Location()),
		"orgs": orgs,
	})
}

// ---- GET /cases/{id} ----

func (s *Server) getCase(w http.ResponseWriter, r *http.Request) {
	now, err := s.parseNow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_now", "message": err.Error()})
		return
	}
	b, err := s.store.GetCase(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal", "message": err.Error()})
		return
	}
	if b == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "case_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, s.toCaseView(b, actorRole(r), now))
}

// ---- GET /cases/{id}/deadline-explanation ----

type recalcView struct {
	ReceiptID       string              `json:"receipt_id"`
	Stage           string              `json:"stage"`
	SLAHours        int                 `json:"sla_business_hours"`
	BaseTime        time.Time           `json:"base_time"`
	ComputedDue     time.Time           `json:"computed_due"`
	Segments        []domain.Segment    `json:"segments"`
	SkippedDays     []domain.SkippedDay `json:"skipped_days"`
	HolidayRollover bool                `json:"holiday_rollover"`
}

func (s *Server) getDeadlineExplanation(w http.ResponseWriter, r *http.Request) {
	now, err := s.parseNow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_now", "message": err.Error()})
		return
	}
	caseID := r.PathValue("id")
	b, err := s.store.GetCase(r.Context(), caseID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal", "message": err.Error()})
		return
	}
	if b == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "case_not_found"})
		return
	}
	history, err := s.store.RecalcHistory(r.Context(), caseID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal", "message": err.Error()})
		return
	}
	loc := s.fixtures.Rules.Location()
	recalcs := make([]recalcView, 0, len(history))
	for _, h := range history {
		skipped := h.SkippedDays
		if skipped == nil {
			skipped = []domain.SkippedDay{}
		}
		segments := h.Segments
		if segments == nil {
			segments = []domain.Segment{}
		}
		recalcs = append(recalcs, recalcView{
			ReceiptID:       h.ReceiptID,
			Stage:           h.Stage,
			SLAHours:        h.SLAHours,
			BaseTime:        h.BaseTime.In(loc),
			ComputedDue:     h.ComputedDue.In(loc),
			Segments:        segments,
			SkippedDays:     skipped,
			HolidayRollover: h.HolidayRollover,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"case_id":          caseID,
		"risk_level":       b.Case.RiskLevel,
		"stage":            b.Case.Stage,
		"now":              now.In(loc),
		"current_worklist": s.toWorklistView(b.Worklist, now),
		"recalculations":   recalcs,
	})
}

// ---- POST /cases/{id}/simulate-material ----

type simulateRequest struct {
	MaterialCode string `json:"material_code"`
	Now          string `json:"now"`
}

func (s *Server) simulateMaterial(w http.ResponseWriter, r *http.Request) {
	var req simulateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": err.Error()})
		return
	}
	if req.MaterialCode == "" || len(req.MaterialCode) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": "material_code 缺失或非法"})
		return
	}
	now := time.Now()
	if req.Now != "" {
		t, err := time.Parse(time.RFC3339, req.Now)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_now", "message": err.Error()})
			return
		}
		now = t
	}
	b, err := s.store.GetCase(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal", "message": err.Error()})
		return
	}
	if b == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "case_not_found"})
		return
	}
	sim, err := domain.SimulateMaterial(s.fixtures.Rules, b.Case, b.Worklist, req.MaterialCode, now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal", "message": err.Error()})
		return
	}
	already := domain.Known(b.Case.Materials, req.MaterialCode)
	wouldUnblock := sim != nil && b.Worklist != nil && sim.Stage != b.Worklist.Stage
	writeJSON(w, http.StatusOK, map[string]any{
		"case_id":         b.Case.CaseID,
		"material_code":   req.MaterialCode,
		"already_present": already,
		"would_unblock":   wouldUnblock,
		"current":         s.toWorklistView(b.Worklist, now),
		"simulated":       s.toWorklistView(sim, now),
	})
}

// ---- 工具 ----

func actorRole(r *http.Request) string { return strings.TrimSpace(r.Header.Get("X-Actor-Role")) }

func (s *Server) parseNow(r *http.Request) (time.Time, error) {
	v := r.URL.Query().Get("now")
	if v == "" {
		return time.Now(), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("now 参数不是合法 RFC3339 时间: %v", err)
	}
	return t, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
