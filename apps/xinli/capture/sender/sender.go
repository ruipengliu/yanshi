// Package sender 提供开发 HostApp 的单写者持久发送端；文件只保存文字，不接收音频。
package sender

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"persona/domain"
	"xinli/capture"
)

var ErrGap = errors.New("capture staged; missing sequence must be supplied")
var ErrStopped = errors.New("capture sender stopped")

type Transport interface {
	Send(context.Context, capture.Batch) (capture.Receipt, error)
}

type Identity struct {
	BusinessLine string `json:"business_line"`
	EndUser      string `json:"end_user"`
	DeviceID     string `json:"device_id"`
	SessionID    string `json:"session_id"`
}

type Entry struct {
	Batch   capture.Batch   `json:"batch"`
	Status  string          `json:"status"`
	Receipt capture.Receipt `json:"receipt"`
}

type State struct {
	Version  int      `json:"version"`
	Identity Identity `json:"identity"`
	Next     uint64   `json:"next"`
	Stopped  string   `json:"stopped,omitempty"`
	Entries  []Entry  `json:"entries"`
}

type Sender struct {
	mu       sync.Mutex
	path     string
	lock     *os.File
	state    State
	poisoned bool
}

// Open 的文件锁在进程退出时释放；不能用残留 lock 文件判断另一个进程仍活着。
func Open(path string, id Identity) (*Sender, error) {
	if id.BusinessLine == "" || id.EndUser == "" || id.DeviceID == "" || id.SessionID == "" {
		return nil, errors.New("sender identity required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("sender already open")
	}
	s := &Sender{path: path, lock: lock, state: State{Version: 1, Identity: id, Next: 1}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		err = s.persist(s.state)
	case err == nil:
		err = json.Unmarshal(raw, &s.state)
		if err == nil && (s.state.Version != 1 || s.state.Identity != id || s.state.Next != uint64(len(s.state.Entries))+1) {
			err = errors.New("sender state or identity mismatch")
		}
		if err == nil {
			for _, entry := range s.state.Entries {
				if domain.ValidateLegacyCaptureSegments(entry.Batch.Ordered().Segments) != nil {
					err = domain.ErrCaptureInvalid
					break
				}
			}
		}
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Sender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func (s *Sender) snapshot() State {
	out := s.state
	out.Entries = slices.Clone(out.Entries)
	for i := range out.Entries {
		out.Entries[i].Batch.Segments = capture.CloneSegments(out.Entries[i].Batch.Segments)
		out.Entries[i].Receipt.Refs = slices.Clone(out.Entries[i].Receipt.Refs)
	}
	return out
}

func (s *Sender) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot()
}

// Enqueue 在落盘后才交出 ID/序号；重试发送不走此入口，不重新分配身份或业务日。
func (s *Sender) Enqueue(day, zone string, generation uint64, segments []capture.Segment) (capture.Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil || s.poisoned || s.state.Stopped != "" {
		return capture.Batch{}, ErrStopped
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return capture.Batch{}, err
	}
	b := capture.Batch{SessionID: s.state.Identity.SessionID, Seq: s.state.Next, CaptureID: hex.EncodeToString(id[:]), BusinessDay: day, TimeZone: zone, Generation: generation, Segments: capture.CloneSegments(segments)}
	ordered := b.Ordered()
	if err := domain.ValidateLegacyCaptureSegments(ordered.Segments); err != nil {
		return capture.Batch{}, domain.ErrCaptureInvalid
	}
	normalized, err := domain.NormalizeOrderedCapture(ordered)
	if err != nil {
		return capture.Batch{}, err
	}
	b.BusinessDay = normalized.BusinessDay
	next := s.snapshot()
	next.Next++
	next.Entries = append(next.Entries, Entry{Batch: b, Status: "pending"})
	if err := s.persist(next); err != nil {
		return capture.Batch{}, err
	}
	b.Segments = capture.CloneSegments(b.Segments)
	return b, nil
}

// Flush 只确认正式接受；断网和丢响应保持原请求，202 保留待确认且报告缺口。
// 世代失效停止整条采集会话，避免跨午夜后半段绕过同一序列的删除屏障。
func (s *Sender) Flush(ctx context.Context, transport Transport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil || s.poisoned {
		return ErrStopped
	}
	if s.state.Stopped == "resync_required" {
		return capture.ErrResync
	}
	if s.state.Stopped != "" {
		return ErrStopped
	}
	for i := range s.state.Entries {
		entry := s.state.Entries[i]
		if entry.Status == "accepted" {
			continue
		}
		entry.Batch.Segments = capture.CloneSegments(entry.Batch.Segments)
		receipt, err := transport.Send(ctx, entry.Batch)
		if errors.Is(err, capture.ErrResync) || errors.Is(err, capture.ErrRejected) {
			next := s.snapshot()
			next.Stopped = "rejected"
			if errors.Is(err, capture.ErrResync) {
				next.Stopped = "resync_required"
			}
			for j := range next.Entries {
				if next.Entries[j].Status != "accepted" {
					next.Entries[j].Status = next.Stopped
				}
			}
			if saveErr := s.persist(next); saveErr != nil {
				return saveErr
			}
			return err
		}
		if err != nil {
			return err
		}
		if receipt.BusinessDay != entry.Batch.BusinessDay || receipt.Generation != entry.Batch.Generation {
			return errors.New("capture receipt identity mismatch")
		}
		next := s.snapshot()
		receipt.Refs = slices.Clone(receipt.Refs)
		next.Entries[i].Receipt = receipt
		switch receipt.Status {
		case "staged":
			if receipt.NextSeq == 0 || receipt.NextSeq > entry.Batch.Seq || len(receipt.Refs) != 0 {
				return errors.New("invalid staged receipt")
			}
			next.Entries[i].Status = "staged"
			if err := s.persist(next); err != nil {
				return err
			}
			return ErrGap
		case "accepted":
			if receipt.NextSeq <= entry.Batch.Seq || len(receipt.Refs) != len(entry.Batch.Segments) {
				return errors.New("invalid accepted receipt")
			}
			next.Entries[i].Status = "accepted"
			// 已确认正文不再为重试所需，避免开发发送端无限保留原文。
			next.Entries[i].Batch.Segments = nil
			if err := s.persist(next); err != nil {
				return err
			}
		default:
			return errors.New("invalid capture receipt")
		}
	}
	return nil
}

func (s *Sender) persist(next State) error {
	err := writeState(s.path, next)
	if err != nil {
		s.poisoned = true
		return err
	}
	s.state = next
	return nil
}

func writeState(path string, state State) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".capture-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return fmt.Errorf("sync sender directory: %w", err)
	}
	return nil
}
