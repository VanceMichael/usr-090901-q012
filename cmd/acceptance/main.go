// 验收程序：从空数据库出发，按序验证协同止付工作单服务的关键行为。
// 用法：
//
//	acceptance -phase=before-restart   验证 1~6（重复回执、工作时间边界、
//	                                   材料阻塞、越权决定、期限重算、原子失败）
//	acceptance -phase=after-restart    验证 7（重启后队列顺序）
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

var (
	baseURL = envOr("APP_BASE_URL", "http://127.0.0.1:8080")
	phase   = flag.String("phase", "before-restart", "before-restart | after-restart")
	// fixedNow 让逾期判断在验收中保持确定。
	fixedNow = "2026-09-10T12:00:00+08:00"
	client   = &http.Client{Timeout: 10 * time.Second}
	checks   int
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fail(format string, args ...any) {
	fmt.Printf("  [FAIL] "+format+"\n", args...)
	os.Exit(1)
}

func pass(format string, args ...any) {
	checks++
	fmt.Printf("  [ok] "+format+"\n", args...)
}

// req 发送请求并解析 JSON 响应。
func req(method, path string, body any, headers map[string]string) (int, map[string]any) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			fail("编码请求体: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		fail("构造请求: %v", err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := client.Do(r)
	if err != nil {
		fail("请求 %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		fail("解析响应 %s %s: %v\n%s", method, path, err, string(data))
	}
	return resp.StatusCode, parsed
}

func receipt(id, caseID, typ, role, at, risk string, mats ...string) map[string]any {
	if mats == nil {
		mats = []string{}
	}
	return map[string]any{
		"receipt_id": id, "case_id": caseID, "type": typ, "actor_role": role,
		"occurred_at": at, "risk_level": risk, "material_codes": mats,
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		fail("解析时间 %s: %v", s, err)
	}
	return t
}

// expectDue 断言工作单条目的 due_at 等于期望时刻。
func expectDue(item map[string]any, want string, ctx string) {
	got, _ := item["due_at"].(string)
	if got == "" || !mustTime(got).Equal(mustTime(want)) {
		fail("%s: due_at = %v, 期望 %s", ctx, got, want)
	}
}

// worklistItem 从案件响应中取工作单。
func worklistItem(caseResp map[string]any) map[string]any {
	w, _ := caseResp["worklist"].(map[string]any)
	return w
}

func getCase(caseID string, role string) (int, map[string]any) {
	return req("GET", "/cases/"+caseID+"?now="+url.QueryEscape(fixedNow), nil, map[string]string{"X-Actor-Role": role})
}

func queueOrder() []string {
	_, resp := req("GET", "/worklist?now="+url.QueryEscape(fixedNow), nil, nil)
	items, _ := resp["items"].([]any)
	var order []string
	for _, it := range items {
		order = append(order, it.(map[string]any)["case_id"].(string))
	}
	return order
}

func waitHealthy() {
	deadline := time.Now().Add(90 * time.Second)
	for {
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			fail("服务未在 90 秒内就绪")
		}
		time.Sleep(time.Second)
	}
}

func main() {
	flag.Parse()
	waitHealthy()
	switch *phase {
	case "before-restart":
		phaseBeforeRestart()
	case "after-restart":
		phaseAfterRestart()
	default:
		fail("未知 phase %q", *phase)
	}
	fmt.Printf("阶段 %s 通过（%d 项断言）\n", *phase, checks)
}

// ---- 阶段一：重启前 ----

func phaseBeforeRestart() {
	check1DuplicateReceipt()
	check2WorkingHoursBoundary()
	check3MaterialBlocking()
	check4UnauthorizedDecision()
	check5DeadlineRecalc()
	check6AtomicFailure()
	// 记录重启前队列顺序，供阶段二对照（此处同样断言一次期望值）。
	expectQueueOrder()
}

// 1. 同一来源回执重复送达不得生成第二项工作。
func check1DuplicateReceipt() {
	fmt.Println("检查 1：重复回执幂等")
	st, resp := req("POST", "/receipts", receipt("risk-1", "case-889", "risk_registered", "teller",
		"2026-09-08T10:54:00+08:00", "high", "transaction_summary", "customer_contact"), nil)
	if st != 200 || resp["status"] != "applied" || resp["duplicate"] != false {
		fail("首次登记: HTTP %d, %v", st, resp)
	}
	pass("首次登记 risk-1 已受理")

	st, resp = req("POST", "/receipts", receipt("risk-1", "case-889", "risk_registered", "teller",
		"2026-09-08T10:54:00+08:00", "high", "transaction_summary", "customer_contact"), nil)
	if st != 200 || resp["status"] != "duplicate" || resp["duplicate"] != true || resp["stored_status"] != "applied" {
		fail("重复送达: HTTP %d, %v", st, resp)
	}
	pass("重复送达返回 duplicate，未生成第二项工作")

	_, c := getCase("case-889", "anti_fraud_officer")
	receipts, _ := c["receipts"].([]any)
	if len(receipts) != 1 {
		fail("case-889 回执数 = %d, 期望 1", len(receipts))
	}
	pass("case-889 仅持久化 1 条原始回执")

	// 角色裁剪：柜员只见金额分档，反诈中心见全量，复核见打码联系方式。
	_, teller := getCase("case-889", "teller")
	if teller["amount_band"] != "100万-500万" || teller["amount_cents"] != nil || teller["customer_contact"] != nil {
		fail("柜员视角裁剪错误: %v", teller)
	}
	_, officer := getCase("case-889", "anti_fraud_officer")
	if officer["amount_cents"] != 1.88e8 || officer["customer_contact"] != "+86-138-0000-1234" {
		fail("反诈视角裁剪错误: %v", officer)
	}
	_, reviewer := getCase("case-889", "bank_reviewer")
	if reviewer["amount_cents"] != 1.88e8 || reviewer["customer_contact"] != "+86-****34" {
		fail("复核视角裁剪错误: %v", reviewer)
	}
	pass("敏感金额与联系方式按角色裁剪")
}

// 2. 跨午夜与节假日时限按夹具日历计算。
func check2WorkingHoursBoundary() {
	fmt.Println("检查 2：工作时间边界（跨午夜与节假日顺延）")
	// 周二 16:30 + 2 营业小时 → 当日 30 分钟 + 次日 1.5 小时。
	st, resp := req("POST", "/receipts", receipt("risk-901", "case-901", "risk_registered", "teller",
		"2026-09-08T16:30:00+08:00", "high", "transaction_summary"), nil)
	if st != 200 {
		fail("登记 case-901: HTTP %d, %v", st, resp)
	}
	expectDue(worklistItem(resp["case"].(map[string]any)), "2026-09-09T10:30:00+08:00", "case-901 跨午夜")
	pass("case-901 跨午夜顺延至次日 10:30")

	// 国庆夹具假期：9/30 16:30 + 2h → 10/8 10:30，跳过 7 个非工作日。
	st, resp = req("POST", "/receipts", receipt("risk-902", "case-902", "risk_registered", "anti_fraud_officer",
		"2026-09-30T16:30:00+08:00", "high", "transaction_summary"), nil)
	if st != 200 {
		fail("登记 case-902: HTTP %d, %v", st, resp)
	}
	w := worklistItem(resp["case"].(map[string]any))
	expectDue(w, "2026-10-08T10:30:00+08:00", "case-902 节假日顺延")
	notices, _ := w["notices"].([]any)
	if len(notices) != 1 || notices[0] != "HOLIDAY_ROLLOVER" {
		fail("case-902 缺少 HOLIDAY_ROLLOVER 提示: %v", notices)
	}
	pass("case-902 节假日顺延至 10/8 10:30 并带 HOLIDAY_ROLLOVER")

	_, expl := req("GET", "/cases/case-902/deadline-explanation?now="+url.QueryEscape(fixedNow), nil, nil)
	recalcs, _ := expl["recalculations"].([]any)
	if len(recalcs) != 1 {
		fail("case-902 重算记录数 = %d", len(recalcs))
	}
	r0 := recalcs[0].(map[string]any)
	skipped, _ := r0["skipped_days"].([]any)
	if len(skipped) != 7 || r0["holiday_rollover"] != true {
		fail("case-902 跳过日 = %v", skipped)
	}
	first, _ := skipped[0].(map[string]any)
	if first["date"] != "2026-10-01" || first["reason"] != "holiday" {
		fail("case-902 首个跳过日错误: %v", first)
	}
	pass("单案时限解释列出 7 个跳过日（节假日+周末）")
}

// 3. 必需材料未齐时复核被阻塞，且阻塞期间不能下处置决定。
func check3MaterialBlocking() {
	fmt.Println("检查 3：材料阻塞")
	req("POST", "/receipts", receipt("risk-903", "case-903", "risk_registered", "teller",
		"2026-09-08T10:00:00+08:00", "high", "transaction_summary", "customer_contact"), nil)
	st, resp := req("POST", "/receipts", receipt("contact-903", "case-903", "customer_contacted", "teller",
		"2026-09-08T11:00:00+08:00", "high"), nil)
	if st != 200 {
		fail("联系回执: HTTP %d, %v", st, resp)
	}
	w := worklistItem(resp["case"].(map[string]any))
	if w["stage"] != "awaiting_review" || w["next_action"] != "review_materials" {
		fail("case-903 工作单阶段错误: %v", w)
	}
	blocking, _ := w["blocking_materials"].([]any)
	if len(blocking) != 1 || blocking[0] != "id_verification" {
		fail("case-903 阻塞材料 = %v", blocking)
	}
	reasons, _ := w["reasons"].([]any)
	if !contains(reasons, "MATERIAL_BLOCKED") {
		fail("case-903 缺少 MATERIAL_BLOCKED: %v", reasons)
	}
	pass("case-903 复核被 id_verification 阻塞（MATERIAL_BLOCKED）")

	st, resp = req("POST", "/receipts", receipt("dec-903", "case-903", "decision_escalate", "bank_reviewer",
		"2026-09-08T12:00:00+08:00", "high"), nil)
	if st != 409 || resp["reason"] != "MATERIAL_BLOCKED" {
		fail("阻塞期决定: HTTP %d, %v", st, resp)
	}
	pass("阻塞期间处置决定被拒绝（409 MATERIAL_BLOCKED）")

	// 模拟补齐：不落库地预览变化。
	st, sim := req("POST", "/cases/case-903/simulate-material",
		map[string]any{"material_code": "id_verification", "now": fixedNow}, nil)
	if st != 200 || sim["would_unblock"] != true {
		fail("模拟补齐: HTTP %d, %v", st, sim)
	}
	simItem, _ := sim["simulated"].(map[string]any)
	if simItem["stage"] != "awaiting_decision" || simItem["next_action"] != "decide_release_or_escalate" {
		fail("模拟结果阶段错误: %v", simItem)
	}
	expectDue(simItem, "2026-09-11T12:00:00+08:00", "模拟补齐后期限")
	_, after := getCase("case-903", "bank_reviewer")
	if after["stage"] != "awaiting_review" {
		fail("模拟不应改变实际状态: %v", after["stage"])
	}
	pass("模拟补齐材料预览进入处置决定阶段，且未落库")
}

// 4. 权限不足的决定不能改变工作单。
func check4UnauthorizedDecision() {
	fmt.Println("检查 4：越权决定")
	req("POST", "/receipts", receipt("risk-904", "case-904", "risk_registered", "teller",
		"2026-09-08T09:30:00+08:00", "medium", "transaction_summary", "customer_contact"), nil)
	st, resp := req("POST", "/receipts", receipt("contact-904", "case-904", "customer_contacted", "bank_reviewer",
		"2026-09-08T10:00:00+08:00", "medium"), nil)
	if st != 200 {
		fail("联系回执: HTTP %d, %v", st, resp)
	}
	w := worklistItem(resp["case"].(map[string]any))
	if w["stage"] != "awaiting_decision" {
		fail("case-904 材料齐全应进入处置决定: %v", w)
	}
	expectDue(w, "2026-09-11T10:00:00+08:00", "case-904 处置期限")
	pass("case-904 材料齐全，联系后直接进入处置决定（期限 9/11 10:00）")

	st, resp = req("POST", "/receipts", receipt("dec-904-bad", "case-904", "decision_release", "teller",
		"2026-09-08T11:00:00+08:00", "medium"), nil)
	if st != 422 || resp["reason"] != "ROLE_NOT_ALLOWED" {
		fail("越权决定: HTTP %d, %v", st, resp)
	}
	w = worklistItem(resp["case"].(map[string]any))
	if w == nil || w["stage"] != "awaiting_decision" {
		fail("越权决定后工作单被改变: %v", w)
	}
	expectDue(w, "2026-09-11T10:00:00+08:00", "越权决定后期限")
	pass("柜员解除止付被拒（422 ROLE_NOT_ALLOWED），工作单未变")

	st, resp = req("POST", "/receipts", receipt("dec-904", "case-904", "decision_release", "anti_fraud_officer",
		"2026-09-08T11:30:00+08:00", "medium"), nil)
	if st != 200 || resp["status"] != "applied" {
		fail("合规决定: HTTP %d, %v", st, resp)
	}
	c := resp["case"].(map[string]any)
	if c["stage"] != "closed" || c["decision"] != "release" || c["worklist"] != nil {
		fail("结案状态错误: %v", c)
	}
	pass("反诈中心解除止付，案件结案并移出工作单")
}

// 5. 材料补齐触发期限重算，且重算依据可解释。
func check5DeadlineRecalc() {
	fmt.Println("检查 5：期限重算")
	req("POST", "/receipts", receipt("risk-905", "case-905", "risk_registered", "teller",
		"2026-09-08T09:00:00+08:00", "high", "transaction_summary"), nil)
	st, resp := req("POST", "/receipts", receipt("contact-905", "case-905", "customer_contacted", "teller",
		"2026-09-08T09:30:00+08:00", "high"), nil)
	if st != 200 {
		fail("联系回执: HTTP %d, %v", st, resp)
	}
	w := worklistItem(resp["case"].(map[string]any))
	blocking, _ := w["blocking_materials"].([]any)
	if len(blocking) != 2 {
		fail("case-905 阻塞材料 = %v", blocking)
	}

	st, resp = req("POST", "/receipts", receipt("mat-905-a", "case-905", "material_submitted", "bank_reviewer",
		"2026-09-08T14:00:00+08:00", "high", "customer_contact"), nil)
	if st != 200 {
		fail("补材料 a: HTTP %d, %v", st, resp)
	}
	w = worklistItem(resp["case"].(map[string]any))
	if w["stage"] != "awaiting_review" {
		fail("case-905 仍应处于复核: %v", w)
	}
	pass("case-905 补一份材料后仍阻塞（未重算）")

	st, resp = req("POST", "/receipts", receipt("mat-905-b", "case-905", "material_submitted", "teller",
		"2026-09-09T10:00:00+08:00", "high", "id_verification"), nil)
	if st != 200 {
		fail("补材料 b: HTTP %d, %v", st, resp)
	}
	w = worklistItem(resp["case"].(map[string]any))
	if w["stage"] != "awaiting_decision" || w["responsible_org"] != "anti_fraud_center" {
		fail("case-905 重算后工作单错误: %v", w)
	}
	expectDue(w, "2026-09-10T10:00:00+08:00", "case-905 重算后期限")
	pass("case-905 材料补齐，期限重算为 9/10 10:00（反诈中心）")

	_, expl := req("GET", "/cases/case-905/deadline-explanation?now="+url.QueryEscape(fixedNow), nil, nil)
	recalcs, _ := expl["recalculations"].([]any)
	if len(recalcs) != 3 {
		fail("case-905 重算记录数 = %d, 期望 3", len(recalcs))
	}
	last := recalcs[2].(map[string]any)
	if last["receipt_id"] != "mat-905-b" || last["sla_business_hours"] != 8.0 {
		fail("末次重算依据错误: %v", last)
	}
	if !mustTime(last["base_time"].(string)).Equal(mustTime("2026-09-09T10:00:00+08:00")) {
		fail("末次重算起算点错误: %v", last["base_time"])
	}
	if !mustTime(last["computed_due"].(string)).Equal(mustTime("2026-09-10T10:00:00+08:00")) {
		fail("末次重算结果错误: %v", last["computed_due"])
	}
	pass("期限重算依据完整（3 次：联系/复核/处置）")
}

// 6. 原始回执、工作单、重算依据在失败时一起回滚。
func check6AtomicFailure() {
	fmt.Println("检查 6：原子失败回滚")
	st, resp := req("POST", "/receipts", receipt("risk-906", "case-906", "risk_registered", "teller",
		"2026-09-08T15:00:00+08:00", "high", "transaction_summary"),
		map[string]string{"X-Fault-Inject": "rollback"})
	if st != 500 || resp["error"] != "injected_rollback" {
		fail("故障注入: HTTP %d, %v", st, resp)
	}
	pass("故障注入返回 500")

	st, _ = req("GET", "/cases/case-906", nil, nil)
	if st != 404 {
		fail("回滚后 case-906 不应存在: HTTP %d", st)
	}
	for _, id := range queueOrder() {
		if id == "case-906" {
			fail("回滚后工作单仍含 case-906")
		}
	}
	pass("回滚后无案件、无工作单、无重算依据")

	// 同一 receipt_id 可重新送达并成功 —— 证明回执行也随事务回滚。
	st, resp = req("POST", "/receipts", receipt("risk-906", "case-906", "risk_registered", "teller",
		"2026-09-08T15:00:00+08:00", "high", "transaction_summary"), nil)
	if st != 200 || resp["status"] != "applied" {
		fail("重发 risk-906: HTTP %d, %v", st, resp)
	}
	_, c := getCase("case-906", "teller")
	receipts, _ := c["receipts"].([]any)
	if len(receipts) != 1 {
		fail("case-906 回执数 = %d, 期望 1", len(receipts))
	}
	pass("同一回执重发成功，三者原子提交")
}

// 期望的重启前队列顺序（now = 2026-09-10T12:00+08:00）。
func expectedQueue() []string {
	return []string{"case-889", "case-903", "case-906", "case-901", "case-905", "case-902"}
}

func expectQueueOrder() {
	order := queueOrder()
	want := expectedQueue()
	if strings.Join(order, ",") != strings.Join(want, ",") {
		fail("队列顺序 = %v, 期望 %v", order, want)
	}
	pass(fmt.Sprintf("工作单按紧迫度排序: %s", strings.Join(order, " → ")))
}

// ---- 阶段二：重启后 ----

func phaseAfterRestart() {
	fmt.Println("检查 7：重启后队列顺序保持")
	expectQueueOrder()

	// 机构积压汇总在重启后同样可解释。
	_, resp := req("GET", "/backlog?now="+url.QueryEscape(fixedNow), nil, nil)
	orgs, _ := resp["orgs"].([]any)
	got := map[string]map[string]any{}
	for _, o := range orgs {
		m := o.(map[string]any)
		got[m["org"].(string)] = m
	}
	type want struct{ open, overdue float64 }
	expect := map[string]want{
		"branch_outlet":     {4, 3},
		"bank_backoffice":   {1, 1},
		"anti_fraud_center": {1, 1},
	}
	for org, w := range expect {
		m, ok := got[org]
		if !ok {
			fail("积压汇总缺少机构 %s", org)
		}
		if m["open"] != w.open || m["overdue"] != w.overdue {
			fail("机构 %s 积压 = open %v overdue %v, 期望 %v", org, m["open"], m["overdue"], w)
		}
	}
	pass("机构积压汇总正确（网点 4/3、后台 1/1、反诈 1/1）")

	// 逾期原因在固定 now 下可重现。
	_, resp = req("GET", "/worklist?now="+url.QueryEscape(fixedNow), nil, nil)
	items, _ := resp["items"].([]any)
	reasonsByCase := map[string][]string{}
	for _, it := range items {
		m := it.(map[string]any)
		var rs []string
		for _, r := range m["reasons"].([]any) {
			rs = append(rs, r.(string))
		}
		sort.Strings(rs)
		reasonsByCase[m["case_id"].(string)] = rs
	}
	if got := reasonsByCase["case-889"]; strings.Join(got, ",") != "CONTACT_DUE" {
		fail("case-889 逾期原因 = %v", got)
	}
	if got := reasonsByCase["case-903"]; strings.Join(got, ",") != "ACTION_OVERDUE,MATERIAL_BLOCKED" {
		fail("case-903 逾期原因 = %v", got)
	}
	if got := reasonsByCase["case-902"]; len(got) != 0 {
		fail("case-902 不应逾期: %v", got)
	}
	pass("逾期原因按夹具日历与固定 now 计算")
}

func contains(list []any, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
