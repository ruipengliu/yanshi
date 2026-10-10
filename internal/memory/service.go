package memory

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
)

// Embedder 计算文本的嵌入向量（由模型网关提供）。
type Embedder interface {
	Embed(ctx context.Context, model string, texts []string) ([][]float32, error)
}

// DefaultMaxPerUser 是每个 EndUser 在每个业务线的 Memory 上限（§5）。
const DefaultMaxPerUser = 500

// MaxContentRunes 是单条 Memory 的长度上限：Memory 应是一句陈述，而不是文档。
const MaxContentRunes = 300

type Service struct {
	Store  Store
	Grants Grants
	// Embedder 与 EmbedModel（"provider/model"）都配置时按向量检索，否则按文本相似度检索。
	Embedder   Embedder
	EmbedModel string
	// Deletions 非 nil 时，写入后复查所属 Session 是否已删除（ADR-0015）。
	Deletions  lifecycle.Deletions
	Clock      clock.Clock
	IDs        ids.Generator
	MaxPerUser int
	// HealthAllowed 报告业务线是否已取得用户对健康信息的单独同意（业务线配置 memory.health）；
	// nil 表示所有业务线都未开启。未开启的业务线不能写入 health 类别，也不召回、不检索该类别（ADR-0022）。
	HealthAllowed func(businessLine string) bool
}

func (s *Service) healthAllowed(businessLine string) bool {
	return s.HealthAllowed != nil && s.HealthAllowed(businessLine)
}

// withoutHealth 返回除 health 外的全部类别。
func withoutHealth(cats []Category) []Category {
	if len(cats) == 0 {
		for _, c := range Categories {
			cats = append(cats, c.Category)
		}
	}
	return slices.DeleteFunc(slices.Clone(cats), func(c Category) bool { return c == Health })
}

func (s *Service) max() int {
	if s.MaxPerUser > 0 {
		return s.MaxPerUser
	}
	return DefaultMaxPerUser
}

func (s *Service) embedding() bool { return s.Embedder != nil && s.EmbedModel != "" }

var (
	idCardRE   = regexp.MustCompile(`\d{17}[\dXx]`)
	digitRunRE = regexp.MustCompile(`\d[\d -]{14,24}\d`)
)

// sensitive 拒绝明显的证件号与银行卡号。其余敏感个人信息（金融、精确位置等）由写入指令约束；健康信息按业务线开关处理（ADR-0022）。
// 这些也留待内容安全能力进一步识别。
func sensitive(content string) bool {
	if idCardRE.MatchString(content) {
		return true
	}
	for _, run := range digitRunRE.FindAllString(content, -1) {
		digits := strings.NewReplacer(" ", "", "-", "").Replace(run)
		if len(digits) >= 16 && len(digits) <= 19 && luhn(digits) {
			return true
		}
	}
	return false
}

