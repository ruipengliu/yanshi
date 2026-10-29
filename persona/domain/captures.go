package domain

import (
	"errors"
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
	SpeakerRef string `json:"speaker_ref,omitempty"`
	Text       string `json:"text"`
	SpokenAt   string `json:"spoken_at,omitempty"`
	TimeZone   string `json:"time_zone,omitempty"`
}

// captureDay 独立于 file_day 的总览，换绑工作 Session 不改变引用与处理进度。
type captureDay struct {
	CaptureGeneration uint64
	LastBatchAt       time.Time
	Tail              Ref
	Session           string
	Sessions          []string
	DeleteAt          time.Time
	Deleted           bool
	Tombstones        []Source
	BatchIDs          map[string]uint64
	TimeZone          string
	ExpiresAt         time.Time
	Expired           bool
	CaptureRevision   uint64
	Next              uint64
	Progress          string
	ReadThrough       Ref
	GapMS             int64
	GapSet            bool
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
func (u *Book) BindDay(date, session string, deleteAt ...time.Time) error {
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return fmt.Errorf("date must be YYYY-MM-DD")
	}
	if session == "" {
		return fmt.Errorf("session must not be empty")
	}
	if bound, _ := u.sessionDay(session); bound != "" && bound != date {
		return fmt.Errorf("session is bound to another business day")
	}
	if err := u.CheckCaptures(date); err != nil {
		return err
	}
	day := u.captureDay(date)
	if !slices.Contains(day.Sessions, session) {
		day.Sessions = append(day.Sessions, session)
	}
	if !day.ExpiresAt.IsZero() {
		day.DeleteAt = day.ExpiresAt
	}
	if day.DeleteAt.IsZero() && len(deleteAt) > 0 && !deleteAt[0].IsZero() {
		day.DeleteAt = deleteAt[0]
	}
	if day.Session != session {
		// 换绑后的模型没有见过旧 Session 的页，不能继承它的已读范围。
		day.ReadThrough = Ref{}
		day.Session = session
	}
	// 兼容先写后绑定的夹具；不能等下次快照读取才改变变更集身份。
	for i := range u.changesets {
		if u.changesets[i].Session == session && u.changesets[i].Day == "" {
			u.changesets[i].Day = date
		}
	}
	u.refold()
	return nil
}

var captureSpeakerKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
var captureSpeakerRef = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// AppendCaptures 校验业务日原文的提交边界；错误不回显原文或非法字段。
// capture_id 的去重保留整个业务日，后续擦除也不能复用批次号。
func (u *Book) AppendCaptures(date, captureID string, segments []CaptureSegment) ([]string, error) {
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return nil, fmt.Errorf("date must be YYYY-MM-DD")
	}
	if err := u.CheckCaptures(date); err != nil {
		return nil, err
	}
	if err := validateCaptureBatch(captureID, segments); err != nil {
		return nil, err
	}
	// 首批只决定一次时区；无时区的旧采集入口沿用上海默认值。
	zone := segments[len(segments)-1].TimeZone
	if zone == "" {
		zone = DefaultCaptureTimeZone
	}
	var expiresAt time.Time
	if day := u.captureDays[date]; day != nil && !day.ExpiresAt.IsZero() {
		zone, expiresAt = day.TimeZone, day.ExpiresAt
	} else {
		var err error
		expiresAt, err = CaptureExpiresAt(date, zone)
		if err != nil {
			return nil, err
		}
	}
	if !u.captureNow().Before(expiresAt) {
		return nil, ErrCapturesExpired
	}
	access, err := u.captureAccess()
	if err != nil {
		return nil, err
	}
	day := u.captureDays[date]
	if day != nil {
		if number, ok := day.BatchIDs[captureID]; ok {
			segmentsBefore, err := access.Batch(date, number)
			if err != nil {
				return nil, err
			}
			if slices.ContainsFunc(segmentsBefore, func(g CaptureSegment) bool {
				return g.Text == ""
			}) {
				return nil, fmt.Errorf("capture_id contains erased segments")
			}
			if !slices.Equal(segmentsBefore, segments) {
				return nil, fmt.Errorf("capture_id already has different content")
			}
			return batchRefs(number, len(segments)), nil
		}
		if err := day.checkCaptureRevision(); err != nil {
			return nil, err
		}
		if day.Next == math.MaxUint64 {
			return nil, fmt.Errorf("capture batch numbers exhausted")
		}
	}
	number := uint64(1)
	if day != nil {
		number = day.Next + 1
	}
	if err := access.Append(date, number, segments); err != nil {
		return nil, err
	}
	day = u.captureDay(date)
	if day.BatchIDs == nil {
		day.BatchIDs = map[string]uint64{}
	}
	day.BatchIDs[captureID], day.Next = number, number
	day.TimeZone, day.ExpiresAt, day.DeleteAt = zone, expiresAt, expiresAt
	day.CaptureRevision++
	// 静默从服务端正式接收算起；补传不能借历史说话时间立刻触发整理。
	day.LastBatchAt = u.captureNow().UTC()
	day.Tail = Ref{Seq: number, Index: len(segments) - 1}
	return batchRefs(number, len(segments)), nil
}

