package memory_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/memory/memorytest"
	"yanshi/internal/model"
)

func TestMemStore(t *testing.T) {
	memorytest.RunStore(t, func(*testing.T) memory.Store { return memory.NewMemStore() })
	memorytest.RunAccess(t, func(*testing.T) memory.Store { return memory.NewMemStore() })
}

func TestMemGrants(t *testing.T) {
	memorytest.RunGrants(t, func(*testing.T) memory.Grants { return memory.NewMemGrants() })
}

func TestTextSimilarity(t *testing.T) {
	q := "帮我给同事张三发邮件"
	if a, b := memory.TextSimilarity(q, "张三是同事，邮箱 zs@example.com"), memory.TextSimilarity(q, "偏好简洁的回答"); a <= b || b != 0 {
		t.Fatalf("related = %v, unrelated = %v", a, b)
	}
	if memory.TextSimilarity("Prefers SHORT answers", "prefers short answers") != 1 {
		t.Fatal("case and punctuation should not matter")
	}
}

func newService(clk clock.Clock) (*memory.Service, *memory.MemStore) {
	st := memory.NewMemStore()
	return &memory.Service{Store: st, Grants: memory.NewMemGrants(), Clock: clk, IDs: ids.Sequential("x"), MaxPerUser: 3}, st
}

func TestSaveValidatesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, st := newService(clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	req := memory.SaveRequest{BusinessLine: "a", EndUser: "u", SessionID: "s1", CallID: "c1", Category: memory.Preference, Content: "喜欢喝咖啡"}
	m1, err := s.Save(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := s.Save(ctx, req) // 同一调用重复执行
	if m1.ID != "mem_c1" || m2.ID != m1.ID {
		t.Fatalf("ids = %s, %s", m1.ID, m2.ID)
	}
	if n, _ := st.Count(ctx, "a", "u", time.Now()); n != 1 {
		t.Fatalf("count = %d", n)
	}
	for name, r := range map[string]memory.SaveRequest{
		"unknown category": {Category: "health", Content: "x"},
		"empty":            {Category: memory.Plan, Content: "  "},
		"id card":          {Category: memory.Profile, Content: "身份证号 11010519491231002X"},
		"bank card":        {Category: memory.Profile, Content: "银行卡 4111 1111 1111 1111"},
	} {
		r.BusinessLine, r.EndUser, r.CallID = "a", "u", name
		if _, err := s.Save(ctx, r); !errors.Is(err, memory.ErrRejected) {
			t.Errorf("%s: err = %v, want rejected", name, err)
		}
	}
	// 普通的电话号码不属于被拒绝的号码（不通过 Luhn 校验）。
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "c2", Category: memory.Relationship, Content: "张三电话 13800138000"}); err != nil {
		t.Fatalf("phone number rejected: %v", err)
	}
}

