package domain

import (
	"fmt"
	"iter"
	"regexp"
	"slices"
	"strings"
	"time"
)

// MemoryCandidate 只保存供用户选择的 ID 与摘要；不保存原文引用或内核对象。
type MemoryCandidate struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// DeletionTask 与原文擦除同事务持久化；通知只是唤醒，启动恢复仍须查询 pending。
type DeletionTask struct {
	ID             string            `json:"id"`
	TimeZone       string            `json:"time_zone,omitempty"`
	RebuildPending bool              `json:"rebuild_pending,omitempty"`
	Kind           string            `json:"kind"`
	Date           string            `json:"date"`
	Session        string            `json:"session"`
	Sessions       []string          `json:"sessions"`
	DeleteAt       time.Time         `json:"delete_at,omitzero"`
	Status         string            `json:"status"`
	CreatedAt      time.Time         `json:"created_at"`
	Candidates     []MemoryCandidate `json:"candidates"`
	CompletedAt    time.Time         `json:"completed_at,omitzero"`
	Error          string            `json:"error,omitempty"`
	FailedAt       time.Time         `json:"failed_at,omitzero"`
}

// Erase 在所属 Book 事务内删除闭区间；参数接受页中的编号以及带 # 的引用。
func (u *Book) Erase(date, from, to string, now time.Time) (DeletionTask, error) {
	u.SetCaptureClock(func() time.Time { return now })
	day := u.captureDays[date]
	if day == nil {
		return DeletionTask{}, fmt.Errorf("business day does not exist")
	}
	if day.Deleted {
		return DeletionTask{}, ErrDayDeleted
	}
	f, ok1 := parseCaptureRef(strings.TrimPrefix(from, "#"))
	t, ok2 := parseCaptureRef(strings.TrimPrefix(to, "#"))
	if !ok1 || !ok2 || !refLE(f, t) {
		return DeletionTask{}, fmt.Errorf("invalid erasure range")
	}
	for _, ref := range []Ref{f, t} {
		found, err := u.containsRef(date, ref)
		if err != nil {
			return DeletionTask{}, err
		}
		if !found {
			return DeletionTask{}, fmt.Errorf("invalid erasure range")
		}
	}
	return u.erase(date, f, t, false, now)
}

// EraseDay 也接受尚无片段的业务日；重复请求返回同一待办，不重新开放当天。
func (u *Book) EraseDay(date string, now time.Time) (DeletionTask, error) {
	u.SetCaptureClock(func() time.Time { return now })
	day := u.captureDays[date]
	if day == nil {
		return DeletionTask{}, fmt.Errorf("business day does not exist")
	}
	if day.Deleted {
		for _, task := range u.deletionTasks {
			if task.Date == date && task.Kind == "day" {
				return cloneDeletionTask(task), nil
			}
		}
		return DeletionTask{}, ErrDayDeleted
	}
	access, err := u.captureAccess()
	if err != nil {
		return DeletionTask{}, err
	}
	if u.CheckCaptures(date) == ErrCapturesExpired {
		return u.erase(date, Ref{}, Ref{}, true, now)
	}
	first, _, err := access.Adjacent(date, Ref{}, false)
	if err != nil {
		return DeletionTask{}, err
	}
	last, _, err := access.Adjacent(date, Ref{}, true)
	if err != nil {
		return DeletionTask{}, err
	}
	return u.erase(date, first.Ref, last.Ref, true, now)
}

type captureRange struct{ from, to Ref }

func (r captureRange) contains(ref Ref) bool { return refLE(r.from, ref) && refLE(ref, r.to) }
func (r captureRange) intersects(other captureRange) bool {
	return refLE(r.from, other.to) && refLE(other.from, r.to)
}
func sourceRange(from, to string) (captureRange, bool) {
	f, ok1 := parseRef(from)
	t, ok2 := parseRef(to)
	return captureRange{f, t}, ok1 && ok2 && refLE(f, t)
}

// captureRanges 与处理进度采用相同间隔；没有时间的片段保守地留在同一时段。
func captureRanges(segs iter.Seq2[captureSegment, error], gap time.Duration) ([]captureRange, error) {
	var out []captureRange
	var prev CaptureSegment
	for s, err := range segs {
		if err != nil {
			return nil, err
		}
		if len(out) == 0 || captureApart(prev, s.g, gap) {
			out = append(out, captureRange{from: s.ref})
		}
		out[len(out)-1].to = s.ref
		prev = s.g
	}
	return out, nil
}

