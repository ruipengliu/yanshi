package domain

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"
)

// CaptureSegment 是提交方交给 Persona 的原文；审核在平台读取结构化页时进行。
type CaptureSegment struct {
	Speaker    string `json:"speaker"`
	SpeakerKey string `json:"speaker_key,omitempty"`
	Text       string `json:"text"`
	SpokenAt   string `json:"spoken_at,omitempty"`
	TimeZone   string `json:"time_zone,omitempty"`
}

type captureBatch struct {
	ID       string
	Number   uint64
	Segments []CaptureSegment
}

// captureDay 独立于 file_day 的总览，换绑工作 Session 不改变引用与处理进度。
type captureDay struct {
	Session     string
	Batches     []captureBatch
	Next        uint64
	Progress    string
	ReadThrough Ref
	GapMS       int64
	GapSet      bool
}

func (u *Book) captureDay(date string) *captureDay {
	if u.captureDays == nil {
		u.captureDays = map[string]*captureDay{}
	}
	if u.captureDays[date] == nil {
		u.captureDays[date] = &captureDay{Progress: "0"}
	}
	return u.captureDays[date]
}

func (u *Book) boundDay(session string) (string, *captureDay) {
	if session != "" {
		for date, day := range u.captureDays {
			if day.Session == session {
				return date, day
			}
		}
	}
	return "", nil
}

// BindDay 在存储事务中绑定当天工作 Session；一个 Session 不能同时代表两个业务日。
func (u *Book) BindDay(date, session string) error {
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return fmt.Errorf("date must be YYYY-MM-DD")
	}
	if session == "" {
		return fmt.Errorf("session must not be empty")
	}
	if bound, _ := u.boundDay(session); bound != "" && bound != date {
		return fmt.Errorf("session is bound to another business day")
	}
	day := u.captureDay(date)
	if day.Session != session {
		// 换绑后的模型没有见过旧 Session 的页，不能继承它的已读范围。
		day.ReadThrough = Ref{}
		day.Session = session
	}
	return nil
}

var captureSpeakerKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// AppendCaptures 与内核 CheckCapture/SubmitCaptures 使用同样的提交边界；不回显原文或非法字段。
// capture_id 的去重保留整个业务日，后续擦除也不能复用批次号。
func (u *Book) AppendCaptures(date, captureID string, segments []CaptureSegment) ([]string, error) {
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return nil, fmt.Errorf("date must be YYYY-MM-DD")
	}
	if captureID == "" || len(captureID) > 128 {
		return nil, fmt.Errorf("capture_id must contain 1 to 128 bytes")
	}
	if len(segments) == 0 || len(segments) > 200 {
		return nil, fmt.Errorf("captures must contain 1 to 200 segments")
	}
	for _, g := range segments {
		if g.Speaker != "wearer" && g.Speaker != "other" {
			return nil, fmt.Errorf("invalid capture speaker")
		}
		if g.Speaker == "wearer" && g.SpeakerKey != "" || g.Speaker == "other" && !captureSpeakerKey.MatchString(g.SpeakerKey) {
			return nil, fmt.Errorf("invalid capture speaker_key")
		}
		if g.Text == "" || utf8.RuneCountInString(g.Text) > 2000 {
			return nil, fmt.Errorf("capture text must contain 1 to 2000 characters")
		}
		if g.SpokenAt != "" {
			at, err := time.Parse(time.RFC3339Nano, g.SpokenAt)
			if err != nil || at.UTC().Year() < 1 || at.UTC().Year() > 9999 {
				return nil, fmt.Errorf("invalid capture spoken_at")
			}
		}
		if g.TimeZone != "" {
			if _, ok := captureZone(g.TimeZone); !ok {
				return nil, fmt.Errorf("invalid capture time_zone")
			}
		}
	}
	day := u.captureDays[date]
	if day != nil {
		for _, b := range day.Batches {
			if b.ID == captureID {
				if !slices.Equal(b.Segments, segments) {
					return nil, fmt.Errorf("capture_id already has different content")
				}
				return batchRefs(b), nil
			}
		}
		if day.Next == math.MaxUint64 {
			return nil, fmt.Errorf("capture batch numbers exhausted")
		}
	}
	day = u.captureDay(date)
	day.Next++
	batch := captureBatch{ID: captureID, Number: day.Next, Segments: slices.Clone(segments)}
	day.Batches = append(day.Batches, batch)
	return batchRefs(batch), nil
}

func batchRefs(b captureBatch) []string {
	refs := make([]string, len(b.Segments))
	for i := range refs {
		refs[i] = (Ref{Seq: b.Number, Index: i}).String()
	}
	return refs
}

func (d *captureDay) segments() []captureSegment {
	var out []captureSegment
	for _, b := range d.Batches {
		for i, g := range b.Segments {
			out = append(out, captureSegment{ref: Ref{Seq: b.Number, Index: i}, g: g})
		}
	}
	return out
}

// containsRef 只检查端点是否存在，不要求范围中的批次连续；擦除后的空号也不能靠范围补回。
func (d *captureDay) containsRef(ref Ref) bool {
	for _, batch := range d.Batches {
		if batch.Number == ref.Seq {
			return ref.Index >= 0 && ref.Index < len(batch.Segments)
		}
	}
	return false
}

// CapturePosition 由当前工作 Session 查处理进度，避免换绑后旧 Session 继续参与评测编排。
func (u *Book) CapturePosition(session string) (string, error) {
	date, day := u.boundDay(session)
	if day == nil {
		return "", fmt.Errorf("session is not bound to a business day")
	}
	return u.CaptureProgress(date), nil
}

// CaptureProgress 只暴露处理位置，不把原文塞进评测镜像 State。
func (u *Book) CaptureProgress(date string) string {
	if day := u.captureDays[date]; day != nil {
		return day.Progress
	}
	return "0"
}

// advanceCaptures 与变更集 commit 同处 Book 事务，只提交当前范围完整覆盖的连续时段。
// 不拼接不同变更集的部分范围，且时段末尾不能超过当前工作 Session 实际返回过的页尾。
func (c *Call) advanceCaptures(from, to Ref) {
	_, day := c.u.boundDay(c.session)
	if day == nil || !day.GapSet {
		return
	}
	at, _ := parseCaptureCursor(day.Progress)
	segs := day.segments()
	start := 0
	for start < len(segs) && refLE(segs[start].ref, at.Last) {
		start++
	}
	gap := time.Duration(day.GapMS) * time.Millisecond
	for start < len(segs) {
		end := start
		for end+1 < len(segs) && !captureApart(segs[end].g, segs[end+1].g, gap) {
			end++
		}
		if !refLE(from, segs[start].ref) || !refLE(segs[end].ref, to) || !refLE(segs[end].ref, day.ReadThrough) {
			return
		}
		day.Progress = segs[end].ref.String()
		start = end + 1
	}
}
