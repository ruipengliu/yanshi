package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/shard"
	"yanshi/internal/workqueue/memqueue"
)

// TestMediaMustBeOwnArtifact：输入中的 Media 只能引用本 Session 的工件，类型、大小、名称以工件元数据为准；
// 内联字节、外部链接、别的 Session 的工件都被拒绝，不写日志。
func TestMediaMustBeOwnArtifact(t *testing.T) {
	ctx := context.Background()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	mlog, mq := memlog.New(), memqueue.New(clock.Real{})
	store := &session.Store{Log: mlog, Waking: mlog.Waking(mq), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	arts := &artifact.Service{Meta: artifact.NewMemMeta(), Blobs: artifact.NewMemBlobs(), IDs: ids.Sequential("art"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Agents: agents, Artifacts: arts}
	mine, _ := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	other, _ := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "v", Agent: "a"})
	own, err := arts.Put(ctx, mine, "photo.png", "image/png", strings.NewReader("png bytes"))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := arts.Put(ctx, other, "secret.png", "image/png", strings.NewReader("other"))
	if err != nil {
		t.Fatal(err)
	}
	// 伪造的槽位提示会定位到另一库，不能代替读出元数据后的真实归属校验。
	// C1 只验证定位纯函数与既有授权路径，实际多库适配留给 C4/C5。
	forged := "art_v1_000_forged"
	table, err := shard.NewTable(shard.Config{Shards: []int{0, 1}, Ranges: []shard.Range{{First: 0, Last: 1023, Shard: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	slot, legacy, err := ids.SlotOf(forged)
	if err != nil {
		t.Fatal(err)
	}
	if target, err := table.Shard(slot, legacy); err != nil || target != 1 {
		t.Fatalf("forged hint = %d, %v", target, err)
	}
	if err := arts.Meta.Create(ctx, &artifact.Meta{ID: forged, SessionID: other, MimeType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	before, err := svc.Load(ctx, mine)
	if err != nil {
		t.Fatal(err)
	}
	media := func(m *v1.Media) []*v1.ContentBlock {
		return append(model.TextBlocks("看看这张图"), &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: m}})
	}
	for name, m := range map[string]*v1.Media{
		"inline data":            {MimeType: "image/png", Uri: artifact.URI(own.ID), Data: []byte("unchecked bytes"), Size: 1 << 30},
		"external link":          {MimeType: "image/png", Uri: "https://example.com/x.png"},
		"another session's file": {MimeType: "image/png", Uri: artifact.URI(theirs.ID)},
		"unknown artifact":       {MimeType: "image/png", Uri: artifact.URI("art_missing")},
		"forged slot":            {MimeType: "image/png", Uri: artifact.URI(forged)},
	} {
		if _, err := svc.Submit(ctx, mine, media(m)); !errors.Is(err, service.ErrInvalid) {
			t.Errorf("%s: %v, want invalid", name, err)
		}
		after, err := svc.Load(ctx, mine)
		if err != nil || after.Seq != before.Seq {
			t.Fatalf("%s changed the log: %v", name, err)
		}
	}
	// 自报的类型与大小被元数据取代。
	in := media(&v1.Media{MimeType: "text/plain", Uri: artifact.URI(own.ID), Size: 1, Name: "fake.txt"})
	if _, err := svc.Submit(ctx, mine, in); err != nil {
		t.Fatal(err)
	}
	if in[1].GetMedia().GetMimeType() != "text/plain" {
		t.Fatal("the caller's input was modified")
	}
	st, _ := svc.Load(ctx, mine)
	var logged *v1.Media
	for _, e := range st.History {
		for _, b := range e.GetRunRequested().GetInput() {
			if b.GetMedia() != nil {
				logged = b.GetMedia()
			}
		}
	}
	if logged.GetMimeType() != "image/png" || logged.GetSize() != uint64(own.Size) || logged.GetName() != "photo.png" || len(logged.GetData()) != 0 {
		t.Fatalf("logged media = %v", logged)
	}
}
