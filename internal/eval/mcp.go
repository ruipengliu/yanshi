package eval

import (
	"context"
	"log/slog"

	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/eventlog"
	"yanshi/internal/mcpcap"
)

// 评测把两条业务线放进同一进程；生产 Connector 的名称缓存属于单个业务线单元，不能混用凭据。
type evalMCP map[string]*mcpcap.Connector

func newEvalMCP(servers []mcpcap.Server, artifacts *artifact.Service, log eventlog.Log, logger *slog.Logger) evalMCP {
	byLine := evalMCP{}
	for _, server := range servers {
		connector := byLine[server.BusinessLine]
		if connector == nil {
			connector = &mcpcap.Connector{Artifacts: artifacts, Log: log, Logger: logger}
			byLine[server.BusinessLine] = connector
		}
		connector.Servers = append(connector.Servers, server)
	}
	return byLine
}

func (m evalMCP) Tools(ctx context.Context, businessLine string, patterns []string) ([]capability.Tool, error) {
	if connector := m[businessLine]; connector != nil {
		return connector.Tools(ctx, businessLine, patterns)
	}
	return nil, nil
}

func (m evalMCP) Close() {
	for _, connector := range m {
		connector.Close()
	}
}
