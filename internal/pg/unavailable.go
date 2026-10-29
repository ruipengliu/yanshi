package pg

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// IsUnavailable 只识别连接故障与时限；约束、事务和清理回调的错误仍由调用方处理。
func IsUnavailable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, puddle.ErrClosedPool) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var connection *pgconn.ConnectError
	var network net.Error
	if errors.As(err, &connection) || errors.As(err, &network) || pgconn.Timeout(err) {
		return true
	}
	var database *pgconn.PgError
	if errors.As(err, &database) {
		return strings.HasPrefix(database.Code, "08") || database.Code == "57P01" ||
			database.Code == "57P02" || database.Code == "57P03"
	}
	return false
}
