package nodesdk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

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

func (l *MemLedger) Forget(sessionID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, r := range l.m {
		if r.SessionID == sessionID {
			delete(l.m, id)
		}
	}
	return nil
}

func (l *MemLedger) Prune(before time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, r := range l.m {
		if r.UpdatedAt.Before(before) {
			delete(l.m, id)
		}
	}
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

// 文件格式 v2：魔数 "Y2"、State（1 字节）、UpdatedAt（Unix 纳秒，8 字节大端）、
// SessionID 长度（2 字节大端）与内容，其余为 InvokeResult 的 protobuf 编码。
// v1（早期版本写下的文件）：首字节为 State，其余为 InvokeResult；读取时兼容，更新时间取文件修改时间。
var fileMagic = []byte("Y2")

func decodeRecord(id string, b []byte, modTime time.Time) (*Record, error) {
	if len(b) == 0 {
		return nil, errors.New("corrupt ledger record " + id)
	}
	r := &Record{UpdatedAt: modTime}
	if bytes.HasPrefix(b, fileMagic) {
		if len(b) < 13 {
			return nil, errors.New("corrupt ledger record " + id)
		}
		r.State = State(b[2])
		r.UpdatedAt = time.Unix(0, int64(binary.BigEndian.Uint64(b[3:11])))
		n := int(binary.BigEndian.Uint16(b[11:13]))
		if len(b) < 13+n {
			return nil, errors.New("corrupt ledger record " + id)
		}
		r.SessionID = string(b[13 : 13+n])
		b = b[13+n:]
	} else {
		r.State = State(b[0])
		b = b[1:]
	}
	if r.State == StateDone {
		r.Result = &v1.InvokeResult{}
		if err := proto.Unmarshal(b, r.Result); err != nil {
			return nil, err
		}
	}
	return r, nil
}

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
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	return decodeRecord(id, b, fi.ModTime())
}

// each 遍历全部记录；读取失败的文件跳过（由 Prune 按修改时间兜底清理）。
func (l FileLedger) each(fn func(path string, r *Record) error) error {
	entries, err := os.ReadDir(l.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !safeID.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(l.Dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		r, err := decodeRecord(e.Name(), b, fi.ModTime())
		if err != nil {
			r = &Record{UpdatedAt: fi.ModTime()}
		}
		if err := fn(p, r); err != nil {
			return err
		}
	}
	return nil
}

func (l FileLedger) Forget(sessionID string) error {
	return l.each(func(p string, r *Record) error {
		if r.SessionID == sessionID {
			return removeIfExists(p)
		}
		return nil
	})
}

func (l FileLedger) Prune(before time.Time) error {
	return l.each(func(p string, r *Record) error {
		if r.UpdatedAt.Before(before) {
			return removeIfExists(p)
		}
		return nil
	})
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l FileLedger) Put(id string, r *Record) error {
	p, err := l.path(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	b := append(slices.Clone(fileMagic), byte(r.State))
	b = binary.BigEndian.AppendUint64(b, uint64(r.UpdatedAt.UnixNano()))
	b = binary.BigEndian.AppendUint16(b, uint16(len(r.SessionID)))
	b = append(b, r.SessionID...)
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

// HasSession 报告是否仍有属于该 Session 的记录（测试与模拟用于检查删除是否彻底）。
func (l *MemLedger) HasSession(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.m {
		if r.SessionID == sessionID {
			return true
		}
	}
	return false
}
