package nodesdk

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
)

// MemLedger 是不持久化的 Ledger，仅用于测试。
type MemLedger struct {
	mu sync.Mutex
	m  map[string]*Record
}

func NewMemLedger() *MemLedger { return &MemLedger{m: map[string]*Record{}} }

func (l *MemLedger) Get(id string) (*Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r := l.m[id]; r != nil {
		cp := *r
		return &cp, nil
	}
	return &Record{}, nil
}

func (l *MemLedger) Put(id string, r *Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := *r
	l.m[id] = &cp
	return nil
}

// FileLedger 把每个 call_id 的记录存为目录下的一个文件，写入经 fsync 与原子重命名。
type FileLedger struct{ Dir string }

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (l FileLedger) path(id string) (string, error) {
	if !safeID.MatchString(id) {
		return "", errors.New("invalid call id")
	}
	return filepath.Join(l.Dir, id), nil
}

// 文件格式：首字节为 State，其后为 InvokeResult 的 protobuf 编码。
func (l FileLedger) Get(id string) (*Record, error) {
	p, err := l.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("corrupt ledger record " + id)
	}
	r := &Record{State: State(b[0])}
	if r.State == StateDone {
		r.Result = &v1.InvokeResult{}
		if err := proto.Unmarshal(b[1:], r.Result); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (l FileLedger) Put(id string, r *Record) error {
	p, err := l.path(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	b := []byte{byte(r.State)}
	if r.Result != nil {
		rb, err := proto.Marshal(r.Result)
		if err != nil {
			return err
		}
		b = append(b, rb...)
	}
	f, err := os.CreateTemp(l.Dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