func TestLimitAndReplace(t *testing.T) {
	ctx := context.Background()
	s, st := newService(clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	save := func(call, content, replaces string) error {
		_, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: call, Category: memory.Plan, Content: content, Replaces: replaces})
		return err
	}
	for i, c := range []string{"一", "二", "三"} {
		if err := save(string(rune('a'+i)), c, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := save("d", "四", ""); !errors.Is(err, memory.ErrLimit) {
		t.Fatalf("over limit: %v", err)
	}
	if err := save("e", "搬家到北京", "mem_a"); err != nil {
		t.Fatalf("replace at limit: %v", err)
	}
	if m, err := st.Get(ctx, "mem_a"); err != nil || m.SupersededBy != "mem_e" {
		t.Fatalf("replaced memory = %+v, %v; want it kept as history", m, err)
	}
	if err := save("f", "x", "mem_missing"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("replace unknown: %v", err)
	}
	if err := save("g", "搬家到上海", "mem_a"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("replace a superseded memory: %v", err)
	}
}

// 有效期：过了最后一天不再检索、不计入上限；history 检索带上失效与过期的，并注明原因；忘记一条时连同被它取代的链一起删除；
// 失效满 180 天、过期满 30 天的在下次写入时清除。
func TestValidityAndHistory(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)) // 当地 10-20 10:00
	s, st := newService(clk)
	s.MaxPerUser = 100
	save := func(call string, cat memory.Category, content, replaces, until string) (*memory.Memory, error) {
		return s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: call, Category: cat, Content: content, Replaces: replaces, ValidUntil: until})
	}
	if _, err := save("past", memory.Plan, "上周出差", "", "2026-10-19"); !errors.Is(err, memory.ErrRejected) {
		t.Fatalf("past valid_until: %v", err)
	}
	if _, err := save("bad", memory.Plan, "出差", "", "下周三"); !errors.Is(err, memory.ErrRejected) {
		t.Fatalf("invalid valid_until: %v", err)
	}
	trip, err := save("trip", memory.Plan, "用户周三去杭州出差", "", "2026-10-21")
	if err != nil || !trip.ValidUntil.Equal(time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("trip = %+v, %v", trip, err)
	}
	_, _ = save("bj", memory.Profile, "用户常住北京", "", "")
	_, _ = save("sh", memory.Profile, "用户搬到上海了", "mem_bj", "")
	_, _ = save("hz", memory.Profile, "用户搬到杭州了", "mem_sh", "")
	contents := func(hits []memory.Hit) []string {
		var out []string
		for _, h := range hits {
			out = append(out, h.Content)
		}
		slices.Sort(out)
		return out
	}
	hits, _ := s.Search(ctx, "u", "a", "", 10)
	if got := contents(hits); !slices.Equal(got, []string{"用户周三去杭州出差", "用户搬到杭州了"}) || hits[0].Recorded != "2026-10-20" {
		t.Fatalf("valid = %v (%+v)", got, hits)
	}
	clk.Advance(48 * time.Hour) // 当地 10-22：出差已过期
	hits, _ = s.Search(ctx, "u", "a", "", 10)
	if got := contents(hits); !slices.Equal(got, []string{"用户搬到杭州了"}) {
		t.Fatalf("after expiry = %v", got)
	}
	hits, _ = s.SearchHistory(ctx, "u", "a", "", 10)
	if got := contents(hits); len(got) != 4 {
		t.Fatalf("history = %v", got)
	}
	out, err := memory.Capabilities(s)[2].Invoke(ctx, capability.Invocation{BusinessLine: "a", EndUser: "u", Arguments: `{"query":"住","history":true}`})
	if text := model.Text(out); err != nil || !strings.Contains(text, "用户常住北京（mem_bj，2026-10-20 记，2026-10-20 被 mem_sh 取代）") ||
		!strings.Contains(text, "用户周三去杭州出差（mem_trip，2026-10-20 记，2026-10-21 后已过期）") {
		t.Fatalf("memory_search history = %q, %v", text, err)
	}

	if err := s.Forget(ctx, "a", "u", "mem_hz"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mem_hz", "mem_sh", "mem_bj"} {
		if _, err := st.Get(ctx, id); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("%s survived forgetting the chain", id)
		}
	}
	clk.Advance(31 * 24 * time.Hour)
	_, _ = save("next", memory.Interest, "用户喜欢爬山", "", "")
	if _, err := st.Get(ctx, "mem_trip"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatal("expired memory not purged after 30 days")
	}
}

