package domain

import (
	"fmt"
	"time"
)

// stageSLAKey 把案件阶段映射到 sla_business_hours 的键。
func stageSLAKey(stage string) string {
	switch stage {
	case StageAwaitingContact:
		return "contact"
	case StageAwaitingReview:
		return "review"
	case StageAwaitingDecision:
		return "decision"
	}
	return ""
}

// stageAction 把案件阶段映射到工作单下一动作。
func stageAction(stage string) string {
	switch stage {
	case StageAwaitingContact:
		return ActionContactCustomer
	case StageAwaitingReview:
		return ActionReviewMaterials
	case StageAwaitingDecision:
		return ActionDecide
	}
	return ""
}

// Apply 是纯函数：给定案件现状与一条回执，给出全部状态变更。
// existing 为 nil 表示案件尚不存在；txn 为案件对应的脱敏交易（可空）。
// 返回的 ApplyResult 由持久化层在单个事务中落库。
func Apply(rules *Rules, txn *Transaction, existing *Case, rec Receipt) (*ApplyResult, error) {
	if !rules.Allowed(rec.Type, rec.ActorRole) {
		return &ApplyResult{ReceiptStatus: ReceiptRejectedRole, RejectReason: ReasonRoleNotAllowed}, nil
	}
	switch rec.Type {
	case TypeRiskRegistered:
		return applyRiskRegistered(rules, txn, existing, rec)
	case TypeCustomerContacted:
		return applyCustomerContacted(rules, existing, rec)
	case TypeMaterialSubmitted:
		return applyMaterialSubmitted(rules, existing, rec)
	case TypeDecisionRelease, TypeDecisionEscalate:
		return applyDecision(rules, existing, rec)
	}
	return nil, fmt.Errorf("未知回执类型 %q", rec.Type)
}

// newWorklist 组装某阶段的工作单条目并计算期限。
func newWorklist(rules *Rules, c *Case, rec Receipt, stage string, blocking []string) (*WorklistItem, *RecalcBasis, error) {
	sla := rules.SLA(stageSLAKey(stage), c.RiskLevel)
	basis, notices, err := rules.ComputeDeadline(c.CaseID, rec.ReceiptID, stage, stageSLAKey(stage), sla, rec.OccurredAt)
	if err != nil {
		return nil, nil, err
	}
	item := &WorklistItem{
		CaseID:            c.CaseID,
		RiskLevel:         c.RiskLevel,
		Stage:             stage,
		NextAction:        stageAction(stage),
		DueAt:             basis.ComputedDue,
		ResponsibleOrg:    rules.ResponsibleOrg(stage),
		BlockingMaterials: SortedSet(blocking),
		Notices:           notices,
	}
	return item, basis, nil
}

func applyRiskRegistered(rules *Rules, txn *Transaction, existing *Case, rec Receipt) (*ApplyResult, error) {
	if existing != nil {
		return &ApplyResult{ReceiptStatus: ReceiptRejectedState, RejectReason: RejectCaseExists}, nil
	}
	c := &Case{
		CaseID:    rec.CaseID,
		RiskLevel: rec.RiskLevel,
		Stage:     StageAwaitingContact,
		Materials: SortedSet(rec.MaterialCodes),
	}
	if txn != nil {
		amt := txn.AmountCents
		c.AmountCents = &amt
		c.Currency = txn.Currency
		c.CustomerContact = txn.CustomerContact
	}
	item, basis, err := newWorklist(rules, c, rec, StageAwaitingContact, nil)
	if err != nil {
		return nil, err
	}
	return &ApplyResult{
		ReceiptStatus: ReceiptApplied,
		Case:          c,
		Worklist:      item,
		Recalc:        basis,
	}, nil
}

func applyCustomerContacted(rules *Rules, existing *Case, rec Receipt) (*ApplyResult, error) {
	if existing == nil {
		return &ApplyResult{ReceiptStatus: ReceiptRejectedState, RejectReason: RejectCaseNotFound}, nil
	}
	if existing.Stage != StageAwaitingContact {
		return &ApplyResult{ReceiptStatus: ReceiptRejectedState, RejectReason: RejectInvalidStage}, nil
	}
	c := *existing
	c.Materials = MergeSet(existing.Materials, rec.MaterialCodes)
	blocking := rules.MissingMaterials(c.RiskLevel, c.Materials)
	// 联系即材料齐全：直接进入处置决定阶段；否则进入复核并列出阻塞材料。
	stage := StageAwaitingReview
	if len(blocking) == 0 {
		stage = StageAwaitingDecision
	}
	c.Stage = stage
	item, basis, err := newWorklist(rules, &c, rec, stage, blocking)
	if err != nil {
		return nil, err
	}
	return &ApplyResult{
		ReceiptStatus: ReceiptApplied,
		Case:          &c,
		Worklist:      item,
		Recalc:        basis,
	}, nil
}

