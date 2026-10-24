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
	// Location 是解读有效期与注明记录日期的时区，默认 Asia/Shanghai。
	Location *time.Location
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
	// Replaces 非空时，写入后把这条旧 Memory 标为被新的取代（须属于同一 EndUser、业务线与类别，且仍有效，§11）。
	Replaces string
	// ValidUntil 非空时是有效期的最后一天（当地日期 YYYY-MM-DD），过了这一天不再召回。
	ValidUntil string
	// At 非零时作为记录时间（导入已有记忆、评测预置不同时间的记忆）；否则取当前时间。
	At time.Time
}

const (
	// KeepSuperseded 与 KeepExpired 是失效、过期的 Memory 作为历史保留的时长（§11）。
	KeepSuperseded = 180 * 24 * time.Hour
	KeepExpired    = 30 * 24 * time.Hour
)

func (s *Service) location() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return shanghai
}

var shanghai = func() *time.Location {
	l, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return l
}()

// status 注明一条历史记忆为何不再有效："，10-22 被 mem_x 取代"或"，10-25 已过期"；有效的为空。
func (s *Service) status(m *Memory) string {
	switch {
	case m.SupersededBy != "":
		return "，" + s.Date(m.SupersededAt) + " 被 " + m.SupersededBy + " 取代"
	case !m.ValidUntil.IsZero() && !s.Clock.Now().Before(m.ValidUntil):
		return "，" + s.Date(m.ValidUntil.Add(-time.Second)) + " 后已过期"
	case !m.ValidUntil.IsZero():
		return "，有效至 " + s.Date(m.ValidUntil.Add(-time.Second))
	}
	return ""
}

// Date 是 t 在当地的日期（YYYY-MM-DD），召回与检索的结果据此注明记录日期。
func (s *Service) Date(t time.Time) string { return t.In(s.location()).Format(time.DateOnly) }

