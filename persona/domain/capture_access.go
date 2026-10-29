package domain

import (
	"errors"
	"iter"
)

// CaptureRecord 是原文存取返回的值；引用只在所属业务日内唯一。
type CaptureRecord struct {
	Ref     Ref
	Segment CaptureSegment
}

// CaptureAccess 属于一个 owner 的存储事务。实现必须使用同一事务连接/快照，
// 回调结束后所有方法及已取得的迭代器都返回 ErrCaptureTransactionClosed。
// 返回值不与存储共享可变引用；Scan 按引用升序、跳过已擦除片段，提前退出须释放读取资源。
// 读取错误必须返回，不能用 found=false 或空迭代代替。调用方不并发使用同一事务，
// 不在迭代中写入或重入读取；实现须保留事务取消错误，Scan 消费结束前仍属于该事务。
type CaptureAccess interface {
	// Lookup 的 found 只表示尚存原文；不存在与已擦除都为 false，故障另由 error 表达。
	Lookup(date string, ref Ref) (CaptureSegment, bool, error)
	// Adjacent 返回严格相邻的尚存片段；before 指定向前。ref 为零时取最后/第一段。
	Adjacent(date string, ref Ref, before bool) (CaptureRecord, bool, error)
	// Scan 是闭区间，零端点表示该方向无界；只有消费时才读取，内存不随全天正文增长。
	Scan(date string, from, to Ref) iter.Seq2[CaptureRecord, error]
	// Batch 包含擦除后的空位，供领域判断同 capture_id 的重交；批次不存在必须返回错误。
	Batch(date string, number uint64) ([]CaptureSegment, error)
	Append(date string, number uint64, segments []CaptureSegment) error
	// Erase 清掉整个片段（含说话人/时间），保留批次与段号；零端点表示无界。
	Erase(date string, from, to Ref) error
}

var ErrCaptureTransactionClosed = errors.New("capture access is outside its transaction")

// SetCaptureAccess 仅由存储在回调前绑定，或由测试包装以注入读取故障。
// Clone 与 JSON 故意不继承它，避免账本快照带走事务连接或原文。
func (u *Book) SetCaptureAccess(access CaptureAccess) {
	u.captures = access
}

func (u *Book) CaptureAccess() CaptureAccess {
	return u.captures
}

func (u *Book) captureAccess() (CaptureAccess, error) {
	if u.captures == nil {
		return nil, ErrCaptureTransactionClosed
	}
	return u.captures, nil
}

func (u *Book) captureSegments(date string, from, to Ref) iter.Seq2[captureSegment, error] {
	return func(yield func(captureSegment, error) bool) {
		access, err := u.captureAccess()
		if err != nil {
			yield(captureSegment{}, err)
			return
		}
		for record, err := range access.Scan(date, from, to) {
			if !yield(captureSegment{ref: record.Ref, g: record.Segment}, err) || err != nil {
				return
			}
		}
	}
}

func (u *Book) containsRef(date string, ref Ref) (bool, error) {
	access, err := u.captureAccess()
	if err != nil {
		return false, err
	}
	_, found, err := access.Lookup(date, ref)
	return found, err
}