func (u *Book) erase(date string, from, to Ref, whole bool, now time.Time) (DeletionTask, error) {
	day := u.captureDays[date]
	if err := day.checkCaptureRevision(); err != nil {
		return DeletionTask{}, err
	}
	zone := u.BusinessDay(date).TimeZone
	erased := captureRange{from, to}
	gap := time.Duration(day.GapMS) * time.Millisecond
	var affected []captureRange
	var spans []captureRange
	if !whole {
		var err error
		spans, err = captureRanges(u.captureSegments(date, Ref{}, Ref{}), gap)
		if err != nil {
			return DeletionTask{}, err
		}
	}
	for _, span := range spans {
		if span.intersects(erased) {
			affected = append(affected, span)
		}
	}
	removed := u.erasureChangesets(date, erased, whole)
	kept := make([]Changeset, 0, len(u.changesets))
	for i, cs := range u.changesets {
		if !removed[i] {
			kept = append(kept, cs)
			continue
		}
		if u.changesetDay(cs) == date {
			if r, ok := sourceRange(cs.Source.From, cs.Source.To); ok {
				affected = append(affected, r)
			}
		}
	}
	access, err := u.captureAccess()
	if err != nil {
		return DeletionTask{}, err
	}
	if err := access.Erase(date, from, to); err != nil {
		return DeletionTask{}, err
	}
	if whole {
		day.Tail = Ref{}
	} else {
		last, _, err := access.Adjacent(date, Ref{}, true)
		if err != nil {
			return DeletionTask{}, err
		}
		day.Tail = last.Ref
	}
	if !whole {
		if err := u.rewindCaptures(date, affected, gap); err != nil {
			return DeletionTask{}, err
		}
	}
	if from.Seq != 0 {
		day.Tombstones = append(day.Tombstones, Source{From: "#" + from.String(), To: "#" + to.String()})
	}
	u.changesets = kept
	// 展示日期相同的旧 Session 并不属于这个业务日，擦除只沿实际绑定传播。
	u.days = slices.DeleteFunc(u.days, func(d Day) bool {
		bound, _ := u.sessionDay(d.Session)
		return bound == date
	})
	u.invalidateDayFact(date)
	if whole {
		day.Progress, day.Deleted = "0", true
	}
	day.CaptureRevision++
	day.ReadThrough = Ref{}
	sequence := 1
	for _, task := range u.deletionTasks {
		if task.Date == date {
			sequence++
		}
	}
	kind := "erase"
	if whole {
		kind = "day"
	}
	task := DeletionTask{ID: fmt.Sprintf("del_%s_%d", date, sequence), Kind: kind, Date: date,
		Session: day.Session, Sessions: orEmpty(slices.Clone(day.Sessions)), DeleteAt: day.DeleteAt,
		TimeZone: zone, Status: "pending", CreatedAt: now, Candidates: []MemoryCandidate{}}
	day.Session = ""
	u.deletionTasks = append(u.deletionTasks, task)
	u.refold()
	return cloneDeletionTask(task), nil
}

func (u *Book) rewindCaptures(date string, affected []captureRange, gap time.Duration) error {
	d := u.captureDays[date]
	if len(affected) == 0 {
		return nil
	}
	// 整个旧时段消失时仍以旧起点回退；剩余片段合并成更早开始的时段时继续向前找。
	first := affected[0].from
	for _, r := range affected {
		if refLE(r.from, first) {
			first = r.from
		}
	}
	spans, err := captureRanges(u.captureSegments(date, Ref{}, Ref{}), gap)
	if err != nil {
		return err
	}
	for _, span := range spans {
		if slices.ContainsFunc(affected, span.intersects) && refLE(span.from, first) {
			first = span.from
		}
	}
	access, err := u.captureAccess()
	if err != nil {
		return err
	}
	previous, _, err := access.Adjacent(date, first, true)
	if err != nil {
		return err
	}
	at, _ := parseCaptureCursor(d.Progress)
	if refLE(previous.Ref, at.Last) {
		d.Progress = (captureCursor{Last: previous.Ref}).String()
	}
	return nil
}

func (u *Book) changesetDay(cs Changeset) string {
	if cs.Day != "" {
		return cs.Day
	}
	date, _ := u.sessionDay(cs.Session)
	return date
}