// Save 写入一条 Memory。ID 由调用 ID 派生，因此同一调用重复执行是幂等的。已有几乎相同的一条（有效的）时不再写入，返回
// 已有的那条（ID 与本次调用派生的不同）；有 Replaces 时仍把被替换的那条标为失效。
func (s *Service) Save(ctx context.Context, r SaveRequest) (*Memory, error) {
	content := strings.TrimSpace(r.Content)
	var until time.Time
	if r.ValidUntil != "" {
		d, err := time.ParseInLocation(time.DateOnly, r.ValidUntil, s.location())
		if err != nil {
			return nil, fmt.Errorf("%w: valid_until %q must be a date YYYY-MM-DD", ErrRejected, r.ValidUntil)
		}
		until = d.AddDate(0, 0, 1)
		if !s.Clock.Now().Before(until) {
			return nil, fmt.Errorf("%w: valid_until %s has already passed; an expired fact need not be remembered", ErrRejected, r.ValidUntil)
		}
	}
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
		// 只能替换同类别的记忆：类别不同多半是 ID 用错，会把一条无关的信息标为失效。
		if m.Category != r.Category {
			return nil, fmt.Errorf("%w: memory %s is %s, not %s; use memory_forget and memory_save to move it", ErrRejected, r.Replaces, m.Category, r.Category)
		}
		if !m.Valid(s.Clock.Now()) {
			return nil, fmt.Errorf("%w: memory %s is no longer valid", ErrNotFound, r.Replaces)
		}
		old = m
	}
	// 失效、过期太久的历史在同一 EndUser 写入时顺带清除（§11）：一条 SQL、一个键，不需要后台任务。
	now := s.Clock.Now()
	if err := s.Store.Purge(ctx, r.BusinessLine, r.EndUser, now.Add(-KeepSuperseded), now.Add(-KeepExpired)); err != nil {
		return nil, err
	}
	// 已有几乎相同的一条：不再写入。替换时只把被替换的那条标为失效（"把 X 换成已经记着的 C"就是让 X 失效）。
	if d, err := s.duplicate(ctx, r, content, old); err != nil || d != nil {
		if err == nil && old != nil {
			err = s.Store.Supersede(ctx, old.ID, d.ID, now)
		}
		if err != nil {
			return nil, err
		}
		return d, nil
	}
	if old == nil {
		n, err := s.Store.Count(ctx, r.BusinessLine, r.EndUser, now)
		if err != nil {
			return nil, err
		}
		if n >= s.max() {
			return nil, fmt.Errorf("%w: %d memories stored; forget or replace outdated ones first", ErrLimit, n)
		}
	}
	recorded := now
	if !r.At.IsZero() {
		recorded = r.At
	}
	m := &Memory{ID: id, BusinessLine: r.BusinessLine, EndUser: r.EndUser, Category: r.Category, Content: content,
		SourceSession: r.SessionID, SourceCall: r.CallID, CreatedAt: recorded, UpdatedAt: recorded, ValidUntil: until}
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
	gone, err := lifecycle.Recheck(ctx, s.Deletions, r.SessionID, func(ctx context.Context) error { return s.Store.Delete(ctx, id) })
	if err != nil && !gone {
		_ = s.Store.Delete(context.WithoutCancel(ctx), id) // 无法确认时同样撤回：写入失败，调用方重试
	}
	if gone {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, r.SessionID)
	}
	if err != nil {
		return nil, err
	}
	if old != nil {
		if err := s.Store.Supersede(ctx, old.ID, m.ID, now); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// DuplicateSimilarity 是视为同一条事实的文本相似度下限：只合并几乎相同的表述（标点、空白、个别字的差别）。
const DuplicateSimilarity = 0.85

var literalRE = regexp.MustCompile(`[A-Za-z0-9@._%+-]+`)

// literals 返回内容中的字母数字串（号码、邮箱、链接的片段），排序去重。
func literals(s string) []string {
	out := literalRE.FindAllString(strings.ToLower(s), -1)
	slices.Sort(out)
	return slices.Compact(out)
}

// duplicate 返回同一 EndUser 与业务线中与 content 几乎相同的已有 Memory（被替换的 old 除外）。每日整理会一再写下同一个
// 事实（用户每次提到都写一条），重复的条目既占满上限，也让召回被同一件事挤占；写入方得到已有的那条，不再新写。
// 不限于同一类别：同一句话归到另一个类别（"不吃香菜"写成 health）也是重复。号码、邮箱等字母数字串须完全相同：只差
// 一位号码的是更新，不是重复。
func (s *Service) duplicate(ctx context.Context, r SaveRequest, content string, old *Memory) (*Memory, error) {
	var cats []Category
	if !s.healthAllowed(r.BusinessLine) {
		cats = withoutHealth(nil)
	}
	all, err := s.Store.List(ctx, r.EndUser, []Scope{{BusinessLine: r.BusinessLine, Categories: cats}}, s.Clock.Now(), s.max())
	if err != nil {
		return nil, err
	}
	lit := literals(content)
	for _, m := range all {
		if old != nil && m.ID == old.ID {
			continue
		}
		if TextSimilarity(content, m.Content) >= DuplicateSimilarity && slices.Equal(literals(m.Content), lit) {
			return m, nil
		}
	}
	return nil, nil
}

// Related 返回与 m 同一 EndUser、业务线与类别的其他 Memory，按文本相似度排序，至多 limit 条。写入之后把它们交给写入方：
// 新的事实可能取代了其中一条（搬家、换工作），写入方没有先查时，这是它看到旧记忆的唯一机会。
func (s *Service) Related(ctx context.Context, m *Memory, limit int) ([]Hit, error) {
	all, err := s.Store.List(ctx, m.EndUser, []Scope{{BusinessLine: m.BusinessLine, Categories: []Category{m.Category}}}, s.Clock.Now(), s.max())
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(all))
	for _, x := range all {
		if x.ID != m.ID {
			hits = append(hits, Hit{Memory: x, Score: TextSimilarity(m.Content, x.Content)})
		}
	}
	return rank(hits, limit), nil
}

