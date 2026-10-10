// Package memory 实现 EndUser 的 Memory 与跨业务线的 Grant（docs/design/m4-memory-grant.md，ADR-0003、ADR-0016）。
//
// Memory 归属写入它的 BusinessLine；其他业务线只有在 EndUser 授予 Grant 后，才能读取指定类别的 Memory。
// 检索时有嵌入向量就按余弦相似度排序，否则按确定性的文本相似度排序（每个 EndUser 每个业务线至多
// MaxPerUser 条，在应用层计算即可）。
package memory

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

var (
	ErrNotFound = errors.New("memory: not found")
	// ErrLimit 表示 EndUser 在该业务线的 Memory 已达上限。
	ErrLimit = errors.New("memory: limit reached")
	// ErrRejected 表示内容不允许存储（敏感个人信息、类别无效等）。
	ErrRejected = errors.New("memory: rejected")
)

// Category 是平台固定的 Memory 类别：Grant 按类别授权，EndUser 也按类别理解自己的 Memory。
type Category string

const (
	Profile      Category = "profile"
	Preference   Category = "preference"
	Relationship Category = "relationship"
	Interest     Category = "interest"
	Plan         Category = "plan"
	// Health 是健康信息（敏感个人信息）：只有声明已取得用户单独同意的业务线可以写入与召回（ADR-0022）。
	Health Category = "health"
)

// Categories 是全部类别及其说明（用于工具描述与界面）。
var Categories = []struct {
	Category    Category
	Description string
}{
	{Profile, "基本信息：称呼、职业、常驻城市"},
	{Preference, "偏好：回答风格、饮食与生活偏好"},
	{Relationship, "人际关系：家人、同事及其联系方式"},
	{Interest, "兴趣：关注的话题与爱好"},
	{Plan, "长期事项：计划、正在进行的事"},
	{Health, "健康：过敏、饮食禁忌、慢性病与用药等与安全相关的信息"},
}

func ValidCategory(c Category) bool {
	for _, x := range Categories {
		if x.Category == c {
			return true
		}
	}
	return false
}

