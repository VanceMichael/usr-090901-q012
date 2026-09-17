// Package domain 实现协同止付工作单的核心规则：营业时限计算、
// 回执驱动的状态机、以及按角色裁剪敏感字段。所有函数均为纯函数，
// 持久化由 internal/store 负责。
package domain

import (
	"fmt"
	"slices"
	"time"
)

// 回执类型（夹具 receipt_types）。
const (
	TypeRiskRegistered    = "risk_registered"
	TypeCustomerContacted = "customer_contacted"
	TypeMaterialSubmitted = "material_submitted"
	TypeDecisionRelease   = "decision_release"
	TypeDecisionEscalate  = "decision_escalate"
)

// 案件阶段。
const (
	StageAwaitingContact  = "awaiting_contact"
	StageAwaitingReview   = "awaiting_review"
	StageAwaitingDecision = "awaiting_decision"
	StageClosed           = "closed"
)

// 工作单下一动作。
const (
	ActionContactCustomer = "contact_customer"
	ActionReviewMaterials = "review_materials"
	ActionDecide          = "decide_release_or_escalate"
)

// 回执处理结果状态。
const (
	ReceiptApplied       = "applied"
	ReceiptRejectedRole  = "rejected_role"
	ReceiptRejectedState = "rejected_state"
)

// 原因码（夹具 reason_codes）。
const (
	ReasonContactDue      = "CONTACT_DUE"
	ReasonMaterialBlocked = "MATERIAL_BLOCKED"
	ReasonActionOverdue   = "ACTION_OVERDUE"
	ReasonRoleNotAllowed  = "ROLE_NOT_ALLOWED"
	ReasonHolidayRollover = "HOLIDAY_ROLLOVER"
)

// 状态类拒绝原因（非夹具原因码，用于解释拒绝的具体状态冲突）。
const (
	RejectCaseExists    = "CASE_EXISTS"
	RejectCaseNotFound  = "CASE_NOT_FOUND"
	RejectCaseClosed    = "CASE_CLOSED"
	RejectInvalidStage  = "INVALID_STAGE"
	RejectContactNotDue = "CONTACT_NOT_DONE"
)

// FieldMask 描述某角色对敏感字段的可见性：full / band / masked / hidden。
type FieldMask struct {
	Amount  string `json:"amount"`
	Contact string `json:"contact"`
}

// AmountBand 金额分档（脱敏展示用）。
type AmountBand struct {
	MinCents int64  `json:"min_cents"`
	Label    string `json:"label"`
}

// Rules 来自 fixtures/rules.json 的固定规则。
type Rules struct {
	Version         string                      `json:"version"`
	ReasonCodes     []string                    `json:"reason_codes"`
	Timezone        string                      `json:"timezone"`
	WorkingWindow   struct{ Start, End string } `json:"working_window"`
	WorkingWeekdays []int                       `json:"working_weekdays"`
	Holidays        []string                    `json:"holidays"`
	ReceiptTypes    []string                    `json:"receipt_types"`
	RiskLevels      []string                    `json:"risk_levels"`
	Roles           []string                    `json:"roles"`
	Permissions     map[string][]string         `json:"permissions"`
	RequiredMats    map[string][]string         `json:"required_materials"`
	SLAHours        map[string]map[string]int   `json:"sla_business_hours"`
	ResponsibleOrgs map[string]string           `json:"responsible_orgs"`
	Masking         map[string]FieldMask        `json:"masking"`
	AmountBands     []AmountBand                `json:"amount_bands"`

	loc          *time.Location
	holidaySet   map[string]bool
	weekdaySet   map[time.Weekday]bool
	windowStartM int
	windowEndM   int
}

// Transaction 是 fixtures/transactions.json 中的一条脱敏交易。
type Transaction struct {
	CaseID          string `json:"case_id"`
	AmountCents     int64  `json:"amount_cents"`
	Currency        string `json:"currency"`
	CustomerContact string `json:"customer_contact"`
}

// Receipt 是一条来源回执（对应 contracts/request.schema.json）。
type Receipt struct {
	ReceiptID     string    `json:"receipt_id"`
	CaseID        string    `json:"case_id"`
	Type          string    `json:"type"`
	ActorRole     string    `json:"actor_role"`
	OccurredAt    time.Time `json:"occurred_at"`
	RiskLevel     string    `json:"risk_level"`
	MaterialCodes []string  `json:"material_codes"`
}

// Case 是案件当前状态。
type Case struct {
	CaseID          string
	RiskLevel       string
	Stage           string
	Decision        string // release | escalate，结案前为空
	AmountCents     *int64
	Currency        string
	CustomerContact string
	Materials       []string // 已收材料，排序去重
}

// WorklistItem 是某案件当前唯一的工作单条目。
type WorklistItem struct {
	CaseID            string
	RiskLevel         string
	Stage             string
	NextAction        string
	DueAt             time.Time
	ResponsibleOrg    string
	BlockingMaterials []string
	Notices           []string // 计算期提示，如 HOLIDAY_ROLLOVER
}