func applyMaterialSubmitted(rules *Rules, existing *Case, rec Receipt) (*ApplyResult, error) {
	if existing == nil {
		return &ApplyResult{ReceiptStatus: ReceiptRejectedState, RejectReason: RejectCaseNotFound}, nil
	}
	if existing.Stage == StageClosed {
		return &ApplyResult{ReceiptStatus: ReceiptRejectedState, RejectReason: RejectCaseClosed}, nil
	}
	c := *existing
	c.Materials = MergeSet(existing.Materials, rec.MaterialCodes)
	res := &ApplyResult{ReceiptStatus: ReceiptApplied, Case: &c}

	// 复核阶段材料补齐后，案件进入处置决定阶段并重算期限。
	if existing.Stage == StageAwaitingReview && len(rules.MissingMaterials(c.RiskLevel, c.Materials)) == 0 {
		c.Stage = StageAwaitingDecision
		res.Case = &c
		item, basis, err := newWorklist(rules, &c, rec, StageAwaitingDecision, nil)
		if err != nil {
			return nil, err
		}
		res.Worklist = item
		res.Recalc = basis
	}
	return res, nil
}

func applyDecision(rules *Rules, existing *Case, rec Receipt) (*ApplyResult, error) {
	if existing == nil {
		return &ApplyResult{ReceiptStatus: ReceiptRejectedState, RejectReason: RejectCaseNotFound}, nil
	}
	switch existing.Stage {
	case StageClosed:
		return &ApplyResult{ReceiptStatus: ReceiptRejectedState, RejectReason: RejectCaseClosed}, nil
	case StageAwaitingContact:
		return &ApplyResult{ReceiptStatus: ReceiptRejectedState, RejectReason: RejectContactNotDue}, nil
	case StageAwaitingReview:
		return &ApplyResult{ReceiptStatus: ReceiptRejectedState, RejectReason: ReasonMaterialBlocked}, nil
	}
	c := *existing
	c.Stage = StageClosed
	if rec.Type == TypeDecisionRelease {
		c.Decision = "release"
	} else {
		c.Decision = "escalate"
	}
	return &ApplyResult{
		ReceiptStatus:   ReceiptApplied,
		Case:            &c,
		WorklistDeleted: true,
	}, nil
}

// SimulateMaterial 模拟补齐某份材料后的工作单（纯计算，不落库）。
// now 作为假想重算的起算时刻。current 为案件当前工作单（结案时为 nil）。
// 返回模拟出的工作单条目；案件已结案时返回 nil。
func SimulateMaterial(rules *Rules, existing *Case, current *WorklistItem, materialCode string, now time.Time) (*WorklistItem, error) {
	if existing == nil || existing.Stage == StageClosed {
		return nil, nil
	}
	materials := MergeSet(existing.Materials, []string{materialCode})
	if existing.Stage == StageAwaitingReview {
		missing := rules.MissingMaterials(existing.RiskLevel, materials)
		if len(missing) == 0 {
			// 假想补齐后进入处置决定阶段，期限从 now 起算。
			sla := rules.SLA("decision", existing.RiskLevel)
			basis, notices, err := rules.ComputeDeadline(existing.CaseID, "(simulate)", StageAwaitingDecision, "decision", sla, now)
			if err != nil {
				return nil, err
			}
			return &WorklistItem{
				CaseID:         existing.CaseID,
				RiskLevel:      existing.RiskLevel,
				Stage:          StageAwaitingDecision,
				NextAction:     ActionDecide,
				DueAt:          basis.ComputedDue,
				ResponsibleOrg: rules.ResponsibleOrg(StageAwaitingDecision),
				Notices:        notices,
			}, nil
		}
		item := *current
		item.BlockingMaterials = missing
		return &item, nil
	}
	// 其他阶段补材料不改变工作单。
	if current == nil {
		return nil, nil
	}
	item := *current
	return &item, nil
}
