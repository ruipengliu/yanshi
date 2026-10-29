// Package domain 保存 Persona 的知识、变更集检查与回执文字；仅经事务接缝访问原文，不持锁，时间由调用方传入。
package domain

import (
	"encoding/json"
	"time"
)

// Citation 是线上引用的领域输入（docs/design/captures.md §6）。
// SpokenAt 由入口按 TimeZone 转为当地时间，领域换算期限时不再读取时区数据库。
type Citation struct {
	Ref        string    `json:"ref"`
	Speaker    string    `json:"speaker,omitempty"`
	SpeakerKey string    `json:"speaker_key,omitempty"`
	SpeakerRef string    `json:"speaker_ref,omitempty"`
	Redacted   bool      `json:"redacted,omitempty"`
	SpokenAt   time.Time `json:"spoken_at,omitzero"`
	TimeZone   string    `json:"time_zone,omitempty"`
	Missing    bool      `json:"missing,omitempty"`
}

// Call 在一个 Book 的事务内执行一次领域操作，所有输入都由适配层显式传入。
// 不复用 Call，以免上一次调用的通知泄漏到下一次。
type Call struct {
	err     error
	u       *Book
	session string
	args    json.RawMessage
	cites   map[string]Citation
	now     time.Time
	Filed   *DayFiled
}

// DayFiled 是已接受的当天整理所涉及的项目；只描述事实，不创建 Session 或提交输入。
type DayFiled struct {
	Session  string
	Date     string
	Projects []string
}

func NewCall(book *Book, session string, args json.RawMessage, cites map[string]Citation, now time.Time) *Call {
	if book != nil {
		book.SetCaptureClock(func() time.Time { return now })
	}
	return &Call{u: book, session: session, args: args, cites: cites, now: now}
}

// Err 与文字回执分开，存储故障不能变成可提交的领域拒收。适配层必须将它交回事务。
func (c *Call) Err() error {
	return c.err
}
