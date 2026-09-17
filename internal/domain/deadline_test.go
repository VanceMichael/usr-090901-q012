package domain

import (
	"testing"
	"time"
)

func testRules(t *testing.T) *Rules {
	t.Helper()
	r := &Rules{
		Timezone:        "Asia/Shanghai",
		WorkingWeekdays: []int{1, 2, 3, 4, 5},
		Holidays:        []string{"2026-09-25", "2026-10-01", "2026-10-02", "2026-10-05", "2026-10-06", "2026-10-07"},
		ReceiptTypes:    []string{TypeRiskRegistered, TypeCustomerContacted, TypeMaterialSubmitted, TypeDecisionRelease, TypeDecisionEscalate},
		RiskLevels:      []string{"high", "medium", "low"},
		Roles:           []string{"teller", "bank_reviewer", "anti_fraud_officer"},
		Permissions: map[string][]string{
			TypeRiskRegistered:    {"teller", "anti_fraud_officer"},
			TypeCustomerContacted: {"teller", "bank_reviewer"},
			TypeMaterialSubmitted: {"teller", "bank_reviewer", "anti_fraud_officer"},
			TypeDecisionRelease:   {"anti_fraud_officer"},
			TypeDecisionEscalate:  {"bank_reviewer", "anti_fraud_officer"},
		},
		RequiredMats: map[string][]string{
			"high":   {"transaction_summary", "customer_contact", "id_verification"},
			"medium": {"transaction_summary", "customer_contact"},
			"low":    {"transaction_summary"},
		},
		SLAHours: map[string]map[string]int{
			"contact":  {"high": 2, "medium": 4, "low": 8},
			"review":   {"high": 4, "medium": 8, "low": 16},
			"decision": {"high": 8, "medium": 24, "low": 48},
		},
		ResponsibleOrgs: map[string]string{
			StageAwaitingContact:  "branch_outlet",
			StageAwaitingReview:   "bank_backoffice",
			StageAwaitingDecision: "anti_fraud_center",
		},
		Masking: map[string]FieldMask{
			"teller":             {Amount: "band", Contact: "hidden"},
			"bank_reviewer":      {Amount: "full", Contact: "masked"},
			"anti_fraud_officer": {Amount: "full", Contact: "full"},
		},
		AmountBands: []AmountBand{
			{MinCents: 0, Label: "10万以下"},
			{MinCents: 10000000, Label: "10万-50万"},
			{MinCents: 50000000, Label: "50万-100万"},
			{MinCents: 100000000, Label: "100万-500万"},
			{MinCents: 500000000, Label: "500万以上"},
		},
	}
	r.WorkingWindow.Start = "09:00"
	r.WorkingWindow.End = "17:00"
	if err := r.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return r
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return v
}

func TestDeadlineSameWindow(t *testing.T) {
	r := testRules(t)
	res, err := r.AddBusinessHours(mustParse(t, "2026-09-08T10:54:00+08:00"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.due.Format(time.RFC3339); got != "2026-09-08T12:54:00+08:00" {
		t.Fatalf("due = %s", got)
	}
	if len(res.skipped) != 0 {
		t.Fatalf("unexpected skipped: %+v", res.skipped)
	}
}

func TestDeadlineCrossesMidnight(t *testing.T) {
	r := testRules(t)
	res, err := r.AddBusinessHours(mustParse(t, "2026-09-08T16:30:00+08:00"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.due.Format(time.RFC3339); got != "2026-09-09T10:30:00+08:00" {
		t.Fatalf("due = %s", got)
	}
	if len(res.segments) != 2 {
		t.Fatalf("segments = %+v", res.segments)
	}
}

func TestDeadlineEndsExactlyAtWindowEnd(t *testing.T) {
	r := testRules(t)
	res, err := r.AddBusinessHours(mustParse(t, "2026-09-08T15:00:00+08:00"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.due.Format(time.RFC3339); got != "2026-09-08T17:00:00+08:00" {
		t.Fatalf("due = %s", got)
	}
}

func TestDeadlineAfterWindowRollsToNextDay(t *testing.T) {
	r := testRules(t)
	res, err := r.AddBusinessHours(mustParse(t, "2026-09-08T17:30:00+08:00"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.due.Format(time.RFC3339); got != "2026-09-09T10:00:00+08:00" {
		t.Fatalf("due = %s", got)
	}
}

func TestDeadlineBeforeWindowStartsAtWindow(t *testing.T) {
	r := testRules(t)
	res, err := r.AddBusinessHours(mustParse(t, "2026-09-08T07:45:00+08:00"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.due.Format(time.RFC3339); got != "2026-09-08T10:00:00+08:00" {
		t.Fatalf("due = %s", got)
	}
}

func TestDeadlineWeekendRollover(t *testing.T) {
	r := testRules(t)
	// 2026-09-25 是周五且为夹具节假日，2026-09-26/27 周末。
	res, err := r.AddBusinessHours(mustParse(t, "2026-09-24T16:30:00+08:00"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.due.Format(time.RFC3339); got != "2026-09-28T10:30:00+08:00" {
		t.Fatalf("due = %s", got)
	}
	if !res.holidayRollover {
		t.Fatal("expected holiday rollover")
	}
	var reasons []string
	for _, s := range res.skipped {
		reasons = append(reasons, s.Date+":"+s.Reason)
	}
	want := []string{"2026-09-25:holiday", "2026-09-26:weekend", "2026-09-27:weekend"}
	for i := range want {
		if reasons[i] != want[i] {
			t.Fatalf("skipped = %v, want %v", reasons, want)
		}
	}
}

func TestDeadlineNationalDayHoliday(t *testing.T) {
	r := testRules(t)
	res, err := r.AddBusinessHours(mustParse(t, "2026-09-30T16:30:00+08:00"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.due.Format(time.RFC3339); got != "2026-10-08T10:30:00+08:00" {
		t.Fatalf("due = %s", got)
	}
	if !res.holidayRollover {
		t.Fatal("expected holiday rollover")
	}
	if len(res.skipped) != 7 {
		t.Fatalf("skipped = %+v", res.skipped)
	}
}

func TestDeadlineZeroHours(t *testing.T) {
	r := testRules(t)
	res, err := r.AddBusinessHours(mustParse(t, "2026-09-08T10:00:00+08:00"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.due.Format(time.RFC3339); got != "2026-09-08T10:00:00+08:00" {
		t.Fatalf("due = %s", got)
	}
}
