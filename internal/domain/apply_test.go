package domain

import (
	"testing"
)

func receipt(t *testing.T, id, caseID, typ, role, at, risk string, mats ...string) Receipt {
	t.Helper()
	return Receipt{
		ReceiptID:     id,
		CaseID:        caseID,
		Type:          typ,
		ActorRole:     role,
		OccurredAt:    mustParse(t, at),
		RiskLevel:     risk,
		MaterialCodes: mats,
	}
}

func TestRiskRegisteredCreatesWorklist(t *testing.T) {
	r := testRules(t)
	txn := &Transaction{CaseID: "c1", AmountCents: 100, Currency: "CNY", CustomerContact: "+86-1"}
	res, err := Apply(r, txn, nil, receipt(t, "r1", "c1", TypeRiskRegistered, "teller", "2026-09-08T10:54:00+08:00", "high", "transaction_summary"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ReceiptStatus != ReceiptApplied {
		t.Fatalf("status = %s", res.ReceiptStatus)
	}
	if res.Case == nil || res.Case.Stage != StageAwaitingContact {
		t.Fatalf("case = %+v", res.Case)
	}
	if res.Worklist.NextAction != ActionContactCustomer {
		t.Fatalf("next_action = %s", res.Worklist.NextAction)
	}
	if got := res.Worklist.DueAt.In(r.Location()).Format("2006-01-02T15:04:05Z07:00"); got != "2026-09-08T12:54:00+08:00" {
		t.Fatalf("due = %s", got)
	}
	if res.Worklist.ResponsibleOrg != "branch_outlet" {
		t.Fatalf("org = %s", res.Worklist.ResponsibleOrg)
	}
	if res.Recalc == nil || res.Recalc.SLAHours != 2 {
		t.Fatalf("recalc = %+v", res.Recalc)
	}
	if res.Case.AmountCents == nil || *res.Case.AmountCents != 100 {
		t.Fatalf("amount not attached")
	}
}

func TestRiskRegisteredDuplicateCaseRejected(t *testing.T) {
	r := testRules(t)
	existing := &Case{CaseID: "c1", Stage: StageAwaitingContact, RiskLevel: "high"}
	res, err := Apply(r, nil, existing, receipt(t, "r2", "c1", TypeRiskRegistered, "teller", "2026-09-08T10:54:00+08:00", "high"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ReceiptStatus != ReceiptRejectedState || res.RejectReason != RejectCaseExists {
		t.Fatalf("res = %+v", res)
	}
	if res.Case != nil || res.Worklist != nil {
		t.Fatal("rejected receipt must not change state")
	}
}

func TestRoleNotAllowedLeavesNoChanges(t *testing.T) {
	r := testRules(t)
	existing := &Case{CaseID: "c1", Stage: StageAwaitingDecision, RiskLevel: "medium"}
	res, err := Apply(r, nil, existing, receipt(t, "r3", "c1", TypeDecisionRelease, "teller", "2026-09-08T11:00:00+08:00", "medium"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ReceiptStatus != ReceiptRejectedRole || res.RejectReason != ReasonRoleNotAllowed {
		t.Fatalf("res = %+v", res)
	}
	if res.Case != nil || res.Worklist != nil || res.Recalc != nil || res.WorklistDeleted {
		t.Fatal("unauthorized decision must not change worklist")
	}
}

func TestContactThenBlockedReview(t *testing.T) {
	r := testRules(t)
	c := &Case{CaseID: "c1", Stage: StageAwaitingContact, RiskLevel: "high", Materials: []string{"transaction_summary"}}
	res, err := Apply(r, nil, c, receipt(t, "r4", "c1", TypeCustomerContacted, "teller", "2026-09-08T11:00:00+08:00", "high", "customer_contact"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Case.Stage != StageAwaitingReview {
		t.Fatalf("stage = %s", res.Case.Stage)
	}
	if len(res.Worklist.BlockingMaterials) != 1 || res.Worklist.BlockingMaterials[0] != "id_verification" {
		t.Fatalf("blocking = %v", res.Worklist.BlockingMaterials)
	}
	if got := res.Worklist.DueAt.In(r.Location()).Format("2006-01-02T15:04:05Z07:00"); got != "2026-09-08T15:00:00+08:00" {
		t.Fatalf("due = %s", got)
	}
}

func TestMaterialCompletionAdvancesToDecision(t *testing.T) {
	r := testRules(t)
	c := &Case{CaseID: "c1", Stage: StageAwaitingReview, RiskLevel: "high",
		Materials: []string{"customer_contact", "transaction_summary"}}
	res, err := Apply(r, nil, c, receipt(t, "r5", "c1", TypeMaterialSubmitted, "bank_reviewer", "2026-09-09T10:00:00+08:00", "high", "id_verification"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Case.Stage != StageAwaitingDecision {
		t.Fatalf("stage = %s", res.Case.Stage)
	}
	if res.Recalc == nil || res.Recalc.SLAHours != 8 {
		t.Fatalf("recalc = %+v", res.Recalc)
	}
	// 8 营业小时：9/9 10:00→17:00 共 7h，余 1h 到 9/10 10:00。
	if got := res.Worklist.DueAt.In(r.Location()).Format("2006-01-02T15:04:05Z07:00"); got != "2026-09-10T10:00:00+08:00" {
		t.Fatalf("due = %s", got)
	}
	if res.Worklist.ResponsibleOrg != "anti_fraud_center" {
		t.Fatalf("org = %s", res.Worklist.ResponsibleOrg)
	}
}

func TestMaterialSubmittedWhileAwaitingContactOnlyMerges(t *testing.T) {
	r := testRules(t)
	c := &Case{CaseID: "c1", Stage: StageAwaitingContact, RiskLevel: "low", Materials: nil}
	res, err := Apply(r, nil, c, receipt(t, "r6", "c1", TypeMaterialSubmitted, "teller", "2026-09-08T10:00:00+08:00", "low", "transaction_summary"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Case.Stage != StageAwaitingContact {
		t.Fatalf("stage = %s", res.Case.Stage)
	}
	if res.Worklist != nil || res.Recalc != nil {
		t.Fatal("no worklist change expected")
	}
	if len(res.Case.Materials) != 1 {
		t.Fatalf("materials = %v", res.Case.Materials)
	}
}

func TestDecisionWhileBlockedRejected(t *testing.T) {
	r := testRules(t)
	c := &Case{CaseID: "c1", Stage: StageAwaitingReview, RiskLevel: "high", Materials: []string{"transaction_summary"}}
	res, err := Apply(r, nil, c, receipt(t, "r7", "c1", TypeDecisionEscalate, "bank_reviewer", "2026-09-08T12:00:00+08:00", "high"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ReceiptStatus != ReceiptRejectedState || res.RejectReason != ReasonMaterialBlocked {
		t.Fatalf("res = %+v", res)
	}
}

func TestDecisionClosesCase(t *testing.T) {
	r := testRules(t)
	c := &Case{CaseID: "c1", Stage: StageAwaitingDecision, RiskLevel: "medium"}
	res, err := Apply(r, nil, c, receipt(t, "r8", "c1", TypeDecisionRelease, "anti_fraud_officer", "2026-09-08T12:00:00+08:00", "medium"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ReceiptStatus != ReceiptApplied || !res.WorklistDeleted {
		t.Fatalf("res = %+v", res)
	}
	if res.Case.Stage != StageClosed || res.Case.Decision != "release" {
		t.Fatalf("case = %+v", res.Case)
	}
}

func TestSimulateMaterial(t *testing.T) {
	r := testRules(t)
	c := &Case{CaseID: "c1", Stage: StageAwaitingReview, RiskLevel: "high",
		Materials: []string{"customer_contact", "transaction_summary"}}
	cur := &WorklistItem{CaseID: "c1", Stage: StageAwaitingReview, BlockingMaterials: []string{"id_verification"}}
	sim, err := SimulateMaterial(r, c, cur, "id_verification", mustParse(t, "2026-09-10T12:00:00+08:00"))
	if err != nil {
		t.Fatal(err)
	}
	if sim.Stage != StageAwaitingDecision || sim.NextAction != ActionDecide {
		t.Fatalf("sim = %+v", sim)
	}
	// 9/10 12:00 + 8h → 5h 当日 + 3h 次日 = 9/11 12:00。
	if got := sim.DueAt.In(r.Location()).Format("2006-01-02T15:04:05Z07:00"); got != "2026-09-11T12:00:00+08:00" {
		t.Fatalf("due = %s", got)
	}
	// 未补齐时仍显示剩余阻塞。
	sim2, err := SimulateMaterial(r, c, cur, "other_material", mustParse(t, "2026-09-10T12:00:00+08:00"))
	if err != nil {
		t.Fatal(err)
	}
	if sim2.Stage != StageAwaitingReview || len(sim2.BlockingMaterials) != 1 {
		t.Fatalf("sim2 = %+v", sim2)
	}
}

func TestReasonsFor(t *testing.T) {
	item := &WorklistItem{Stage: StageAwaitingContact, DueAt: mustParse(t, "2026-09-08T12:00:00+08:00")}
	if rs := ReasonsFor(item, mustParse(t, "2026-09-08T11:00:00+08:00")); len(rs) != 0 {
		t.Fatalf("reasons = %v", rs)
	}
	if rs := ReasonsFor(item, mustParse(t, "2026-09-08T12:00:01+08:00")); len(rs) != 1 || rs[0] != ReasonContactDue {
		t.Fatalf("reasons = %v", rs)
	}
	blocked := &WorklistItem{Stage: StageAwaitingReview, DueAt: mustParse(t, "2026-09-08T12:00:00+08:00"), BlockingMaterials: []string{"x"}}
	rs := ReasonsFor(blocked, mustParse(t, "2026-09-09T12:00:00+08:00"))
	if len(rs) != 2 || rs[0] != ReasonMaterialBlocked || rs[1] != ReasonActionOverdue {
		t.Fatalf("reasons = %v", rs)
	}
}

func TestContactWithCompleteMaterialsJumpsToDecision(t *testing.T) {
	r := testRules(t)
	c := &Case{CaseID: "c1", Stage: StageAwaitingContact, RiskLevel: "medium",
		Materials: []string{"customer_contact", "transaction_summary"}}
	res, err := Apply(r, nil, c, receipt(t, "r9", "c1", TypeCustomerContacted, "teller", "2026-09-08T10:00:00+08:00", "medium"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Case.Stage != StageAwaitingDecision {
		t.Fatalf("stage = %s", res.Case.Stage)
	}
	if res.Worklist.NextAction != ActionDecide || len(res.Worklist.BlockingMaterials) != 0 {
		t.Fatalf("worklist = %+v", res.Worklist)
	}
	// medium 处置时限 24 营业小时：9/8 余 7h + 9/9 8h + 9/10 8h + 9/11 1h。
	if got := res.Worklist.DueAt.In(r.Location()).Format("2006-01-02T15:04:05Z07:00"); got != "2026-09-11T10:00:00+08:00" {
		t.Fatalf("due = %s", got)
	}
}
