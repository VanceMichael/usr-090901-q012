package domain

import (
	"fmt"
	"time"
)

// calcResult 是 AddBusinessHours 的完整计算过程。
type calcResult struct {
	due             time.Time
	skipped         []SkippedDay
	segments        []Segment
	holidayRollover bool
}

// maxCalcDays 防止夹具错误导致死循环（约三年工作日）。
const maxCalcDays = 1100

// isWorkingDay 判断某日是否为工作日（周一至周五且非节假日，以夹具为准）。
func (r *Rules) isWorkingDay(t time.Time) bool {
	if !r.weekdaySet[t.Weekday()] {
		return false
	}
	return !r.holidaySet[t.Format("2006-01-02")]
}

// skipReason 返回某日被跳过的原因：weekend 或 holiday。
func (r *Rules) skipReason(t time.Time) string {
	if !r.weekdaySet[t.Weekday()] {
		return "weekend"
	}
	return "holiday"
}

// dayStart 返回某日 00:00（夹具时区）。
func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// AddBusinessHours 从 base 起累加 hours 个营业小时，返回截止时刻及完整计算
// 过程（跨午夜自动顺延到下一工作日，跳过周末与夹具节假日）。base 落在非
// 工作时间时，从下一工作时段起算。
func (r *Rules) AddBusinessHours(base time.Time, hours int) (calcResult, error) {
	if hours < 0 {
		return calcResult{}, fmt.Errorf("营业小时数不能为负: %d", hours)
	}
	loc := r.loc
	cursor := base.In(loc)
	remaining := time.Duration(hours) * time.Hour
	res := calcResult{due: cursor}

	for day := 0; ; day++ {
		if day > maxCalcDays {
			return calcResult{}, fmt.Errorf("时限计算超过 %d 天，夹具日历可能有误", maxCalcDays)
		}
		if !r.isWorkingDay(cursor) {
			res.skipped = append(res.skipped, SkippedDay{
				Date:   cursor.Format("2006-01-02"),
				Reason: r.skipReason(cursor),
			})
			if r.skipReason(cursor) == "holiday" {
				res.holidayRollover = true
			}
			cursor = dayStart(cursor).AddDate(0, 0, 1)
			continue
		}
		start := dayStart(cursor)
		windowStart := start.Add(time.Duration(r.windowStartM) * time.Minute)
		windowEnd := start.Add(time.Duration(r.windowEndM) * time.Minute)

		if !cursor.Before(windowEnd) {
			// 当日工作窗口已结束，顺延到次日（不记为跳过日）。
			cursor = dayStart(cursor).AddDate(0, 0, 1)
			continue
		}
		if cursor.Before(windowStart) {
			cursor = windowStart
		}
		avail := windowEnd.Sub(cursor)
		if remaining <= avail {
			end := cursor.Add(remaining)
			res.segments = append(res.segments, segment(cursor, end))
			res.due = end
			return res, nil
		}
		res.segments = append(res.segments, segment(cursor, windowEnd))
		remaining -= avail
		cursor = dayStart(cursor).AddDate(0, 0, 1)
	}
}

func segment(from, to time.Time) Segment {
	return Segment{
		Date:  from.Format("2006-01-02"),
		From:  from.Format("15:04"),
		To:    to.Format("15:04"),
		Hours: fmt.Sprintf("%.2f", to.Sub(from).Hours()),
	}
}

// ComputeDeadline 计算某阶段在某回执发生时刻的截止时间，返回重算依据与
// 计算期提示（如 HOLIDAY_ROLLOVER）。工作单条目由调用方组装。
func (r *Rules) ComputeDeadline(caseID, receiptID, stage, slaKey string, slaHours int, base time.Time) (*RecalcBasis, []string, error) {
	res, err := r.AddBusinessHours(base, slaHours)
	if err != nil {
		return nil, nil, err
	}
	basis := &RecalcBasis{
		CaseID:          caseID,
		ReceiptID:       receiptID,
		Stage:           stage,
		SLAHours:        slaHours,
		BaseTime:        base.UTC(),
		ComputedDue:     res.due.UTC(),
		SkippedDays:     res.skipped,
		Segments:        res.segments,
		HolidayRollover: res.holidayRollover,
	}
	var notices []string
	if res.holidayRollover {
		notices = append(notices, ReasonHolidayRollover)
	}
	return basis, notices, nil
}