// erasureChangesets 求依据的传递闭包，先确定要删的事实，再一次性重放。
// 用户操作没有原文，保留日志；其目标被移除时由 fold 自然跳过。
func (u *Book) erasureChangesets(date string, erased captureRange, whole bool) []bool {
	removed := make([]bool, len(u.changesets))
	// 此次直接受影响的交谈都属于同一业务日，裸 ID 只在该日内比较。
	exchanges := map[string]bool{}
	refsHit := func(refs []string) bool {
		return slices.ContainsFunc(refs, func(s string) bool {
			r, ok := parseRef(s)
			return ok && erased.contains(r)
		})
	}
	for i, cs := range u.changesets {
		if cs.Author == AuthorUser {
			continue
		}
		local := u.changesetDay(cs) == date
		if !local && cs.Source.Date != date {
			continue
		}
		hit := whole && local
		if r, ok := sourceRange(cs.Source.From, cs.Source.To); ok && local && erased.intersects(r) {
			hit = true
		}
		for _, e := range cs.CreateExchanges {
			if r, ok := sourceRange(e.From, e.To); ok && local && erased.intersects(r) {
				hit = true
			}
		}
		for _, m := range cs.CreateCommitments {
			hit = hit || refsHit(m.Refs)
		}
		for _, changes := range [][]CommitmentChange{cs.ReviseCommitments, cs.CloseCommitments} {
			for _, change := range changes {
				hit = hit || refsHit(change.Refs)
			}
		}
		removed[i] = hit
	}
	for {
		// 修订或合并被撤销也会改变摘要，依赖旧摘要的项目修订不能继续保留。
		for i, cs := range u.changesets {
			if !removed[i] || u.changesetDay(cs) != date {
				continue
			}
			for _, e := range cs.CreateExchanges {
				exchanges[e.ID] = true
			}
			for _, r := range cs.ReviseExchanges {
				exchanges[r.ID] = true
			}
			for _, m := range cs.MergeExchanges {
				exchanges[m.Into] = true
				for _, id := range m.From {
					exchanges[id] = true
				}
			}
		}
		changed := false
		for i, cs := range u.changesets {
			if removed[i] || cs.Author == AuthorUser {
				continue
			}
			hit := false
			if u.changesetDay(cs) == date {
				for _, r := range cs.ReviseExchanges {
					hit = hit || exchanges[r.ID]
				}
				for _, m := range cs.MergeExchanges {
					hit = hit || exchanges[m.Into] || slices.ContainsFunc(m.From, func(id string) bool { return exchanges[id] })
				}
				// 新建承诺的 Exchange 只是出处，依据是上面已检查的 Refs。
				// 摘要改动被撤销不能反向删除仍有原文依据的创建变更集。
			}
			if u.changesetDay(cs) == date || cs.Source.Date == date {
				for _, changes := range [][]CommitmentChange{cs.ReviseCommitments, cs.CloseCommitments} {
					for _, change := range changes {
						hit = hit || slices.ContainsFunc(change.Exchanges, func(id string) bool { return exchanges[id] })
					}
				}
			}
			if hit {
				removed[i], changed = true, true
			}
		}
		if !changed {
			return removed
		}
	}
}

func cloneDeletionTask(task DeletionTask) DeletionTask {
	task.Sessions = slices.Clone(task.Sessions)
	task.Candidates = slices.Clone(task.Candidates)
	return task
}

func (u *Book) DeletionTasks() []DeletionTask {
	out := make([]DeletionTask, 0, len(u.deletionTasks))
	for _, task := range u.deletionTasks {
		out = append(out, cloneDeletionTask(task))
	}
	return out
}

// PendingDeletionTasks 包含清理已完成、但重新整理尚未确认的待办，供启动恢复发现。
func (u *Book) PendingDeletionTasks() []DeletionTask {
	return slices.DeleteFunc(u.DeletionTasks(), func(task DeletionTask) bool { return task.Status != "pending" && !task.RebuildPending })
}

func (u *Book) CompleteDeletionTask(id string, candidates []MemoryCandidate, now time.Time) error {
	for i := range u.deletionTasks {
		task := &u.deletionTasks[i]
		if task.ID == id {
			if task.Status != "completed" {
				task.Candidates = orEmpty(slices.Clone(candidates))
				task.Status, task.CompletedAt = "completed", now
				task.Error, task.FailedAt = "", time.Time{}
			}
			return nil
		}
	}
	return fmt.Errorf("deletion task does not exist")
}

var deletionErrorClass = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// FailDeletionTask 不收错误原文，只收调用方分类后的短代码，避免远端错误夹带个人数据。
func (u *Book) FailDeletionTask(id, errorClass string, now time.Time) error {
	if !deletionErrorClass.MatchString(errorClass) {
		return fmt.Errorf("invalid deletion error class")
	}
	for i := range u.deletionTasks {
		task := &u.deletionTasks[i]
		if task.ID == id {
			if task.Status == "pending" || task.RebuildPending {
				task.Error, task.FailedAt = errorClass, now
			}
			return nil
		}
	}
	return fmt.Errorf("deletion task does not exist")
}

// CompleteDeletionWithRebuild 把清理完成与后续整理意图同事务记录，避免两者之间崩溃漏掉整理。
func (u *Book) CompleteDeletionWithRebuild(id string, candidates []MemoryCandidate, rebuild bool, now time.Time) error {
	for i := range u.deletionTasks {
		task := &u.deletionTasks[i]
		if task.ID == id && task.Status == "pending" {
			if rebuild && task.Kind != "erase" {
				return fmt.Errorf("only erasure can rebuild")
			}
			task.RebuildPending = rebuild
		}
	}
	return u.CompleteDeletionTask(id, candidates, now)
}

func (u *Book) CompleteDeletionRebuild(id string) error {
	for i := range u.deletionTasks {
		task := &u.deletionTasks[i]
		if task.ID == id {
			if task.Status != "completed" {
				return fmt.Errorf("deletion is not completed")
			}
			task.RebuildPending = false
			task.Error, task.FailedAt = "", time.Time{}
			return nil
		}
	}
	return fmt.Errorf("deletion task does not exist")
}
