package nodesdk_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/sdk/nodesdk"
	"yanshi/sdk/nodesdk/ledgertest"
)

func TestLedgerConformance(t *testing.T) {
	t.Run("mem", func(t *testing.T) {
		ledgertest.Run(t, func(*testing.T) nodesdk.Ledger { return nodesdk.NewMemLedger() })
	})
	t.Run("file", func(t *testing.T) {
		ledgertest.Run(t, func(t *testing.T) nodesdk.Ledger { return nodesdk.FileLedger{Dir: t.TempDir()} })
	})
}

// TestFileLedgerReadsV1Records：早期版本写下的记录（无 Session 与时间）仍可读取，并按文件修改时间清理。
func TestFileLedgerReadsV1Records(t *testing.T) {
	dir := t.TempDir()
	res, _ := proto.Marshal(&v1.InvokeResult{CallId: "old", IsError: true})
	if err := os.WriteFile(filepath.Join(dir, "old"), append([]byte{byte(nodesdk.StateDone)}, res...), 0o600); err != nil {
		t.Fatal(err)
	}
	l := nodesdk.FileLedger{Dir: dir}
	r, err := l.Get("old")
	if err != nil || r.State != nodesdk.StateDone || !r.Result.GetIsError() || r.SessionID != "" {
		t.Fatalf("v1 record = %+v, %v", r, err)
	}
	if err := l.Prune(time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r, _ := l.Get("old"); r.State != nodesdk.StateDone {
		t.Fatal("pruned a fresh v1 record")
	}
	if err := l.Prune(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r, _ := l.Get("old"); r.State != nodesdk.StateNew {
		t.Fatal("old v1 record not pruned by modification time")
	}
}
