package task

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/http"
	"time"

	"xinli/kernel"
)

type Kernel interface {
	SubmitInput(context.Context, string, string, string) (kernel.InputReceipt, error)
	FindInput(context.Context, string, string) (kernel.InputReceipt, bool, error)
}

var (
	ErrObsolete      = errors.New("task: source or target deleted")
	ErrConfiguration = errors.New("task: configuration")
)

func IsConfiguration(err error) bool {
	if errors.Is(err, ErrConfiguration) {
		return true
	}
	var status *kernel.HTTPError
	return errors.As(err, &status) && status.StatusCode >= 300 && status.StatusCode < 500 &&
		status.StatusCode != http.StatusNotFound && status.StatusCode != http.StatusRequestTimeout &&
		status.StatusCode != http.StatusTooManyRequests && status.StatusCode != http.StatusConflict
}

// Prepare 在事务外读取当前绑定；A9 的 CAS 创建可直接接在这里。
// 返回的文字只允许由 Spec 渲染固定命令，不把正文存进任务或错误字段。
type Prepare func(context.Context, Task) (sessionID, text string, err error)

type Executor struct {
	Store     Store
	Kernel    Kernel
	Prepare   Prepare
	Now       func() time.Time
	Lease     time.Duration
	RetryBase time.Duration
	RetryMax  time.Duration
}

func (w *Executor) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Backoff 的抖动只由任务身份决定；重启或双执行者不会重新抽样缩短退避。
func Backoff(id string, attempt int, base, ceiling time.Duration) time.Duration {
	if base <= 0 {
		base = 30 * time.Second
	}
	if ceiling < base {
		ceiling = max(base, 10*time.Minute)
	}
	delay := base
	for i := 1; i < attempt && delay < ceiling; i++ {
		if delay > ceiling/2 {
			delay = ceiling
		} else {
			delay *= 2
		}
	}
	h := sha256.Sum256([]byte(id))
	// [75%,100%]，抖动后仍不超过上限；避免大 duration 的乘法溢出。
	fraction := binary.BigEndian.Uint64(h[:8]) % 251
	return max(time.Nanosecond, delay/1000*time.Duration(750+fraction))
}

func (w *Executor) deferTask(ctx context.Context, t Task, class string, cause error) (Task, error) {
	t.LastErrorClass = class
	t.RetryAt = w.now().Add(Backoff(t.TaskID, t.Attempt, w.RetryBase, w.RetryMax))
	err := w.Store.Save(ctx, t, w.now(), true)
	return t, errors.Join(cause, err)
}

func (w *Executor) failure(ctx context.Context, t Task, cause error) (Task, error) {
	class := "retryable"
	switch {
	case errors.Is(cause, kernel.ErrNotFound), errors.Is(cause, ErrObsolete):
		t.State = Terminated
		class = "target_deleted"
	case IsConfiguration(cause):
		t.State = Failed
		class = "configuration"
	}
	return w.deferTask(ctx, t, class, cause)
}

func (w *Executor) accepted(ctx context.Context, t Task, receipt kernel.InputReceipt) (Task, error) {
	if receipt.InputID != t.InputID || receipt.RunID == "" {
		return w.deferTask(ctx, t, "receipt_invalid", errors.New("task: invalid receipt"))
	}
	if t.Receipt.RunID != "" && t.Receipt.RunID != receipt.RunID {
		return w.deferTask(ctx, t, "receipt_invalid", errors.New("task: conflicting receipt"))
	}
	t.Receipt = receipt
	t.State = Accepted
	class := ""
	if receipt.Terminal == "completed" {
		t.State = Succeeded
	}
	if receipt.Terminal == "failed" || receipt.Terminal == "interrupted" {
		t.State = Failed
		class = "run_" + receipt.Terminal
	}
	return w.deferTask(ctx, t, class, nil)
}

// Run 可被通知及周期扫描同时调用；只有认领成功者调用内核。
func (w *Executor) Run(ctx context.Context, id string) (Task, error) {
	lease := w.Lease
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	t, claimed, err := w.Store.Claim(ctx, id, w.now(), lease)
	if err != nil {
		return t, err
	}
	if !claimed {
		return w.Store.Get(ctx, id)
	}
	if w.Kernel == nil || w.Prepare == nil {
		return w.failure(ctx, t, ErrConfiguration)
	}
	// 给网络调用留在租约内的预算；超时不能证明远端已停止，接管者仍须先对账。
	callCtx, cancel := context.WithTimeout(ctx, max(time.Nanosecond, t.LeaseUntil.Sub(w.now())))
	defer cancel()
	if t.State == Unknown || t.State == Accepted {
		receipt, found, readErr := w.Kernel.FindInput(callCtx, t.Session, t.InputID)
		if errors.Is(readErr, kernel.ErrNotFound) {
			return w.failure(ctx, t, readErr)
		}
		if readErr != nil {
			return w.deferTask(ctx, t, "reconcile", readErr)
		}
		if found {
			return w.accepted(ctx, t, receipt)
		}
		// 曾收到明确接受回执却在日志中找不到：不能推翻已知事实后再次提交。
		if t.State == Accepted {
			return w.deferTask(ctx, t, "receipt_missing", errors.New("task: accepted input missing"))
		}
	}
	sid, text, err := w.Prepare(callCtx, t)
	if err != nil {
		return w.failure(ctx, t, err)
	}
	if sid == "" || (t.Session != "" && t.Session != sid) {
		return w.failure(ctx, t, ErrConfiguration)
	}
	before := t
	t.Session = sid
	t.State = Unknown
	if err := w.Store.Save(ctx, t, w.now(), false); err != nil {
		if errors.Is(err, ErrTargetBusy) {
			return w.deferTask(ctx, before, "target_busy", nil)
		}
		return t, err
	}
	receipt, err := w.Kernel.SubmitInput(callCtx, sid, t.InputID, text)
	if err != nil {
		return w.failure(ctx, t, err)
	}
	return w.accepted(ctx, t, receipt)
}

// Scan 每轮有界；单项失败不阻断其他 owner，退避与未知状态均由存储恢复。
func (w *Executor) Scan(ctx context.Context, limit int) error {
	ids, err := w.Store.Due(ctx, w.now(), limit)
	if err != nil {
		return err
	}
	var result error
	for _, id := range ids {
		_, err := w.Run(ctx, id)
		result = errors.Join(result, err)
	}
	return result
}
