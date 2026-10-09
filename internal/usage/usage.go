// Package usage 计量模型调用与沙箱执行的用量，并施加业务线与 EndUser 两层配额
// （docs/design/m4-quota-usage.md，ADR-0019）。
//
// 用量独立于 Event 日志保存：Session 会被物理删除，而结算需要在删除之后仍可核对。
// 因此记录中不含 Session、Run 与任何内容，只有业务线、EndUser 与数量。
package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Kind string

const (
	Model   Kind = "model"
	Sandbox Kind = "sandbox"
)

// Anonymous 是注销后的 EndUser：其用量仍计入业务线合计，但不再留存个人标识。
const Anonymous = "~"

// Entry 是一条用量记录。金额单位是微元（10⁻⁶ 元）。
type Entry struct {
	ID           string
	BusinessLine string
	EndUser      string
	Kind         Kind
	// Model 是模型引用（provider/model）；沙箱执行为 "sandbox"。
	Model string
	// Agent 是发起调用的 AgentDef 版本（name@version），用于比较灰度版本的成本；沙箱执行为空。
	Agent         string
	InputTokens   uint64
	OutputTokens  uint64
	SandboxMillis uint64
	Cost          int64
	At            time.Time
}

func (e *Entry) Validate() error {
	if e.ID == "" || e.BusinessLine == "" || e.EndUser == "" || e.EndUser == Anonymous || e.Kind == "" || e.Cost < 0 {
		return fmt.Errorf("usage: invalid entry %q", e.ID)
	}
	return nil
}

type GroupBy string

const (
	ByDay     GroupBy = "day"
	ByEndUser GroupBy = "end_user"
	ByModel   GroupBy = "model"
	ByAgent   GroupBy = "agent"
)

// Query 汇总 [From, To) 内的用量；EndUser 为空表示整个业务线。按日汇总时日期取 Location 中的日期，
// Location 须是 IANA 时区（存储按名称换算）。
type Query struct {
	BusinessLine string
	EndUser      string
	From, To     time.Time
	GroupBy      GroupBy
	Location     *time.Location
}

type Row struct {
	Key           string `json:"key"`
	Calls         int64  `json:"calls"`
	Cost          int64  `json:"cost_micros"`
	InputTokens   uint64 `json:"input_tokens"`
	OutputTokens  uint64 `json:"output_tokens"`
	SandboxMillis uint64 `json:"sandbox_millis"`
}

// Store 保存用量。实现须通过 usagetest 一致性套件。
type Store interface {
	// Record 写入一条用量；ID 已存在时不变（幂等）。
	Record(ctx context.Context, e *Entry) error
	// Spent 返回 [from, to) 内的金额；from、to 须与整点对齐。endUser 为空表示整个业务线。
	Spent(ctx context.Context, businessLine, endUser string, from, to time.Time) (int64, error)
	// Report 按 Query 汇总，结果按 Key 排序。
	Report(ctx context.Context, q Query) ([]Row, error)
	// DeleteEndUser 把 EndUser 的用量匿名化为 Anonymous；业务线合计不变。
	DeleteEndUser(ctx context.Context, businessLine, endUser string) error
	// AnonymizeEntry 把一条用量匿名化；不存在或已匿名时无操作。
	AnonymizeEntry(ctx context.Context, id string) error
	// Prune 删除 before（向下取整到整点）之前的用量。
	Prune(ctx context.Context, before time.Time) error
}

// Price 是模型单价：元 / 百万 token，数值上等于微元 / token。
type Price struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

// PriceList 是部署配置中的价格表（pricing.yaml）。
type PriceList struct {
	Currency string           `yaml:"currency"`
	Models   map[string]Price `yaml:"models"`
	Sandbox  struct {
		// PerHour 是沙箱每小时执行时间的价格（元）。
		PerHour float64 `yaml:"per_hour"`
	} `yaml:"sandbox"`
}

func LoadPriceList(path string) (*PriceList, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := &PriceList{}
	if err := yaml.Unmarshal(b, p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for m, pr := range p.Models {
		if pr.Input < 0 || pr.Output < 0 {
			return nil, fmt.Errorf("%s: negative price for %s", path, m)
		}
	}
	if p.Sandbox.PerHour < 0 {
		return nil, fmt.Errorf("%s: negative sandbox price", path)
	}
	return p, nil
}

// Has 报告模型是否有价格；nil 价格表没有任何价格。
func (p *PriceList) Has(model string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Models[model]
	return ok
}

// ModelCost 返回一次模型调用的金额（微元）；没有价格时为 0。
func (p *PriceList) ModelCost(model string, input, output uint64) int64 {
	if p == nil {
		return 0
	}
	pr := p.Models[model]
	return int64(math.Round(float64(input)*pr.Input + float64(output)*pr.Output))
}

// SandboxCost 返回沙箱执行 d 的金额（微元）。
func (p *PriceList) SandboxCost(d time.Duration) int64 {
	if p == nil || d <= 0 {
		return 0
	}
	return int64(math.Round(p.Sandbox.PerHour * 1e6 * d.Hours()))
}

// Limits 是业务线配置中的配额（元）；零值表示该层不限制。
type Limits struct {
	Monthly      float64 `yaml:"monthly"`
	EndUserDaily float64 `yaml:"end_user_daily"`
	// Timezone 决定日、月的边界，默认 Asia/Shanghai；偏移须为整小时（聚合按小时进行）。
	Timezone string `yaml:"timezone"`

	loc *time.Location
}

func (l Limits) Enabled() bool { return l.Monthly > 0 || l.EndUserDaily > 0 }

// Prepare 校验并加载时区；业务线配置加载时调用。
func (l *Limits) Prepare() error {
	if l.Monthly < 0 || l.EndUserDaily < 0 {
		return errors.New("quota: limits must not be negative")
	}
	tz := l.Timezone
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("quota: %w", err)
	}
	// 检查未来两年每个月初的偏移（覆盖夏令时切换），保证日、月边界与整点对齐。
	now := time.Now().In(loc)
	for i := range 24 {
		t := time.Date(now.Year(), now.Month()+time.Month(i), 1, 0, 0, 0, 0, loc)
		if _, off := t.Zone(); off%3600 != 0 {
			return fmt.Errorf("quota: timezone %s is not offset by whole hours", tz)
		}
	}
	l.loc = loc
	return nil
}

