package memory_test

import (
	"context"
	"errors"
	"fmt"
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
	req := memory.SaveRequest{BusinessLine: "a", EndUser: "u", SessionID: "ses_v1_000_000_s1", CallID: "c1", Category: memory.Preference, Content: "喜欢喝咖啡"}
	m1, err := s.Save(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := s.Save(ctx, req) // 同一调用重复执行
	if m1.ID != scopedMemoryID("a", "u", "ses_v1_000_000_s1", "c1") || m2.ID != m1.ID {
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
	if err := save("e", "搬家到北京", memoryID("a")); err != nil {
		t.Fatalf("replace at limit: %v", err)
	}
	if m, err := st.Get(ctx, "u", memoryID("a")); err != nil || m.SupersededBy != memoryID("e") {
		t.Fatalf("replaced memory = %+v, %v; want it kept as history", m, err)
	}
	if err := save("f", "x", memoryID("missing")); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("replace unknown: %v", err)
	}
	if err := save("g", "搬家到上海", memoryID("a")); !errors.Is(err, memory.ErrNotFound) {
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
	_, _ = save("sh", memory.Profile, "用户搬到上海了", memoryID("bj"), "")
	_, _ = save("hz", memory.Profile, "用户搬到杭州了", memoryID("sh"), "")
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
	if text := model.Text(out); err != nil || !strings.Contains(text, fmt.Sprintf("用户常住北京（%s，2026-10-20 记，2026-10-20 被 %s 取代）", memoryID("bj"), memoryID("sh"))) ||
		!strings.Contains(text, fmt.Sprintf("用户周三去杭州出差（%s，2026-10-20 记，2026-10-21 后已过期）", memoryID("trip"))) {
		t.Fatalf("memory_search history = %q, %v", text, err)
	}

	if err := s.Forget(ctx, "a", "u", memoryID("hz")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{memoryID("hz"), memoryID("sh"), memoryID("bj")} {
		if _, err := st.Get(ctx, "u", id); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("%s survived forgetting the chain", id)
		}
	}
	clk.Advance(31 * 24 * time.Hour)
	_, _ = save("next", memory.Interest, "用户喜欢爬山", "", "")
	if _, err := st.Get(ctx, "u", memoryID("trip")); !errors.Is(err, memory.ErrNotFound) {
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
		"same":     {memory.Preference, "用户不吃香菜。", memoryID("c1")},
		"other":    {memory.Plan, "用户不吃香菜", memoryID("c1")},
		"opposite": {memory.Preference, "用户吃香菜", memoryID("opposite")},
		"number":   {memory.Relationship, "张三的电话是 13800138001", memoryID("number")},
	} {
		if got := save(call, c.cat, c.content); got != c.want {
			t.Errorf("%s: saved as %s, want %s", call, got, c.want)
		}
	}
	// 先写了新的，再"把旧的换成它"：不再写第三条，旧的删除。
	save("old", memory.Profile, "用户常住北京")
	save("new", memory.Profile, "用户搬到上海了")
	related, err := s.Related(ctx, &memory.Memory{ID: memoryID("new"), BusinessLine: "a", EndUser: "u", Category: memory.Profile, Content: "用户搬到上海了"}, 5)
	if err != nil || len(related) != 1 || related[0].ID != memoryID("old") {
		t.Fatalf("related = %v, %v", related, err)
	}
	m, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "again", Category: memory.Profile, Content: "用户搬到上海了", Replaces: memoryID("old")})
	if err != nil || m.ID != memoryID("new") {
		t.Fatalf("replace with an existing fact = %v, %v", m, err)
	}
	if m, err := st.Get(ctx, "u", memoryID("old")); err != nil || m.SupersededBy != memoryID("new") {
		t.Fatalf("replaced memory = %+v, %v; want it superseded by the existing fact", m, err)
	}
}

