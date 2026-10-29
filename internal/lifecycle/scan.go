package lifecycle

import "context"

type scanBoundary struct{}

// WithScanAfter 仅用于分片离线扫描；空边界保持原单库查询顺序与结果。
func WithScanAfter(ctx context.Context, after string) context.Context {
	return context.WithValue(ctx, scanBoundary{}, after)
}
func ScanAfter(ctx context.Context) string {
	value, _ := ctx.Value(scanBoundary{}).(string)
	return value
}