// SkippedDay 记录时限计算中跳过的非工作日。
type SkippedDay struct {
	Date   string `json:"date"`
	Reason string `json:"reason"` // weekend | holiday
}

// Segment 记录时限在某个工作日实际消耗的区间。
type Segment struct {
	Date  string `json:"date"`
	From  string `json:"from"`
	To    string `json:"to"`
	Hours string `json:"hours"`
}

// RecalcBasis 是一次期限重算的全部依据。
type RecalcBasis struct {
	CaseID          string
	ReceiptID       string
	Stage           string
	SLAHours        int
	BaseTime        time.Time
	ComputedDue     time.Time
	SkippedDays     []SkippedDay
	Segments        []Segment
	HolidayRollover bool
}

// ApplyResult 是纯函数 Apply 的输出：要持久化的全部变更。
type ApplyResult struct {
	ReceiptStatus   string // applied | rejected_role | rejected_state
	RejectReason    string
	Case            *Case         // 需要 upsert 的案件状态；nil 表示不变
	Worklist        *WorklistItem // 需要 upsert 的工作单；nil 表示不变或删除
	WorklistDeleted bool          // true 表示删除该案件工作单（结案）
	Recalc          *RecalcBasis  // 需要记录的期限重算依据；nil 表示未重算
}

// Finalize 校验并初始化派生字段。
func (r *Rules) Finalize() error {
	if r.Timezone == "" {
		return fmt.Errorf("rules.timezone 缺失")
	}
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		return fmt.Errorf("加载时区 %s 失败: %w", r.Timezone, err)
	}
	r.loc = loc
	r.holidaySet = map[string]bool{}
	for _, h := range r.Holidays {
		if _, err := time.ParseInLocation("2006-01-02", h, loc); err != nil {
			return fmt.Errorf("节假日 %q 无法解析: %w", h, err)
		}
		r.holidaySet[h] = true
	}
	r.weekdaySet = map[time.Weekday]bool{}
	for _, d := range r.WorkingWeekdays {
		if d < 1 || d > 7 {
			return fmt.Errorf("working_weekdays 含非法值 %d", d)
		}
		r.weekdaySet[time.Weekday(d%7)] = true
	}
	sm, err := parseHHMM(r.WorkingWindow.Start)
	if err != nil {
		return fmt.Errorf("working_window.start: %w", err)
	}
	em, err := parseHHMM(r.WorkingWindow.End)
	if err != nil {
		return fmt.Errorf("working_window.end: %w", err)
	}
	if em <= sm {
		return fmt.Errorf("working_window.end 必须晚于 start")
	}
	r.windowStartM, r.windowEndM = sm, em
	for _, stage := range []string{"contact", "review", "decision"} {
		if len(r.SLAHours[stage]) == 0 {
			return fmt.Errorf("sla_business_hours.%s 缺失", stage)
		}
	}
	return nil
}

// Location 返回夹具时区。
func (r *Rules) Location() *time.Location { return r.loc }

// Allowed 判断角色是否被允许提交某类回执。
func (r *Rules) Allowed(receiptType, role string) bool {
	for _, v := range r.Permissions[receiptType] {
		if v == role {
			return true
		}
	}
	return false
}

// Known 判断值是否在列表中。
func Known(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// RequiredMaterials 返回某风险级别要求的材料（排序后副本）。
func (r *Rules) RequiredMaterials(risk string) []string {
	return SortedSet(r.RequiredMats[risk])
}

// SLA 返回某阶段某风险级别的营业小时时限。
func (r *Rules) SLA(slaKey, risk string) int {
	if m, ok := r.SLAHours[slaKey]; ok {
		return m[risk]
	}
	return 0
}

// ResponsibleOrg 返回某阶段的负责机构。
func (r *Rules) ResponsibleOrg(stage string) string { return r.ResponsibleOrgs[stage] }

// MissingMaterials 计算还缺哪些必需材料（排序）。
func (r *Rules) MissingMaterials(risk string, have []string) []string {
	set := map[string]bool{}
	for _, m := range have {
		set[m] = true
	}
	var missing []string
	for _, req := range r.RequiredMats[risk] {
		if !set[req] {
			missing = append(missing, req)
		}
	}
	return SortedSet(missing)
}

// SortedSet 去重并排序。
func SortedSet(in []string) []string {
	set := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || set[v] {
			continue
		}
		set[v] = true
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// MergeSet 合并两个材料集合。
func MergeSet(a, b []string) []string { return SortedSet(append(append([]string{}, a...), b...)) }

// ReasonsFor 计算某工作单条目在 now 时刻的原因码（阻塞 + 逾期）。
func ReasonsFor(item *WorklistItem, now time.Time) []string {
	var out []string
	if len(item.BlockingMaterials) > 0 {
		out = append(out, ReasonMaterialBlocked)
	}
	if now.After(item.DueAt) {
		switch item.Stage {
		case StageAwaitingContact:
			out = append(out, ReasonContactDue)
		default:
			out = append(out, ReasonActionOverdue)
		}
	}
	return out
}

func parseHHMM(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, fmt.Errorf("非法时间 %q", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("非法时间 %q", s)
	}
	return h*60 + m, nil
}