func TestSaveWithdrawsWhenSessionDeleted(t *testing.T) {
	ctx := context.Background()
	s, st := newService(clock.Real{})
	d := lifecycle.NewMemDeletions()
	s.Deletions = d
	_, _ = d.Mark(ctx, &lifecycle.Tombstone{SessionID: "ses_v1_000_000_gone", BusinessLine: "a", RequestedAt: time.Now()})
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", SessionID: "ses_v1_000_000_gone", CallID: "c", Category: memory.Plan, Content: "x"}); err == nil {
		t.Fatal("saved into a deleted session")
	}
	if st.HasSession("ses_v1_000_000_gone") {
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
			out[h.SourceCall] = true
		}
		return out
	}
	prefs := []memory.Category{memory.Preference}
	cases := map[string]struct {
		grant func(s *memory.Service)
		want  map[string]bool
	}{
		"no grant": {func(*memory.Service) {}, map[string]bool{"own": true}},
		"active preference grant": {func(s *memory.Service) { _, _ = s.CreateGrant(ctx, "u", "a", "b", prefs, time.Time{}) },
			map[string]bool{"own": true, "pref": true}},
		"grant to another reader": {func(s *memory.Service) { _, _ = s.CreateGrant(ctx, "u", "a", "c", prefs, time.Time{}) },
			map[string]bool{"own": true}},
		"expired": {func(s *memory.Service) { _, _ = s.CreateGrant(ctx, "u", "a", "b", prefs, t0) },
			map[string]bool{"own": true}},
		"revoked": {func(s *memory.Service) {
			g, _ := s.CreateGrant(ctx, "u", "a", "b", prefs, time.Time{})
			_ = s.Grants.Revoke(ctx, "u", g.ID, t0)
		}, map[string]bool{"own": true}},
		"two grants union categories": {func(s *memory.Service) {
			_, _ = s.CreateGrant(ctx, "u", "a", "b", prefs, time.Time{})
			_, _ = s.CreateGrant(ctx, "u", "a", "b", []memory.Category{memory.Relationship}, time.Time{})
		}, map[string]bool{"own": true, "pref": true, "rel": true}},
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
	s.Health = func(_ context.Context, bl, _ string) (bool, error) { return enabled[bl], nil }

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

// TestHealthPerEndUserConsent：业务线开启健康信息之后，仍只对已登记单独同意的 EndUser 写入、召回、检索；经 Grant 读取时
// 看写入方业务线对该 EndUser 的同意（ADR-0022 修订）。
func TestHealthPerEndUserConsent(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(clock.Real{})
	s.MaxPerUser = 10
	consented := map[[2]string]bool{{"clinic", "u"}: true}
	s.Health = func(_ context.Context, bl, eu string) (bool, error) { return consented[[2]string{bl, eu}], nil }
	health := func(eu, call, content string) error {
		req := memory.SaveRequest{BusinessLine: "clinic", EndUser: eu, CallID: call, Category: memory.Health, Content: content}
		checked := s.Check(ctx, req)
		_, err := s.Save(ctx, req)
		if (checked == nil) != (err == nil) {
			t.Fatalf("%s: Check %v, Save %v", content, checked, err)
		}
		return err
	}
	if err := health("v", "c1", "对花生过敏"); !errors.Is(err, memory.ErrRejected) || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("health saved for a user without consent: %v", err)
	}
	if err := health("u", "c2", "对青霉素过敏"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "clinic", EndUser: "u", CallID: "c3", Category: memory.Preference, Content: "喜欢清淡"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGrant(ctx, "u", "clinic", "shop", []memory.Category{memory.Health, memory.Preference}, time.Time{}); err != nil {
		t.Fatal(err)
	}
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
	both, plain := []string{"喜欢清淡", "对青霉素过敏"}, []string{"喜欢清淡"}
	if got := contents("clinic"); !slices.Equal(got, both) {
		t.Fatalf("clinic recall %v", got)
	}
	// shop 自己没有该用户的同意，不影响经 Grant 读取 clinic 的健康信息：看的是写入方。
	if got := contents("shop"); !slices.Equal(got, both) {
		t.Fatalf("shop with grant %v", got)
	}
	// 撤回：不再写入、召回、检索，经 Grant 也读不到；已有的不删除，仍可列出。
	consented[[2]string{"clinic", "u"}] = false
	if got := contents("clinic"); !slices.Equal(got, plain) {
		t.Fatalf("clinic after withdrawal %v", got)
	}
	if got := contents("shop"); !slices.Equal(got, plain) {
		t.Fatalf("shop after withdrawal %v", got)
	}
	if err := health("u", "c4", "对芒果过敏"); !errors.Is(err, memory.ErrRejected) {
		t.Fatalf("health saved after withdrawal: %v", err)
	}
	if listed, err := s.List(ctx, "clinic", "u", memory.Health, "", false, 10); err != nil || len(listed) != 1 {
		t.Fatalf("withdrawal deleted existing health memories: %v, %v", listed, err)
	}
	// 重新同意后照常可用。
	consented[[2]string{"clinic", "u"}] = true
	if got := contents("clinic"); !slices.Equal(got, both) {
		t.Fatalf("clinic after consent again %v", got)
	}
}

// TestHealthConsentReadFailure：读不到同意记录时预检查不下结论（不把读失败当作拒绝交给模型），写入失败，召回报错。
func TestHealthConsentReadFailure(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(clock.Real{})
	failed := errors.New("consent store down")
	s.Health = func(context.Context, string, string) (bool, error) { return false, failed }
	req := memory.SaveRequest{BusinessLine: "clinic", EndUser: "u", CallID: "c1", Category: memory.Health, Content: "对青霉素过敏"}
	if err := s.Check(ctx, req); err != nil {
		t.Fatalf("Check turned a read failure into %v", err)
	}
	if _, err := s.Save(ctx, req); !errors.Is(err, failed) {
		t.Fatalf("Save = %v, want the read failure", err)
	}
	if _, err := s.Search(ctx, "u", "clinic", "", 10); !errors.Is(err, failed) {
		t.Fatalf("Search = %v, want the read failure", err)
	}
	// 与健康无关的预检查不读同意记录。
	if err := s.Check(ctx, memory.SaveRequest{BusinessLine: "clinic", EndUser: "u", CallID: "c2", Category: memory.Preference, Content: "喜欢清淡"}); err != nil {
		t.Fatal(err)
	}
}

// TestForgetCategory：撤回同意时清除该用户在本业务线的健康信息，连同被健康条目取代的旧条目（以别的类别记下的同一件事）；
// 其他类别、其他业务线、其他用户不受影响。
func TestForgetCategory(t *testing.T) {
	ctx := context.Background()
	s, st := newService(clock.Real{})
	s.Health = func(context.Context, string, string) (bool, error) { return true, nil }
	now := time.Now()
	for _, m := range []*memory.Memory{
		{ID: "mem_old", BusinessLine: "a", EndUser: "u", Category: memory.Relationship, Content: "用户的母亲血压偏高"},
		{ID: "mem_pref", BusinessLine: "a", EndUser: "u", Category: memory.Preference, Content: "喜欢清淡"},
		{ID: "mem_b", BusinessLine: "b", EndUser: "u", Category: memory.Health, Content: "对花生过敏"},
		{ID: "mem_v", BusinessLine: "a", EndUser: "v", Category: memory.Health, Content: "对花生过敏"},
	} {
		m.CreatedAt, m.UpdatedAt = now, now
		if err := st.Put(ctx, m, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "c1", Category: memory.Health, Content: "用户的母亲血压偏高"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "c2", Category: memory.Health, Content: "对青霉素过敏"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.ForgetCategory(ctx, "a", "u", memory.Health)
	if err != nil || n != 3 {
		t.Fatalf("ForgetCategory = %d, %v; want 3 (two health memories and the one superseded by them)", n, err)
	}
	for id, kept := range map[string]bool{"mem_old": false, memoryID("c1"): false, memoryID("c2"): false} {
		if _, err := st.Get(ctx, "u", id); (err == nil) != kept {
			t.Fatalf("%s kept=%v: %v", id, kept, err)
		}
	}
	for _, k := range [][2]string{{"u", "mem_pref"}, {"u", "mem_b"}, {"v", "mem_v"}} {
		if _, err := st.Get(ctx, k[0], k[1]); err != nil {
			t.Fatalf("%s was deleted: %v", k[1], err)
		}
	}
	if n, err := s.ForgetCategory(ctx, "a", "u", memory.Health); err != nil || n != 0 {
		t.Fatalf("second ForgetCategory = %d, %v", n, err)
	}
}

// TestHealthByContent：类别是模型自报的，健康信息（包括他人的）写成其他类别时同样受业务线的同意约束：
// 未开启的业务线一律拒绝，开启的业务线要求改用 health（工作日评审 F3：母亲的血压以 relationship 写入）。
// Check 与 Save 的结论相同：Worker 据此在审批之前拒绝。
func TestHealthByContent(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(clock.Real{})
	s.MaxPerUser = 20
	s.Health = func(_ context.Context, bl, _ string) (bool, error) { return bl == "clinic", nil }
	n := 0
	save := func(bl string, c memory.Category, content string) error {
		n++
		req := memory.SaveRequest{BusinessLine: bl, EndUser: "u", CallID: fmt.Sprint("c", n), Category: c, Content: content}
		checked := s.Check(ctx, req)
		_, err := s.Save(ctx, req)
		if (checked == nil) != (err == nil) {
			t.Fatalf("%s: Check %v, Save %v", content, checked, err)
		}
		return err
	}
	for _, content := range []string{"用户的母亲血压偏高", "母亲长期服用降压药", "用户乳糖不耐受", "父亲需要透析", "母亲收缩压偏高"} {
		for _, c := range []memory.Category{memory.Relationship, memory.Health} {
			if err := save("shop", c, content); !errors.Is(err, memory.ErrRejected) || !strings.Contains(err.Error(), "not enabled") {
				t.Fatalf("shop saved %q as %s: %v", content, c, err)
			}
		}
		if err := save("clinic", memory.Relationship, content); !errors.Is(err, memory.ErrRejected) || !strings.Contains(err.Error(), "category health") {
			t.Fatalf("clinic saved %q as relationship: %v", content, err)
		}
		if err := save("clinic", memory.Health, content); err != nil {
			t.Fatalf("%s: %v", content, err)
		}
	}
	// 不收只在别的意思里出现的词：职业、习语、偏好照常写入。
	for _, content := range []string{"张三是医生", "用户对加班不感冒", "用户喜欢健康饮食", "用户负责压力测试", "用户是心理咨询师",
		"用户从事药物研发", "用户负责复查合同", "张三是住院医师", "李四是心理医生"} {
		if err := save("shop", memory.Profile, content); err != nil {
			t.Fatalf("%s: %v", content, err)
		}
	}
}

// TestHealthSupersedesMiscategorized：同一件健康信息之前以别的类别记下（按内容认定之前写入的）时，写入 health 不被当作
// 重复吞掉：新的照常写入，旧的失效，不再以原来的类别被召回或经 Grant 读取。
func TestHealthSupersedesMiscategorized(t *testing.T) {
	ctx := context.Background()
	s, st := newService(clock.Real{})
	s.Health = func(context.Context, string, string) (bool, error) { return true, nil }
	old := &memory.Memory{ID: "mem_old", BusinessLine: "a", EndUser: "u", Category: memory.Relationship, Content: "用户的母亲血压偏高",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.Put(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	m, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "c1", Category: memory.Health, Content: "用户的母亲血压偏高"})
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != memoryID("c1") || m.Category != memory.Health {
		t.Fatalf("saved %+v, want a new health memory", m)
	}
	if got, err := st.Get(ctx, "u", "mem_old"); err != nil || got.SupersededBy != memoryID("c1") {
		t.Fatalf("old memory %+v %v, want superseded by mem_c1", got, err)
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
	if _, err := st.Get(ctx, "u", old.ID); err != nil {
		t.Fatal("old memory deleted by a rejected replace")
	}
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: "u", CallID: "c3", Category: memory.Relationship, Content: "张三的邮箱是 zhangsan@example.com", Replaces: old.ID}); err != nil {
		t.Fatal(err)
	}
}

// TestGrantsIdentityDomain 验证跨业务线 Memory Grant 的身份域约束（ADR-0064）：
// 同域可建且召回与列举生效；跨域、任一方未登记、任一方身份域为空（包括另一方显式写前者名称）均拒绝；
// 配置改为跨域后 Scopes 与 ListGrants 立即跳过旧授权，RevokeGrant 仍可用；IdentityDomain 为 nil 时维持现状。
func TestGrantsIdentityDomain(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	s, _ := newService(clk)
	s.MaxPerUser = 100

	domains := map[string]string{
		"a":     "corp-secret-domain",
		"b":     "corp-secret-domain",
		"c":     "other-domain",
		"solo1": "",
		"solo2": "",
		"mimic": "solo1", // 显式把身份域写成 solo1 的业务线名，仍不得与未声明身份域的 solo1 同域
	}
	s.IdentityDomain = func(bl string) (string, bool) {
		d, ok := domains[bl]
		return d, ok
	}

	for call, r := range map[string]memory.SaveRequest{
		"a1": {BusinessLine: "a", Category: memory.Preference, Content: "A 的偏好"},
		"b1": {BusinessLine: "b", Category: memory.Plan, Content: "B 自己的计划"},
	} {
		r.EndUser, r.CallID = "u", call
		if _, err := s.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	prefs := []memory.Category{memory.Preference}

	// 同域可建，召回与 ListGrants 均生效。
	g1, err := s.CreateGrant(ctx, "u", "a", "b", prefs, time.Time{})
	if err != nil {
		t.Fatalf("same-domain CreateGrant failed: %v", err)
	}
	g2, err := s.CreateGrant(ctx, "u", "a", "b", []memory.Category{memory.Relationship}, time.Time{})
	if err != nil {
		t.Fatalf("second same-domain CreateGrant failed: %v", err)
	}
	scopes, err := s.Scopes(ctx, "u", "b", t0)
	if err != nil || len(scopes) != 2 || scopes[1].BusinessLine != "a" {
		t.Fatalf("same-domain Scopes = %+v, %v", scopes, err)
	}
	hits, err := s.Search(ctx, "u", "b", "", 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("same-domain Search = %+v, %v", hits, err)
	}
	listed, err := s.ListGrants(ctx, "u", "b")
	if err != nil || len(listed) != 2 {
		t.Fatalf("same-domain ListGrants = %+v, %v", listed, err)
	}

	// 跨域、未登记、双空、单空+同名伪造均被拒绝，且错误只含业务线名、不含身份域名。
	rejected := []struct {
		name     string
		from, to string
		wantSubs []string
	}{
		{"cross domain", "a", "c", []string{"identity domain", "a", "c"}},
		{"unknown to", "a", "ghost", []string{"ghost"}},
		{"unknown from", "ghost", "a", []string{"ghost"}},
		{"both empty", "solo1", "solo2", []string{"identity domain", "solo1", "solo2"}},
		{"empty vs explicit business line name (from empty)", "solo1", "mimic", []string{"identity domain", "solo1", "mimic"}},
		{"empty vs explicit business line name (to empty)", "mimic", "solo1", []string{"identity domain", "mimic", "solo1"}},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateGrant(ctx, "u", tc.from, tc.to, prefs, time.Time{})
			if !errors.Is(err, memory.ErrRejected) {
				t.Fatalf("CreateGrant(%s, %s) err = %v, want ErrRejected", tc.from, tc.to, err)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("err %q missing %q", err.Error(), sub)
				}
			}
			if strings.Contains(err.Error(), "corp-secret-domain") || strings.Contains(err.Error(), "other-domain") {
				t.Errorf("err %q leaked identity domain name", err.Error())
			}
		})
	}

	// 配置变更：把 b 移到另一个身份域后，Scopes 与 ListGrants 不再返回旧授权，但 RevokeGrant 仍可用。
	domains["b"] = "split-domain"
	scopes, err = s.Scopes(ctx, "u", "b", t0)
	if err != nil || len(scopes) != 1 || scopes[0].BusinessLine != "b" {
		t.Fatalf("after split Scopes = %+v, %v; want only b", scopes, err)
	}
	hits, err = s.Search(ctx, "u", "b", "", 10)
	if err != nil || len(hits) != 1 || hits[0].BusinessLine != "b" {
		t.Fatalf("after split Search = %+v, %v; want only b", hits, err)
	}
	if listed, err := s.ListGrants(ctx, "u", "b"); err != nil || len(listed) != 0 {
		t.Fatalf("after split ListGrants(b) = %+v, %v; want empty", listed, err)
	}
	if listed, err := s.ListGrants(ctx, "u", "a"); err != nil || len(listed) != 0 {
		t.Fatalf("after split ListGrants(a) = %+v, %v; want empty", listed, err)
	}
	// 撤销不受身份域变化影响，便于清理。
	if err := s.RevokeGrant(ctx, "u", "b", g1.ID); err != nil {
		t.Fatalf("RevokeGrant after split failed: %v", err)
	}
	// 若从配置中移除 a（未登记），同样跳过旧授权。
	delete(domains, "a")
	if scopes, err := s.Scopes(ctx, "u", "b", t0); err != nil || len(scopes) != 1 {
		t.Fatalf("after unregistering a Scopes = %+v, %v", scopes, err)
	}
	// 若配置改回同域：已撤销的 g1 仍不生效，未撤销的 g2 恢复可见（ADR-0064 Consequences）。
	domains["a"] = "corp-secret-domain"
	domains["b"] = "corp-secret-domain"
	scopes, err = s.Scopes(ctx, "u", "b", t0)
	if err != nil || len(scopes) != 2 || !slices.Equal(scopes[1].Categories, []memory.Category{memory.Relationship}) {
		t.Fatalf("after rejoin Scopes = %+v, %v; want only unrevoked g2", scopes, err)
	}
	if listed, err := s.ListGrants(ctx, "u", "b"); err != nil || len(listed) != 2 {
		t.Fatalf("after rejoin ListGrants = %+v, %v; want 2 (including revoked g1)", listed, err)
	}
	if err := s.RevokeGrant(ctx, "u", "a", g2.ID); err != nil {
		t.Fatalf("RevokeGrant g2 failed: %v", err)
	}

	// IdentityDomain 为 nil 时不做检查（开发模式、评测与模拟测试维持现状）。
	unrestricted, _ := newService(clk)
	if _, err := unrestricted.Save(ctx, memory.SaveRequest{BusinessLine: "x", EndUser: "u", CallID: "x1", Category: memory.Preference, Content: "X 的偏好"}); err != nil {
		t.Fatal(err)
	}
	if _, err := unrestricted.CreateGrant(ctx, "u", "x", "y", prefs, time.Time{}); err != nil {
		t.Fatalf("nil IdentityDomain CreateGrant failed: %v", err)
	}
	if scopes, err := unrestricted.Scopes(ctx, "u", "y", t0); err != nil || len(scopes) != 2 {
		t.Fatalf("nil IdentityDomain Scopes = %+v, %v", scopes, err)
	}
	if listed, err := unrestricted.ListGrants(ctx, "u", "y"); err != nil || len(listed) != 1 {
		t.Fatalf("nil IdentityDomain ListGrants = %+v, %v", listed, err)
	}
}

// 测试用实际调用身份引用调用派生的对象。
func scopedMemoryID(line, endUser, sessionID, callID string) string {
	id, err := memory.CallMemoryID(memory.SaveRequest{BusinessLine: line, EndUser: endUser, SessionID: sessionID, CallID: callID})
	if err != nil {
		panic(err)
	}
	return id
}

func memoryID(callID string) string { return scopedMemoryID("a", "u", "", callID) }
