package costtest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

const (
	recipeSeed          = 140123
	ledgerDays          = 365
	captureDays         = 8
	batchesPerDay       = 1200
	segmentsPerBatch    = 3
	charsPerSegment     = 40
	exchangesPerDay     = 15
	commitmentsPerDay   = 5
	projectCount        = 30
	weekCount           = 52
	replaceEvery        = 10
	mergeEvery          = 10
	eraseEvery          = 60
	historicalBatches   = 20
	revisionCount       = ledgerDays*2 + (ledgerDays+1)/2
	mergeCount          = (ledgerDays + mergeEvery - 1) / mergeEvery
	changesetCount      = ledgerDays*2 + revisionCount + mergeCount
	deletionCount       = (ledgerDays - captureDays + eraseEvery - 1) / eraseEvery
	openCommitmentCount = ledgerDays*3 - mergeCount
	ownerLine           = "a14"
	ownerUser           = "fixed-owner"
)

var (
	fixtureNow   = time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC)
	commitmentID = regexp.MustCompile(`- 承诺 (C-[0-9]+)（`)
	pageOptions  = domain.PageOptions{Limit: 200, MaxChars: 20000, GapMS: 600000}
)

type object = map[string]any

func dayTime(day int) time.Time {
	return fixtureNow.AddDate(0, 0, day-ledgerDays+1)
}

func daySession(day int) string { return "day-" + dayTime(day).Format(time.DateOnly) }

func encode(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

// 固定字符表和局部 PRNG 保证正文及归属可复现，不依赖进程全局随机源。
func batch(day, number int) []domain.CaptureSegment {
	rng := rand.New(rand.NewSource(recipeSeed + int64(day)*100000 + int64(number)))
	alphabet := []rune("项目讨论方案交付报价文档确认时间安排客户会议进度材料计划")
	at := dayTime(day).Truncate(24 * time.Hour).Add(8*time.Hour + time.Duration(number-1)*24*time.Second)
	out := make([]domain.CaptureSegment, segmentsPerBatch)
	for i := range out {
		text := make([]rune, charsPerSegment)
		for j := range text {
			text[j] = alphabet[rng.Intn(len(alphabet))]
		}
		out[i] = domain.CaptureSegment{Speaker: "wearer", Text: string(text), SpokenAt: at.Add(time.Duration(i) * 6 * time.Second).Format(time.RFC3339), TimeZone: "UTC"}
		if i == 1 {
			out[i].Speaker, out[i].SpeakerKey = "other", "colleague"
		}
	}
	return out
}

func citations(day, first, last int) map[string]domain.Citation {
	out := map[string]domain.Citation{}
	for number := first; number <= last; number++ {
		for i, segment := range batch(day, number) {
			ref := fmt.Sprintf("#%d.%d", number, i)
			at, _ := time.Parse(time.RFC3339, segment.SpokenAt)
			out[ref] = domain.Citation{Ref: ref, Speaker: segment.Speaker, SpeakerKey: segment.SpeakerKey, SpokenAt: at, TimeZone: "UTC"}
		}
	}
	return out
}

func invoke(book *domain.Book, session string, args any, cites map[string]domain.Citation, now time.Time, method func(*domain.Call) string, prefix string) (string, error) {
	call := domain.NewCall(book, session, encode(args), cites, now)
	return checkedCall(call, method, prefix)
}

func checkedCall(call *domain.Call, method func(*domain.Call) string, prefix string) (string, error) {
	out := method(call)
	if err := call.Err(); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out, prefix) {
		return "", fmt.Errorf("domain receipt did not start with %q: %s", prefix, out)
	}
	return out, nil
}

func source(first, last int) object {
	return object{"from": fmt.Sprintf("#%d.0", first), "to": fmt.Sprintf("#%d.2", last)}
}

