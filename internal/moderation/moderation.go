// Package moderation 对用户输入与模型输出做内容安全检查（docs/design/m4-moderation.md，ADR-0021）。
//
// 检查点在写日志之前：违规内容不进入日志、不进入模型上下文、不展示给用户。提供商可替换；
// 本阶段只有模拟实现 Mock，上线前须接入真实提供商。
package moderation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"yanshi/internal/metrics"
)

type Stage string

const (
	Input  Stage = "input"
	Output Stage = "output"
)

// Image 是输入中的图片工件；提供商需要时自行读取内容。
type Image struct {
	ArtifactID string
	MimeType   string
}

type Request struct {
	Stage        Stage
	BusinessLine string
	// Text 是要检查的文本：输入时为用户输入，输出时为回复文本与调用参数。
	Text   string
	Images []Image
}

// Verdict 是检查结果。Labels 是提供商给出的违规类别，用于统计与审计，不含原文。
type Verdict struct {
	Block  bool
	Labels []string
}

// Moderator 是内容安全提供商。返回错误表示无法判断（服务不可用等）：调用方一律不放行（ADR-0021）。
type Moderator interface {
	Check(ctx context.Context, req Request) (Verdict, error)
}

// Refusal 是模型输出被拦截时代替它写入日志、展示给用户的文本。
const Refusal = "抱歉，这个问题我无法回答。"

var (
	// ErrRejected 表示输入违规（HTTP 422）。
	ErrRejected = errors.New("content rejected by moderation")
	// ErrUnavailable 表示内容安全服务不可用（输入时 HTTP 503；输出时 Step 出错重试）。
	ErrUnavailable = errors.New("moderation unavailable")
)

// Check 调用提供商并记录指标；m 为 nil 时不检查（放行）。提供商出错时返回包装了 ErrUnavailable 的错误。
func Check(ctx context.Context, m Moderator, req Request) (Verdict, error) {
	if m == nil {
		return Verdict{}, nil
	}
	start := time.Now()
	v, err := m.Check(ctx, req)
	metrics.ModerationDuration.WithLabelValues(string(req.Stage)).Observe(metrics.Since(start))
	switch {
	case err != nil:
		metrics.ModerationChecks.WithLabelValues(string(req.Stage), "error").Inc()
		return Verdict{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	case v.Block:
		metrics.ModerationChecks.WithLabelValues(string(req.Stage), "block").Inc()
	default:
		metrics.ModerationChecks.WithLabelValues(string(req.Stage), "allow").Inc()
	}
	return v, nil
}

// MockTerms 是模拟提供商的默认关键词：只有测试用的标记，不代表任何真实的审核策略。
var MockTerms = []string{"【违规测试】", "[moderation-test]"}

// Mock 是模拟提供商：文本包含任一关键词即拦截，标签为 "mock"。它没有真实的识别能力，只用于开发与测试；
// 上线前须替换为真实提供商（docs/launch-checklist.md）。
type Mock struct {
	Terms []string
}

func (m Mock) Check(_ context.Context, req Request) (Verdict, error) {
	terms := m.Terms
	if terms == nil {
		terms = MockTerms
	}
	for _, t := range terms {
		if t != "" && strings.Contains(req.Text, t) {
			return Verdict{Block: true, Labels: []string{"mock"}}, nil
		}
	}
	return Verdict{}, nil
}

// LoadTerms 读取关键词文件：每行一个，忽略空行与 # 开头的注释。
func LoadTerms(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, nil
}