// 几乎相同的一条（不论类别）不再写入，返回已有的那条；号码不同、意思相反的照常写入。替换为已经记着的内容时只删除被替换的。
func TestSaveSkipsDuplicates(t *testing.T) {
	ctx := context.Background()
	s, st := newService(clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	s.MaxPerUser = 100
	save := func(call string, cat memory.Category, content string) string {
		t.Helper()
		m, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: call, Category: cat, Content: content})
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	save("c1", memory.Preference, "用户不吃香菜")
	save("c2", memory.Relationship, "张三的电话是 13800138000")
	for call, c := range map[string]struct {
		cat     memory.Category
		content string
		want    string
	}{
		"same":     {memory.Preference, "用户不吃香菜。", "mem_c1"},
		"other":    {memory.Plan, "用户不吃香菜", "mem_c1"},
		"opposite": {memory.Preference, "用户吃香菜", "mem_opposite"},
		"number":   {memory.Relationship, "张三的电话是 13800138001", "mem_number"},
	} {
		if got := save(call, c.cat, c.content); got != c.want {
			t.Errorf("%s: saved as %s, want %s", call, got, c.want)
		}
	}
	// 先写了新的，再"把旧的换成它"：不再写第三条，旧的删除。
	save("old", memory.Profile, "用户常住北京")
	save("new", memory.Profile, "用户搬到上海了")
	related, err := s.Related(ctx, &memory.Memory{ID: "mem_new", BusinessLine: "a", EndUser: "u", Category: memory.Profile, Content: "用户搬到上海了"}, 5)
	if err != nil || len(related) != 1 || related[0].ID != "mem_old" {
		t.Fatalf("related = %v, %v", related, err)
	}
	m, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "again", Category: memory.Profile, Content: "用户搬到上海了", Replaces: "mem_old"})
	if err != nil || m.ID != "mem_new" {
		t.Fatalf("replace with an existing fact = %v, %v", m, err)
	}
	if m, err := st.Get(ctx, "mem_old"); err != nil || m.SupersededBy != "mem_new" {
		t.Fatalf("replaced memory = %+v, %v; want it superseded by the existing fact", m, err)
	}
}

func TestSaveWithdrawsWhenSessionDeleted(t *testing.T) {
	ctx := context.Background()
	s, st := newService(clock.Real{})
	d := lifecycle.NewMemDeletions()
	s.Deletions = d
	_, _ = d.Mark(ctx, &lifecycle.Tombstone{SessionID: "gone", BusinessLine: "a", RequestedAt: time.Now()})
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", SessionID: "gone", CallID: "c", Category: memory.Plan, Content: "x"}); err == nil {
		t.Fatal("saved into a deleted session")
	}
	if st.HasSession("gone") {
		t.Fatal("write was not withdrawn")
	}
}

