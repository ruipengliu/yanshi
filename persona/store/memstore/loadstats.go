package memstore

import (
	"context"
	"encoding/json"

	"persona/domain"
	"persona/store"
)

// 与 Book.Clone 使用同一 JSON 往返；计数复用已需分配的字节，不额外序列化影响延迟。
// 只计事务入口的装载，提交隔离所需的第二次 Clone 属于写入成本。
func cloneForTransaction(ctx context.Context, book *domain.Book) (*domain.Book, error) {
	if store.LoadStatsFrom(ctx) == nil {
		return book.Clone()
	}
	data, err := json.Marshal(book)
	if err != nil {
		return nil, err
	}
	store.RecordLoad(ctx, len(data), 0)
	var out domain.Book
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