type Memory struct {
	ID           string
	BusinessLine string
	EndUser      string
	Category     Category
	Content      string
	// SourceSession 与 SourceCall 记录由哪个 Session 的哪次调用写入；删除该 Session 时一并删除（§7）。
	SourceSession string
	SourceCall    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Vector 是嵌入向量及产生它的模型；只有同一模型的向量才能相互比较。
type Vector struct {
	Model  string
	Values []float32
}

// Scope 是可读取的范围：某业务线中的若干类别（为空表示全部类别）。
type Scope struct {
	BusinessLine string
	Categories   []Category
}

func (s Scope) allows(m *Memory) bool {
	if m.BusinessLine != s.BusinessLine {
		return false
	}
	if len(s.Categories) == 0 {
		return true
	}
	for _, c := range s.Categories {
		if c == m.Category {
			return true
		}
	}
	return false
}

// Hit 是一条检索结果。
type Hit struct {
	*Memory
	Score float64
}

type Store interface {
	// Put 按 ID 写入或覆盖；vec 可为 nil。
	Put(ctx context.Context, m *Memory, vec *Vector) error
	Get(ctx context.Context, id string) (*Memory, error)
	// Delete 删除一条；不存在时不报错。
	Delete(ctx context.Context, id string) error
	// Count 返回 EndUser 在业务线中的 Memory 数。
	Count(ctx context.Context, businessLine, endUser string) (int, error)
	// List 返回 EndUser 在 scopes 内的 Memory，按更新时间倒序（同时间按 ID），至多 limit 条。
	List(ctx context.Context, endUser string, scopes []Scope, limit int) ([]*Memory, error)
	// Nearest 返回 scopes 内与 vec 同模型的 Memory，按余弦相似度降序（同分按更新时间倒序、ID），至多 limit 条。
	Nearest(ctx context.Context, endUser string, scopes []Scope, vec Vector, limit int) ([]Hit, error)
	// DeleteSession 删除由该 Session 写入的全部 Memory。
	DeleteSession(ctx context.Context, sessionID string) error
	// DeleteEndUser 删除 EndUser 在业务线中的全部 Memory。
	DeleteEndUser(ctx context.Context, businessLine, endUser string) error

	// RecordAccess 记录访问；已不存在的 Memory 跳过。删除 Memory 时其访问记录一并删除。
	RecordAccess(ctx context.Context, accesses []Access) error
	// Accesses 返回 EndUser 在业务线 owner 中的 Memory 自 since 起被读取的记录，按时间倒序，至多 limit 条。
	Accesses(ctx context.Context, endUser, owner string, since time.Time, limit int) ([]Access, error)
}

// Access 是一次读取记录（不含内容）：哪条 Memory 在什么时候被哪个业务线的哪个 Session 召回。
type Access struct {
	MemoryID  string
	Owner     string // Memory 所属业务线
	EndUser   string
	Reader    string // 读取方业务线
	SessionID string
	RunID     string
	At        time.Time
}

// Grant 是 EndUser 允许业务线 To 读取自己在业务线 From 中若干类别 Memory 的授权。
type Grant struct {
	ID         string
	EndUser    string
	From, To   string
	Categories []Category
	CreatedAt  time.Time
	// ExpiresAt 与 RevokedAt 为零值表示未设置。
	ExpiresAt time.Time
	RevokedAt time.Time
}

// ActiveAt 报告授权在 t 时是否有效。
func (g *Grant) ActiveAt(t time.Time) bool {
	return g.RevokedAt.IsZero() && (g.ExpiresAt.IsZero() || t.Before(g.ExpiresAt))
}

type Grants interface {
	Create(ctx context.Context, g *Grant) error
	Get(ctx context.Context, id string) (*Grant, error)
	// Revoke 记录撤销时间；已撤销时不变。
	Revoke(ctx context.Context, id string, at time.Time) error
	// List 返回 EndUser 涉及业务线 businessLine（作为 From 或 To）的全部授权（含已撤销），按创建时间倒序。
	List(ctx context.Context, endUser, businessLine string) ([]*Grant, error)
	// ToReader 返回 EndUser 授予业务线 to 的全部授权（含已撤销与过期，由调用方按时间判断）。
	ToReader(ctx context.Context, endUser, to string) ([]*Grant, error)
	// DeleteBusinessLine 删除 EndUser 涉及该业务线（作为 From 或 To）的全部授权（注销账号）。
	DeleteBusinessLine(ctx context.Context, endUser, businessLine string) error
}

// ngrams 把文本规范为字符二元组集合：中文按字、英文与数字按词内字符，标点与空白作为分隔。
// 二元组对中文短句的相似度判断足够稳定，而且结果确定，适合作为没有嵌入服务时的检索方式。
func ngrams(s string) map[string]bool {
	out := map[string]bool{}
	var run []rune
	flush := func() {
		if len(run) == 1 {
			out[string(run)] = true
		}
		for i := 0; i+1 < len(run); i++ {
			out[string(run[i:i+2])] = true
		}
		run = run[:0]
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			run = append(run, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// TextSimilarity 是两段文本字符二元组的 Dice 系数，范围 [0, 1]。
func TextSimilarity(a, b string) float64 {
	x, y := ngrams(a), ngrams(b)
	if len(x) == 0 || len(y) == 0 {
		return 0
	}
	common := 0
	for g := range x {
		if y[g] {
			common++
		}
	}
	return 2 * float64(common) / float64(len(x)+len(y))
}

// Cosine 是两个向量的余弦相似度；维度不同时为 0。
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// rank 按得分降序、更新时间倒序、ID 排序，取前 limit 条。
func rank(hits []Hit, limit int) []Hit {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if !hits[i].UpdatedAt.Equal(hits[j].UpdatedAt) {
			return hits[i].UpdatedAt.After(hits[j].UpdatedAt)
		}
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}