func luhn(digits string) bool {
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

type SaveRequest struct {
	BusinessLine, EndUser string
	SessionID, CallID     string
	Category              Category
	Content               string
	// Replaces 非空时，写入后删除这条旧 Memory（须属于同一 EndUser 与业务线）。
	Replaces string
	// At 非零时作为记录时间（导入已有记忆、评测预置不同时间的记忆）；否则取当前时间。
	At time.Time
}

// Save 写入一条 Memory。ID 由调用 ID 派生，因此同一调用重复执行是幂等的。
func (s *Service) Save(ctx context.Context, r SaveRequest) (*Memory, error) {
	content := strings.TrimSpace(r.Content)
	switch {
	case !ValidCategory(r.Category):
		return nil, fmt.Errorf("%w: unknown category %q", ErrRejected, r.Category)
	case content == "":
		return nil, fmt.Errorf("%w: content is empty", ErrRejected)
	case utf8.RuneCountInString(content) > MaxContentRunes:
		return nil, fmt.Errorf("%w: content longer than %d characters; store one short statement", ErrRejected, MaxContentRunes)
	case sensitive(content):
		return nil, fmt.Errorf("%w: sensitive personal information (ID or bank card numbers) must not be stored", ErrRejected)
	case r.Category == Health && !s.healthAllowed(r.BusinessLine):
		return nil, fmt.Errorf("%w: health information is not enabled for this business line (it requires the user's separate consent); tell the user it cannot be remembered", ErrRejected)
	}
	id := "mem_" + r.CallID
	if r.CallID == "" {
		id = "mem_" + s.IDs()
	}
	if m, err := s.Store.Get(ctx, id); err == nil {
		return m, nil // 同一调用的重复执行
	}
	var old *Memory
	if r.Replaces != "" {
		m, err := s.Store.Get(ctx, r.Replaces)
		if err != nil || m.BusinessLine != r.BusinessLine || m.EndUser != r.EndUser {
			return nil, fmt.Errorf("%w: memory %s", ErrNotFound, r.Replaces)
		}
		// 只能替换同类别的记忆：替换会删除旧条目，类别不同多半是 ID 用错，会悄悄删掉一条无关的信息。
		if m.Category != r.Category {
			return nil, fmt.Errorf("%w: memory %s is %s, not %s; use memory_forget and memory_save to move it", ErrRejected, r.Replaces, m.Category, r.Category)
		}
		old = m
	}
	if old == nil {
		n, err := s.Store.Count(ctx, r.BusinessLine, r.EndUser)
		if err != nil {
			return nil, err
		}
		if n >= s.max() {
			return nil, fmt.Errorf("%w: %d memories stored; forget or replace outdated ones first", ErrLimit, n)
		}
	}
	now := s.Clock.Now()
	if !r.At.IsZero() {
		now = r.At
	}
	m := &Memory{ID: id, BusinessLine: r.BusinessLine, EndUser: r.EndUser, Category: r.Category, Content: content,
		SourceSession: r.SessionID, SourceCall: r.CallID, CreatedAt: now, UpdatedAt: now}
	var vec *Vector
	if s.embedding() {
		v, err := s.Embedder.Embed(ctx, s.EmbedModel, []string{content})
		if err != nil {
			return nil, fmt.Errorf("embed: %w", err)
		}
		vec = &Vector{Model: s.EmbedModel, Values: v[0]}
	}
	if err := s.Store.Put(ctx, m, vec); err != nil {
		return nil, err
	}
	// 先写、后查：所属 Session 已删除时撤回（ADR-0015）。
	if s.Deletions != nil && r.SessionID != "" {
		gone, err := lifecycle.Deleted(ctx, s.Deletions, r.SessionID)
		if err != nil || gone {
			_ = s.Store.Delete(context.WithoutCancel(ctx), id)
			if gone {
				return nil, fmt.Errorf("%w: session %s", ErrNotFound, r.SessionID)
			}
			return nil, err
		}
	}
	if old != nil {
		if err := s.Store.Delete(ctx, old.ID); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Forget 删除 EndUser 在业务线中的一条 Memory；只能删除本业务线的。
func (s *Service) Forget(ctx context.Context, businessLine, endUser, id string) error {
	m, err := s.Store.Get(ctx, id)
	if err != nil || m.BusinessLine != businessLine || m.EndUser != endUser {
		return fmt.Errorf("%w: memory %s", ErrNotFound, id)
	}
	return s.Store.Delete(ctx, id)
}

// Scopes 返回业务线 reader 在 t 时可读取的范围：本业务线的全部，加上 EndUser 有效授权的类别。
// 授权在每次检索时实时读取，撤销立即生效（ADR-0016）。
func (s *Service) Scopes(ctx context.Context, endUser, reader string, t time.Time) ([]Scope, error) {
	scopes := []Scope{{BusinessLine: reader}}
	if !s.healthAllowed(reader) {
		scopes[0].Categories = withoutHealth(nil)
	}
	if s.Grants == nil {
		return scopes, nil
	}
	grants, err := s.Grants.ToReader(ctx, endUser, reader)
	if err != nil {
		return nil, err
	}
	byFrom := map[string][]Category{}
	var order []string
	for _, g := range grants {
		if !g.ActiveAt(t) || g.From == reader {
			continue
		}
		if _, ok := byFrom[g.From]; !ok {
			order = append(order, g.From)
		}
		for _, c := range g.Categories {
			if !slices.Contains(byFrom[g.From], c) {
				byFrom[g.From] = append(byFrom[g.From], c)
			}
		}
	}
	slices.Sort(order)
	for _, from := range order {
		cats := byFrom[from]
		// 健康信息只在写入它的业务线已取得单独同意时才可被其他业务线读取（即使用户授权了该类别）。
		if !s.healthAllowed(from) {
			cats = withoutHealth(cats)
		}
		slices.Sort(cats)
		if len(cats) > 0 {
			scopes = append(scopes, Scope{BusinessLine: from, Categories: cats})
		}
	}
	return scopes, nil
}

// Search 检索业务线 reader 可读取的、与 text 最相关的 Memory。text 为空时按最近更新排序。
func (s *Service) Search(ctx context.Context, endUser, reader, text string, limit int) ([]Hit, error) {
	scopes, err := s.Scopes(ctx, endUser, reader, s.Clock.Now())
	if err != nil {
		return nil, err
	}
	if s.embedding() && strings.TrimSpace(text) != "" {
		v, err := s.Embedder.Embed(ctx, s.EmbedModel, []string{text})
		if err != nil {
			return nil, fmt.Errorf("embed: %w", err)
		}
		return s.Store.Nearest(ctx, endUser, scopes, Vector{Model: s.EmbedModel, Values: v[0]}, limit)
	}
	all, err := s.Store.List(ctx, endUser, scopes, s.max()*len(scopes))
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, len(all))
	for i, m := range all {
		hits[i] = Hit{Memory: m, Score: TextSimilarity(text, m.Content)}
	}
	return rank(hits, limit), nil
}

// ForgetSession 删除由某个 Session 写入的全部 Memory：发现记忆投毒后按来源撤销（ADR-0023）。
// 调用方须已确认该 Session 属于请求方的业务线。
func (s *Service) ForgetSession(ctx context.Context, sessionID string) error {
	return s.Store.DeleteSession(ctx, sessionID)
}

// CreateGrant 记录 EndUser 允许业务线 to 读取自己在业务线 from 中 categories 类别的 Memory。
func (s *Service) CreateGrant(ctx context.Context, endUser, from, to string, categories []Category, expires time.Time) (*Grant, error) {
	if from == "" || to == "" || from == to {
		return nil, fmt.Errorf("%w: a grant needs two different business lines", ErrRejected)
	}
	if len(categories) == 0 {
		return nil, fmt.Errorf("%w: at least one category is required", ErrRejected)
	}
	for _, c := range categories {
		if !ValidCategory(c) {
			return nil, fmt.Errorf("%w: unknown category %q", ErrRejected, c)
		}
	}
	g := &Grant{ID: "grant_" + s.IDs(), EndUser: endUser, From: from, To: to, Categories: slices.Clone(categories),
		CreatedAt: s.Clock.Now(), ExpiresAt: expires}
	return g, s.Grants.Create(ctx, g)
}

// DeleteEndUser 删除 EndUser 在业务线中的全部 Memory，以及涉及该业务线的全部授权（注销账号，§7）。
func (s *Service) DeleteEndUser(ctx context.Context, businessLine, endUser string) error {
	if err := s.Store.DeleteEndUser(ctx, businessLine, endUser); err != nil {
		return err
	}
	if s.Grants == nil {
		return nil
	}
	return s.Grants.DeleteBusinessLine(ctx, endUser, businessLine)
}

// List 返回 EndUser 在本业务线的 Memory（不含经授权可读的）：category 为空表示全部类别，
// q 非空时按与 q 的相关度排序，否则按最近更新排序。供 EndUser 管理自己的 Memory。
func (s *Service) List(ctx context.Context, businessLine, endUser string, category Category, q string, limit int) ([]Hit, error) {
	scope := Scope{BusinessLine: businessLine}
	if category != "" {
		if !ValidCategory(category) {
			return nil, fmt.Errorf("%w: unknown category %q", ErrRejected, category)
		}
		scope.Categories = []Category{category}
	}
	all, err := s.Store.List(ctx, endUser, []Scope{scope}, s.max())
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, len(all))
	for i, m := range all {
		hits[i] = Hit{Memory: m}
		if q != "" {
			hits[i].Score = TextSimilarity(q, m.Content)
		}
	}
	if q != "" {
		hits = rank(hits, len(hits))
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// RecordAccess 记录召回（不含内容）。
func (s *Service) RecordAccess(ctx context.Context, accesses []Access) error {
	if len(accesses) == 0 {
		return nil
	}
	return s.Store.RecordAccess(ctx, accesses)
}

// Accesses 返回 EndUser 在业务线 owner 中的 Memory 被读取的记录。
func (s *Service) Accesses(ctx context.Context, endUser, owner string, since time.Time, limit int) ([]Access, error) {
	return s.Store.Accesses(ctx, endUser, owner, since, limit)
}

// ListGrants 返回 EndUser 涉及业务线 businessLine 的全部授权（含已撤销）。
func (s *Service) ListGrants(ctx context.Context, endUser, businessLine string) ([]*Grant, error) {
	return s.Grants.List(ctx, endUser, businessLine)
}

// RevokeGrant 撤销 EndUser 的一条授权；调用方所在的业务线须是授权的一方。撤销立即生效（ADR-0016）。
func (s *Service) RevokeGrant(ctx context.Context, endUser, businessLine, id string) error {
	g, err := s.Grants.Get(ctx, id)
	if err != nil || g.EndUser != endUser || (g.From != businessLine && g.To != businessLine) {
		return fmt.Errorf("%w: grant %s", ErrNotFound, id)
	}
	return s.Grants.Revoke(ctx, id, s.Clock.Now())
}