// Location 返回配额周期所在的时区。
func (l Limits) Location() *time.Location {
	if l.loc == nil {
		loc, _ := time.LoadLocation("Asia/Shanghai")
		return loc
	}
	return l.loc
}

// Yuan 把元换算为微元。
func Yuan(v float64) int64 { return int64(math.Round(v * 1e6)) }

// Period 是一层配额的当前周期。
type Period struct {
	Scope        string    `json:"scope"` // "end_user" | "business_line"
	Limit        int64     `json:"limit_micros"`
	Spent        int64     `json:"spent_micros"`
	Start        time.Time `json:"start"`
	ResetAt      time.Time `json:"resets_at"`
	Exceeded     bool      `json:"exceeded"`
	BusinessLine string    `json:"business_line"`
	EndUser      string    `json:"end_user,omitempty"`
}

// Reason 是因该层配额挂起时 RunSuspended.reason 的取值。
func (p *Period) Reason() string { return p.Scope + "_quota" }

func dayOf(t time.Time, loc *time.Location) (time.Time, time.Time) {
	t = t.In(loc)
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

func monthOf(t time.Time, loc *time.Location) (time.Time, time.Time) {
	t = t.In(loc)
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 1, 0)
}

// Quotas 按业务线配置检查配额。检查与扣减不原子：这是软上限（ADR-0019）。
type Quotas struct {
	Store  Store
	Limits map[string]Limits
}

// Enabled 报告业务线是否配置了配额；nil 的 Quotas 不限制任何业务线。
func (q *Quotas) Enabled(businessLine string) bool {
	return q != nil && q.Limits[businessLine].Enabled()
}

// Location 返回业务线配额周期与用量报表所用的时区；未配置时为 Asia/Shanghai。
func (q *Quotas) Location(businessLine string) *time.Location {
	if q == nil {
		return Limits{}.Location()
	}
	return q.Limits[businessLine].Location()
}

// Status 返回当前周期的各层配额（业务线、EndUser；endUser 为空时只有业务线）。
func (q *Quotas) Status(ctx context.Context, businessLine, endUser string, now time.Time) ([]*Period, error) {
	if !q.Enabled(businessLine) {
		return nil, nil
	}
	l := q.Limits[businessLine]
	var out []*Period
	if l.Monthly > 0 {
		from, to := monthOf(now, l.Location())
		p := &Period{Scope: "business_line", Limit: Yuan(l.Monthly), Start: from, ResetAt: to, BusinessLine: businessLine}
		out = append(out, p)
	}
	if l.EndUserDaily > 0 && endUser != "" {
		from, to := dayOf(now, l.Location())
		p := &Period{Scope: "end_user", Limit: Yuan(l.EndUserDaily), Start: from, ResetAt: to, BusinessLine: businessLine, EndUser: endUser}
		out = append(out, p)
	}
	for _, p := range out {
		var err error
		if p.Spent, err = q.Store.Spent(ctx, businessLine, p.EndUser, p.Start, p.ResetAt); err != nil {
			return nil, err
		}
		p.Exceeded = p.Spent >= p.Limit
	}
	return out, nil
}

// Check 返回已超出的一层配额，没有时返回 nil。两层都超出时返回较晚重置的一层：
// 在它重置之前，另一层重置也无法恢复。
func (q *Quotas) Check(ctx context.Context, businessLine, endUser string, now time.Time) (*Period, error) {
	ps, err := q.Status(ctx, businessLine, endUser, now)
	if err != nil {
		return nil, err
	}
	var worst *Period
	for _, p := range ps {
		if p.Exceeded && (worst == nil || p.ResetAt.After(worst.ResetAt)) {
			worst = p
		}
	}
	return worst, nil
}

// ErrExceeded 表示配额已用尽（HTTP 429）。
var ErrExceeded = errors.New("quota exceeded")

// ExceededError 携带超出的一层配额。
type ExceededError struct{ Period *Period }

func (e *ExceededError) Error() string {
	return fmt.Sprintf("%s quota exceeded until %s", e.Period.Scope, e.Period.ResetAt.UTC().Format(time.RFC3339))
}

func (e *ExceededError) Unwrap() error { return ErrExceeded }