// 每轮重新经领域命令构造同一 owner，防止擦除/到期的后续样本退化为幂等空操作。
// 准备事务不计入样本；不导入 Book 快照，不复制数据库表。
func generate(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.(store.Resetter).ResetOwner(ctx, ownerLine, ownerUser); err != nil {
		t.Fatal(err)
	}
	err := s.Update(ctx, ownerLine, ownerUser, func(book *domain.Book) error {
		rng := rand.New(rand.NewSource(recipeSeed))
		for day := range ledgerDays {
			now, date, session := dayTime(day), dayTime(day).Format(time.DateOnly), daySession(day)
			book.SetCaptureClock(func() time.Time { return now })
			if err := book.BindDay(date, session); err != nil {
				return err
			}
			count := historicalBatches
			if day >= ledgerDays-captureDays {
				count = batchesPerDay
			}
			for number := 1; number <= count; number++ {
				refs, err := book.AppendCaptures(date, fmt.Sprintf("batch-%04d", number), batch(day, number))
				if err != nil || len(refs) != segmentsPerBatch {
					return fmt.Errorf("generate day %d batch %d: refs=%d, error=%v", day, number, len(refs), err)
				}
			}
			// 墓碑使用未参与整理的片段，保留完整的一年账本。
			if day%eraseEvery == 0 && day < ledgerDays-captureDays {
				task, err := book.Erase(date, "20.0", "20.0", now)
				if err != nil {
					return err
				}
				if err := book.CompleteDeletionTask(task.ID, nil, now); err != nil {
					return err
				}
				session += "-rebuilt"
				if err := book.BindDay(date, session); err != nil {
					return err
				}
			}
			book.Submitted(session, "整理今天")
			exchanges, commitments := []object{}, []object{}
			for i := range exchangesPerDay {
				project := rng.Intn(projectCount)
				link := object{"match": "existing", "id": fmt.Sprintf("P-%04d", 101+project)}
				if day < projectCount/exchangesPerDay {
					project = day*exchangesPerDay + i
					link = object{"match": "new", "name": fmt.Sprintf("交付项目%02d", project)}
				}
				exchanges = append(exchanges, object{"from": fmt.Sprintf("#%d.0", i+1), "to": fmt.Sprintf("#%d.2", i+1), "title": fmt.Sprintf("交付讨论%02d", i), "summary": "核对交付材料和报价，确认下一次讨论的安排。", "projects": []object{link}, "decisions": []string{"先确认材料清单"}})
				if i < commitmentsPerDay {
					kind, index := "mine", 0
					if i == 2 {
						kind, index = "theirs", 1
					}
					commitments = append(commitments, object{"exchange": fmt.Sprintf("x%d.0", i+1), "kind": kind, "content": fmt.Sprintf("确认交付材料%02d", i), "counterpart": fmt.Sprintf("同事%02d", project), "due_date": now.AddDate(0, 0, 3+i).Format(time.DateOnly), "refs": []string{fmt.Sprintf("#%d.%d", i+1, index)}})
				}
			}
			args := object{"source": source(1, exchangesPerDay), "exchanges": object{"create": exchanges}, "commitments": object{"create": commitments}}
			file := func(args any) (string, error) {
				return invoke(book, session, args, citations(day, 1, historicalBatches), now, (*domain.Call).FileChanges, "已收下变更集")
			}
			receipt, err := file(args)
			if err != nil {
				return err
			}
			ids := commitmentID.FindAllStringSubmatch(receipt, -1)
			if len(ids) != commitmentsPerDay {
				return fmt.Errorf("expected %d assigned commitment IDs", commitmentsPerDay)
			}
			if day%replaceEvery == 0 {
				for i := range commitments {
					commitments[i]["id"] = ids[i][1]
				}
				exchanges[0]["summary"] = "重新核对交付材料和报价，修正下次讨论的安排。"
				if _, err := file(args); err != nil {
					return err
				}
			}
			// 每天第一条有两次修订，偶数日再修订一次，其他承诺保留不同状态。
			for revision := 0; revision < 2+(day+1)%2; revision++ {
				args := object{"source": source(16+revision, 16+revision), "commitments": object{"revise": []object{{"id": ids[0][1], "rev": revision + 1, "content": fmt.Sprintf("确认交付材料第%d轮", revision+2), "refs": []string{fmt.Sprintf("#%d.0", 16+revision)}}}}}
				if _, err := file(args); err != nil {
					return err
				}
			}
			if _, err := file(object{"source": source(19, 19), "commitments": object{"close": []object{{"id": ids[1][1], "rev": 1, "status": "done", "refs": []string{"#19.0"}}, {"id": ids[2][1], "rev": 1, "status": "cancelled", "refs": []string{"#19.1"}}}}}); err != nil {
				return err
			}
			if day%mergeEvery == 0 {
				if _, err := file(object{"source": object{"day": true}, "exchanges": object{"merge": []object{{"into": "x14.0", "from": []string{"x15.0"}}}}, "commitments": object{"merge": []object{{"into": ids[3][1], "from": []string{ids[4][1]}, "reason": "同一次材料确认"}}}}); err != nil {
					return err
				}
			}
			if _, err := invoke(book, session, object{"day": object{"headline": "完成交付讨论与材料核对", "highlights": []string{"明确下一次核对时间"}}, "awareness": object{"habits": []string{"确认对方理解后再约定下一步"}}}, nil, now, (*domain.Call).FileDay, "已收到"); err != nil {
				return err
			}
			if day < ledgerDays-captureDays {
				expires, err := domain.CaptureExpiresAt(date, "UTC")
				if err != nil {
					return err
				}
				if err := book.ExpireCaptures(date, expires); err != nil {
					return err
				}
			}
		}
		for project := range projectCount {
			id, session := fmt.Sprintf("P-%04d", 101+project), fmt.Sprintf("project-%02d", project)
			book.Bind(id, session)
			if _, err := invoke(book, session, object{"status": "已确认交付范围，正在核对报价和材料。", "next_steps": []string{"安排交付复核"}, "open_questions": []string{"最终清单待确认"}}, nil, fixtureNow, (*domain.Call).SetStatus, "已更新"); err != nil {
				return err
			}
		}
		for week := range weekCount {
			start := dayTime(week * 7)
			session := fmt.Sprintf("week-%02d", week)
			book.BindWeek(start.Format(time.DateOnly), session)
			if _, err := invoke(book, session, object{"habits": []domain.Habit{{Habit: "确认对方理解后再约定下一步", Days: []string{start.Format(time.DateOnly), start.AddDate(0, 0, 1).Format(time.DateOnly)}, Example: "约定材料核对时间"}}, "suggestions": []string{"继续明确下一步的责任方"}}, nil, start.AddDate(0, 0, 6), (*domain.Call).FileWeek, "已收到"); err != nil {
				return err
			}
		}
		book.SetCaptureClock(func() time.Time { return fixtureNow })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
