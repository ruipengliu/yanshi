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
	"yanshi/internal/workqueue/memqueue"
)

// TestMediaMustBeOwnArtifact：输入中的 Media 只能引用本 Session 的工件，类型、大小、名称以工件元数据为准；
// 内联字节、外部链接、别的 Session 的工件都被拒绝，不写日志。
func TestMediaMustBeOwnArtifact(t *testing.T) {
	ctx := context.Background()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	arts := &artifact.Service{Meta: artifact.NewMemMeta(), Blobs: artifact.NewMemBlobs(), IDs: ids.Sequential("art"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Queue: memqueue.New(clock.Real{}), Agents: agents, Artifacts: arts}
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
	media := func(m *v1.Media) []*v1.ContentBlock {
		return append(model.TextBlocks("看看这张图"), &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: m}})
	}
	for name, m := range map[string]*v1.Media{
		"inline data":            {MimeType: "image/png", Uri: artifact.URI(own.ID), Data: []byte("unchecked bytes"), Size: 1 << 30},
		"external link":          {MimeType: "image/png", Uri: "https://example.com/x.png"},
		"another session's file": {MimeType: "image/png", Uri: artifact.URI(theirs.ID)},
		"unknown artifact":       {MimeType: "image/png", Uri: artifact.URI("art_missing")},
	} {
		if _, err := svc.Submit(ctx, mine, media(m)); !errors.Is(err, service.ErrInvalid) {
			t.Errorf("%s: %v, want invalid", name, err)
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
