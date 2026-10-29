package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"persona/domain"
	"persona/pg/pgtest"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
)

// Q122 第二层：每个操作后比较全部可观察结果，不能只比较最后一份 State。
// 冻结脚本覆盖账本工具，旁听脚本覆盖分页、进度、擦除、重置与重新预置。
func TestStoreParity(t *testing.T) {
	for _, path := range []string{"testdata/parity_script.json", "testdata/capture_parity_script.json", "testdata/multi_batch_parity_script.json"} {
		var script parityScript
		readParity(t, path, &script)
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart_%t", path, restart), func(t *testing.T) {
				pool := pgtest.New(t)
				config := pool.Config()
				persistent := pgstore.New(pool, captureClock)
				defer func() { persistent.Close() }()
				memory := memstore.New(captureClock)
				left := newParityEndpoint(t, memory, script.Now)
				defer left.close()
				right := newParityEndpoint(t, persistent, script.Now)
				defer func() { right.close() }()
				ctx := context.Background()
				parityEqual(t, "tools/list", parityTools(t, ctx, right.conn), parityTools(t, ctx, left.conn))
				sessions := map[string]bool{"unbound": true}
				for i, step := range script.Steps {
					label := fmt.Sprintf("step %d %s %s", i, step.Op, step.Name)
					want := left.step(t, script, step)
					got := right.step(t, script, step)
					parityEqual(t, label, got, want)
					if path == "testdata/multi_batch_parity_script.json" {
						checkMultiBatchOutput(t, got)
					}
					if step.Session != "" {
						sessions[step.Session] = true
					}
					var input struct{ Session string }
					if json.Unmarshal(step.Args, &input) == nil && input.Session != "" {
						sessions[input.Session] = true
					}
					compare := func() {
						t.Helper()
						for _, op := range []string{"state", "week", "deletion_tasks"} {
							probe := parityStep{Op: op}
							parityEqual(t, label+"/"+op, right.step(t, script, probe), left.step(t, script, probe))
						}
						for session := range sessions {
							for _, op := range []string{"capture_position", "capture_preview"} {
								args := map[string]any{"session": session}
								if op == "capture_preview" {
									args["after"], args["all"] = "new", true
									args["page_options"] = map[string]int{"limit": 2, "max_chars": 6000, "gap_ms": 300000, "max_tool_result": 8000}
								}
								raw, err := json.Marshal(args)
								if err != nil {
									t.Fatal(err)
								}
								probe := parityStep{Op: op, Args: raw}
								parityEqual(t, label+"/"+op+"/"+session, right.step(t, script, probe), left.step(t, script, probe))
							}
						}
						pending := func(s store.Store) []store.Owner {
							t.Helper()
							owners, err := s.PendingDeletions(ctx)
							if err != nil {
								t.Fatal(err)
							}
							return owners
						}
						parityEqual(t, label+"/pending owners", pending(persistent), pending(memory))
					}
					compare()
					if restart && i == len(script.Steps)/2 {
						right.close()
						persistent.Close()
						// 使用同一 database/schema，不能重新调用 pgtest.New 丢掉前半段事实。
						pool, err := pgxpool.NewWithConfig(ctx, config)
						if err != nil {
							t.Fatal(err)
						}
						persistent = pgstore.New(pool, captureClock)
						right = newParityEndpoint(t, persistent, script.Now)
						parityEqual(t, "restart tools/list", parityTools(t, ctx, right.conn), parityTools(t, ctx, left.conn))
						compare()
					}
				}
			})
		}
	}
}

// 即使未启用 PG，也验证脚本确实推进处理位置并产生删除待办，避免两边同样失败而假通过。
func TestCaptureParitySequence(t *testing.T) {
	var script parityScript
	readParity(t, "testdata/capture_parity_script.json", &script)
	endpoint := newParityEndpoint(t, memstore.New(captureClock), script.Now)
	defer endpoint.close()
	positions := []string{"0", "1.2"}
	for _, step := range script.Steps {
		got := endpoint.step(t, script, step)
		switch step.Op {
		case "capture_position":
			if len(positions) == 0 {
				t.Fatal("unexpected position probe")
			}
			parityEqual(t, "processing position", got.Value, map[string]string{"after": positions[0]})
			positions = positions[1:]
		case "deletion_tasks":
			var tasks struct {
				Tasks []struct{ Status string } `json:"tasks"`
			}
			raw, err := json.Marshal(got.Value)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &tasks); err != nil || len(tasks.Tasks) != 1 || tasks.Tasks[0].Status != "pending" {
				t.Fatal("script did not create pending deletion", err)
			}
		case "reset", "seed":
			if got.Error != "" {
				t.Fatal(got.Error)
			}
		}
	}
	if len(positions) != 0 {
		t.Fatal("script lost processing position probes")
	}
}

// 没有 PG 时也检查预期引用，避免两份实现同样跳批而对照通过。
func TestMultiBatchParitySequence(t *testing.T) {
	var script parityScript
	readParity(t, "testdata/multi_batch_parity_script.json", &script)
	endpoint := newParityEndpoint(t, memstore.New(), script.Now)
	defer endpoint.close()
	for _, step := range script.Steps {
		checkMultiBatchOutput(t, endpoint.step(t, script, step))
	}
}

func checkMultiBatchOutput(t *testing.T, got parityOutput) {
	t.Helper()
	if got.Error != "" || got.IsError {
		t.Fatalf("multi-batch step failed: %+v", got)
	}
	if got.Input.Op == "capture_position" {
		parityEqual(t, "multi-batch processing position", got.Value, map[string]string{"after": "12.0"})
	}
	if got.Input.Op != "tool" || got.Input.Name != "captures" {
		return
	}
	var args struct {
		After string
		Limit int
	}
	if err := json.Unmarshal(got.Input.Args, &args); err != nil {
		t.Fatal(err)
	}
	first, ok := map[string]int{"0": 1, "9.0": 10, "8.0": 9, "10.0": 11}[args.After]
	if !ok {
		t.Fatal("unexpected multi-batch cursor", args.After)
	}
	var meta struct {
		Page domain.StructuredPage `json:"yanshi/page"`
	}
	raw, err := json.Marshal(got.Meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Page.Kind != "page" || len(meta.Page.Segments) != args.Limit {
		t.Fatalf("unexpected multi-batch page: %+v", meta.Page)
	}
	for i, segment := range meta.Page.Segments {
		if want := fmt.Sprintf("%d.0", first+i); segment.Ref != want {
			t.Fatalf("after %s: segment %d is %s, want %s", args.After, i, segment.Ref, want)
		}
	}
}
