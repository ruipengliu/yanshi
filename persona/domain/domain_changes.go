package domain

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

const (
	FactDay  = "day"
	FactWeek = "week"
	// DomainChangeRetention 从确认时算起；未确认的变化不能按年龄丢弃。
	DomainChangeRetention = 7 * 24 * time.Hour
)

// Fact 是接受事实的身份，ID 在 owner 内唯一；跨 owner 的消费者须连同 owner 去重。
// Date 是业务日或最近七天的起点；版本与接受时钟无关，擦除后仍不复用。
// 正文仍在 Day/Week，独立元数据避免改变既有 State 与工具回执的契约。
type Fact struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Date       string    `json:"date"`
	Version    uint64    `json:"version"`
	AcceptedAt time.Time `json:"accepted_at"`
}

// DomainChange 只表达接受事实，不携带正文、指纹或 Agent 调用策略。
type DomainChange struct {
	Fact
	AcknowledgedAt time.Time `json:"acknowledged_at,omitzero"`
}

// acceptedFact 的去重信息不对消费者开放；擦除时清掉，版本计数保留到 owner 注销。
type acceptedFact struct {
	Fact
	Session     string
	Fingerprint string
	Active      bool
}

func factID(kind, date string) string { return kind + "/" + date }

// 旧路径没有业务日/周绑定，展示日期不能充当身份；编码 Session 防止分隔符与日期范围混淆。
func sessionFactID(kind, session string) string {
	return kind + "/session/" + base64.RawURLEncoding.EncodeToString([]byte(session))
}

// AcceptedFact 返回当前有效事实；擦除后的版本计数仅用于防止重建复用版本。
func (u *Book) AcceptedFact(kind, date string) (Fact, bool) {
	return u.AcceptedFactByID(factID(kind, date))
}

// AcceptedFactByID 按领域变化的完整事实 ID 读取，也适用于未绑定 Session 的旧路径。
func (u *Book) AcceptedFactByID(id string) (Fact, bool) {
	f := u.acceptedFacts[id]
	return f.Fact, f.Active
}

// acceptFact 与正文写入同在 Book 事务内。入口尚无稳定命令 ID，按 Session 与规范化输入识别重试；
// 只与当前接受比较，A→B→A 是合法重交，不能按历史内容哈希永久去重。
func (c *Call) acceptFact(kind, id, date string, input any) bool {
	data, err := json.Marshal(input)
	if err != nil {
		c.err = err
		return false
	}
	sum := sha256.Sum256(data)
	fingerprint := hex.EncodeToString(sum[:])
	old := c.u.acceptedFacts[id]
	if old.Active && old.Session == c.session && old.Fingerprint == fingerprint {
		return false
	}
	if old.Version == ^uint64(0) {
		c.err = fmt.Errorf("accepted fact version exhausted")
		return false
	}
	fact := Fact{ID: id, Kind: kind, Date: date, Version: old.Version + 1, AcceptedAt: c.now}
	if c.u.acceptedFacts == nil {
		c.u.acceptedFacts = map[string]acceptedFact{}
	}
	c.u.acceptedFacts[id] = acceptedFact{Fact: fact, Session: c.session, Fingerprint: fingerprint, Active: true}
	c.u.domainChanges = append(c.u.domainChanges, DomainChange{Fact: fact})
	c.u.PruneDomainChanges(c.now)
	return true
}

func (u *Book) invalidateDayFact(date string) {
	id := factID(FactDay, date)
	if f, ok := u.acceptedFacts[id]; ok {
		f.Active, f.Session, f.Fingerprint = false, "", ""
		u.acceptedFacts[id] = f
	}
}

// PendingDomainChanges 按接受顺序读取，读取本身不推进确认位置。
func (u *Book) PendingDomainChanges() []DomainChange {
	out := []DomainChange{}
	for _, change := range u.domainChanges {
		if change.AcknowledgedAt.IsZero() {
			out = append(out, change)
		}
	}
	return out
}

// AckDomainChanges 只确认指定事实截至 through 的版本；不能用未来版本吞掉尚未接受的事实。
// 应用须先持久接收再确认，重复确认不刷新保留期限。
func (u *Book) AckDomainChanges(id string, through uint64, now time.Time) error {
	f, ok := u.acceptedFacts[id]
	if !ok || through == 0 || through > f.Version || now.IsZero() {
		return fmt.Errorf("invalid domain change acknowledgement")
	}
	for i := range u.domainChanges {
		change := &u.domainChanges[i]
		if change.ID == id && change.Version <= through && change.AcknowledgedAt.IsZero() {
			change.AcknowledgedAt = now
		}
	}
	u.PruneDomainChanges(now)
	return nil
}

// PruneDomainChanges 可由维护者在 owner Update 中调用；接受和确认时也顺带清理。
// 闲置 owner 可延后物理清理，未确认变化与版本计数始终保留。
func (u *Book) PruneDomainChanges(now time.Time) {
	cutoff := now.Add(-DomainChangeRetention)
	u.domainChanges = slices.DeleteFunc(u.domainChanges, func(change DomainChange) bool {
		return !change.AcknowledgedAt.IsZero() && !change.AcknowledgedAt.After(cutoff)
	})
}

// 未绑定的开发/评测 Session 沿用自身首次接受的周，不能借其他 Session 的同日事实推导。
func (c *Call) weekStart() (string, bool) {
	for _, from := range sortedKeys(c.u.weekSessions) {
		if c.u.weekSessions[from] == c.session {
			return from, true
		}
	}
	if f := c.u.acceptedFacts[sessionFactID(FactWeek, c.session)]; f.Active {
		return f.Date, false
	}
	return c.now.AddDate(0, 0, -weekDays+1).Format(time.DateOnly), false
}