// TestGrantsControlCrossLineRecall 是跨业务线召回矩阵：Grant 的不同状态 × 检索结果。
func TestGrantsControlCrossLineRecall(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	setup := func() *memory.Service {
		s, _ := newService(clk)
		s.MaxPerUser = 100
		for call, r := range map[string]memory.SaveRequest{
			"own":  {BusinessLine: "b", Category: memory.Plan, Content: "B 自己的"},
			"pref": {BusinessLine: "a", Category: memory.Preference, Content: "A 的偏好"},
			"rel":  {BusinessLine: "a", Category: memory.Relationship, Content: "A 的人际关系"},
			"c":    {BusinessLine: "c", Category: memory.Preference, Content: "C 的偏好"},
		} {
			r.EndUser, r.CallID = "u", call
			if _, err := s.Save(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	visible := func(s *memory.Service) map[string]bool {
		hits, err := s.Search(ctx, "u", "b", "", 100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, h := range hits {
			out[h.ID] = true
		}
		return out
	}
	prefs := []memory.Category{memory.Preference}
	cases := map[string]struct {
		grant func(s *memory.Service)
		want  map[string]bool
	}{
		"no grant": {func(*memory.Service) {}, map[string]bool{"mem_own": true}},
		"active preference grant": {func(s *memory.Service) { _, _ = s.CreateGrant(ctx, "u", "a", "b", prefs, time.Time{}) },
			map[string]bool{"mem_own": true, "mem_pref": true}},
		"grant to another reader": {func(s *memory.Service) { _, _ = s.CreateGrant(ctx, "u", "a", "c", prefs, time.Time{}) },
			map[string]bool{"mem_own": true}},
		"expired": {func(s *memory.Service) { _, _ = s.CreateGrant(ctx, "u", "a", "b", prefs, t0) },
			map[string]bool{"mem_own": true}},
		"revoked": {func(s *memory.Service) {
			g, _ := s.CreateGrant(ctx, "u", "a", "b", prefs, time.Time{})
			_ = s.Grants.Revoke(ctx, g.ID, t0)
		}, map[string]bool{"mem_own": true}},
		"two grants union categories": {func(s *memory.Service) {
			_, _ = s.CreateGrant(ctx, "u", "a", "b", prefs, time.Time{})
			_, _ = s.CreateGrant(ctx, "u", "a", "b", []memory.Category{memory.Relationship}, time.Time{})
		}, map[string]bool{"mem_own": true, "mem_pref": true, "mem_rel": true}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := setup()
			c.grant(s)
			got := visible(s)
			if len(got) != len(c.want) {
				t.Fatalf("visible = %v, want %v", got, c.want)
			}
			for id := range c.want {
				if !got[id] {
					t.Fatalf("visible = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// TestHealthRequiresBusinessLineConsent：health 类别只有开启了单独同意的业务线可以写入、召回；
// 其他业务线即使得到用户对该类别的授权，也只能读取到已开启业务线写入的健康信息。
func TestHealthRequiresBusinessLineConsent(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(clock.Real{})
	s.MaxPerUser = 10
	enabled := map[string]bool{"clinic": true}
	s.HealthAllowed = func(bl string) bool { return enabled[bl] }

	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "shop", EndUser: "u", CallID: "c1", Category: memory.Health, Content: "对花生过敏"}); !errors.Is(err, memory.ErrRejected) {
		t.Fatalf("health saved without consent: %v", err)
	}
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "clinic", EndUser: "u", CallID: "c2", Category: memory.Health, Content: "对青霉素过敏"}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Save(ctx, memory.SaveRequest{BusinessLine: "clinic", EndUser: "u", CallID: "c3", Category: memory.Preference, Content: "喜欢清淡"})
	contents := func(reader string) []string {
		hits, err := s.Search(ctx, "u", reader, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, h.Content)
		}
		slices.Sort(out)
		return out
	}
	if got := contents("clinic"); !slices.Equal(got, []string{"喜欢清淡", "对青霉素过敏"}) {
		t.Fatalf("clinic recall %v", got)
	}
	// 用户把 clinic 的健康与偏好授权给 shop：shop 可以读到（写入方 clinic 已取得同意）。
	if _, err := s.CreateGrant(ctx, "u", "clinic", "shop", []memory.Category{memory.Health, memory.Preference}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := contents("shop"); !slices.Equal(got, []string{"喜欢清淡", "对青霉素过敏"}) {
		t.Fatalf("shop with grant %v", got)
	}
	// clinic 关闭开关（撤回同意）：它自己与被授权方都不再召回健康信息。
	enabled["clinic"] = false
	if got := contents("clinic"); !slices.Equal(got, []string{"喜欢清淡"}) {
		t.Fatalf("clinic after disabling %v", got)
	}
	if got := contents("shop"); !slices.Equal(got, []string{"喜欢清淡"}) {
		t.Fatalf("shop after clinic disabled %v", got)
	}
}

// TestReplaceRequiresSameCategory：replaces 指向不同类别的记忆时拒绝，旧记忆保留。
func TestReplaceRequiresSameCategory(t *testing.T) {
	ctx := context.Background()
	s, st := newService(clock.Real{})
	old, _ := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "c1", Category: memory.Relationship, Content: "张三的邮箱是 zs@example.com"})
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "c2", Category: memory.Preference, Content: "喜欢清淡", Replaces: old.ID}); !errors.Is(err, memory.ErrRejected) {
		t.Fatalf("cross-category replace: %v", err)
	}
	if _, err := st.Get(ctx, old.ID); err != nil {
		t.Fatal("old memory deleted by a rejected replace")
	}
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "c3", Category: memory.Relationship, Content: "张三的邮箱是 zhangsan@example.com", Replaces: old.ID}); err != nil {
		t.Fatal(err)
	}
}