func batchRefs(number uint64, count int) []string {
	refs := make([]string, count)
	for i := range refs {
		refs[i] = (Ref{Seq: number, Index: i}).String()
	}
	return refs
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
func (c *Call) advanceCaptures(from, to Ref) error {
	date, day := c.u.boundDay(c.session)
	if day == nil || !day.GapSet {
		return nil
	}
	at, _ := parseCaptureCursor(day.Progress)
	gap := time.Duration(day.GapMS) * time.Millisecond
	// 只保留时段边界，整日检查不把正文装进账本或临时切片。
	segs := func(yield func(captureSegment, error) bool) {
		for s, err := range c.u.captureSegments(date, at.Last, Ref{}) {
			if err != nil {
				yield(captureSegment{}, err)
				return
			}
			if refLE(s.ref, at.Last) {
				continue
			}
			if !yield(s, nil) {
				return
			}
		}
	}
	spans, err := captureRanges(segs, gap)
	if err != nil {
		return err
	}
	for _, span := range spans {
		if !refLE(from, span.from) || !refLE(span.to, to) || !refLE(span.to, day.ReadThrough) {
			break
		}
		day.Progress = span.to.String()
	}
	return nil
}

// 固定错误不附带调用参数，迟到的工作 Session 不能再写入或读取原文。
var (
	ErrInactiveSession = errors.New("这个 Session 已不是当天的工作 Session")
	ErrDayDeleted      = errors.New("这个业务日已删除")
	ErrCapturesExpired = errors.New("这个业务日的原文已到期，不能读取、补传或提交整理结果")
)

// sessionDay 包含历史绑定，避免停用后的 Session 退回未绑定的兼容路径。
func (u *Book) sessionDay(session string) (string, *captureDay) {
	if session != "" {
		for date, day := range u.captureDays {
			if day.Session == session || slices.Contains(day.Sessions, session) {
				return date, day
			}
		}
	}
	return "", nil
}

func (u *Book) checkWorkingSession(session string) error {
	if date, day := u.sessionDay(session); day != nil {
		if err := u.CheckCaptures(date); err == ErrCapturesExpired {
			return err
		}
		if day.Deleted || day.Session != session {
			return ErrInactiveSession
		}
	}
	return nil
}

func (u *Book) sessionDays() map[string]string {
	out := map[string]string{}
	for date, day := range u.captureDays {
		for _, session := range day.Sessions {
			out[session] = date
		}
		if day.Session != "" {
			out[day.Session] = date
		}
	}
	return out
}

// BusinessDay 只读出编排绑定所需的元数据，不改变已读位置与处理进度。
type BusinessDay struct {
	Session         string    `json:"session"`
	DeleteAt        time.Time `json:"delete_at,omitzero"`
	Deleted         bool      `json:"deleted"`
	TimeZone        string    `json:"time_zone,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitzero"`
	Expired         bool      `json:"expired"`
	CaptureRevision uint64    `json:"capture_revision"`
}

func (u *Book) BusinessDay(date string) BusinessDay {
	var out BusinessDay
	if d := u.captureDays[date]; d != nil {
		out.Session, out.DeleteAt, out.Deleted = d.Session, d.DeleteAt, d.Deleted
		out.TimeZone = d.TimeZone
		out.ExpiresAt, out.Expired, out.CaptureRevision = d.ExpiresAt, d.Expired, d.CaptureRevision
	}
	return out
}

// DayCaptures 供本人开发页面读取；返回副本，不登记模型读页或固定时段间隔。
func (u *Book) DayCaptures(date string) ([]PageSegment, error) {
	out := []PageSegment{}
	for s, err := range u.captureSegments(date, Ref{}, Ref{}) {
		if err != nil {
			return nil, err
		}
		g := s.g
		out = append(out, PageSegment{Ref: s.ref.String(), Speaker: g.Speaker, SpeakerKey: g.SpeakerKey, SpeakerRef: g.SpeakerRef, Text: g.Text, SpokenAt: g.SpokenAt, TimeZone: g.TimeZone})
	}
	return out, nil
}

func validateCaptureBatch(captureID string, segments []CaptureSegment) error {
	if captureID == "" || len(captureID) > 128 {
		return fmt.Errorf("capture_id must contain 1 to 128 bytes")
	}
	if len(segments) == 0 || len(segments) > 200 {
		return fmt.Errorf("captures must contain 1 to 200 segments")
	}
	for _, g := range segments {
		if g.Speaker != "wearer" && g.Speaker != "other" {
			return fmt.Errorf("invalid capture speaker")
		}
		if g.Speaker == "wearer" && g.SpeakerKey != "" || g.Speaker == "other" && !captureSpeakerKey.MatchString(g.SpeakerKey) {
			return fmt.Errorf("invalid capture speaker_key")
		}
		if g.SpeakerRef != "" && (g.Speaker != "other" || !captureSpeakerRef.MatchString(g.SpeakerRef)) {
			return fmt.Errorf("invalid capture speaker_ref")
		}
		if g.Text == "" || utf8.RuneCountInString(g.Text) > 2000 {
			return fmt.Errorf("capture text must contain 1 to 2000 characters")
		}
		if g.SpokenAt != "" {
			at, err := time.Parse(time.RFC3339Nano, g.SpokenAt)
			if err != nil || at.UTC().Year() < 1 || at.UTC().Year() > 9999 {
				return fmt.Errorf("invalid capture spoken_at")
			}
		}
		if g.TimeZone != "" {
			if _, ok := captureZone(g.TimeZone); !ok {
				return fmt.Errorf("invalid capture time_zone")
			}
		}
	}
	return nil
}