// Forget 删除 EndUser 在业务线中的一条 Memory，以及被它取代的整条链：用户说"忘掉"时，它的历史也一并忘掉（§11）。
// 只能删除本业务线的。
func (s *Service) Forget(ctx context.Context, businessLine, endUser, id string) error {
	m, err := s.Store.Get(ctx, id)
	if err != nil || m.BusinessLine != businessLine || m.EndUser != endUser {
		return fmt.Errorf("%w: memory %s", ErrNotFound, id)
	}
	all, err := s.Store.List(ctx, endUser, []Scope{{BusinessLine: businessLine, Categories: []Category{m.Category}}}, Everything, 1<<20)
	if err != nil {
		return err
	}
	chain := []string{id}
	for i := 0; i < len(chain); i++ {
		for _, x := range all {
			if x.SupersededBy == chain[i] && !slices.Contains(chain, x.ID) {
				chain = append(chain, x.ID)
			}
		}
	}
	for _, x := range chain {
		if err := s.Store.Delete(ctx, x); err != nil {
			return err
		}
	}
	return nil
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

// Search 检索业务线 reader 可读取的、有效的、与 text 最相关的 Memory。text 为空时按最近更新排序。
func (s *Service) Search(ctx context.Context, endUser, reader, text string, limit int) ([]Hit, error) {
	return s.search(ctx, endUser, reader, text, limit, false)
}

// SearchHistory 与 Search 相同，但同时包含本业务线失效与过期的 Memory（§11），供回答"以前""上次"一类的问题。
// 经 Grant 读取的只有有效的：历史不授出。
func (s *Service) SearchHistory(ctx context.Context, endUser, reader, text string, limit int) ([]Hit, error) {
	return s.search(ctx, endUser, reader, text, limit, true)
}

func (s *Service) search(ctx context.Context, endUser, reader, text string, limit int, history bool) ([]Hit, error) {
	now := s.Clock.Now()
	scopes, err := s.Scopes(ctx, endUser, reader, now)
	if err != nil {
		return nil, err
	}
	at := now
	if history {
		at = Everything
	}
	var hits []Hit
	if s.embedding() && strings.TrimSpace(text) != "" {
		v, err := s.Embedder.Embed(ctx, s.EmbedModel, []string{text})
		if err != nil {
			return nil, fmt.Errorf("embed: %w", err)
		}
		// 多取一些：历史中其他业务线的条目要滤掉。
		if hits, err = s.Store.Nearest(ctx, endUser, scopes, at, Vector{Model: s.EmbedModel, Values: v[0]}, 2*limit); err != nil {
			return nil, err
		}
	} else {
		all, err := s.Store.List(ctx, endUser, scopes, at, s.max()*len(scopes))
		if err != nil {
			return nil, err
		}
		hits = make([]Hit, len(all))
		for i, m := range all {
			hits[i] = Hit{Memory: m, Score: TextSimilarity(text, m.Content)}
		}
		hits = rank(hits, len(hits))
	}
	hits = slices.DeleteFunc(hits, func(h Hit) bool { return h.BusinessLine != reader && !h.Valid(now) })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	for i := range hits {
		hits[i].Recorded = s.Date(hits[i].CreatedAt)
	}
	return hits, nil
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
// q 非空时按与 q 的相关度排序，否则按最近更新排序；history 为 true 时包含失效与过期的。供 EndUser 管理自己的 Memory。
func (s *Service) List(ctx context.Context, businessLine, endUser string, category Category, q string, history bool, limit int) ([]Hit, error) {
	scope := Scope{BusinessLine: businessLine}
	if category != "" {
		if !ValidCategory(category) {
			return nil, fmt.Errorf("%w: unknown category %q", ErrRejected, category)
		}
		scope.Categories = []Category{category}
	}
	at := s.Clock.Now()
	if history {
		at = Everything
	}
	all, err := s.Store.List(ctx, endUser, []Scope{scope}, at, 1<<20)
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
