package auditplatform

import (
	"context"
)

// ErrReviewThrottled 表示审核请求因为平台限流而被拒绝。
// 具体的实现可以返回此错误类型，以便上层进行特定的重试逻辑。
// var ErrReviewThrottled = errors.New("content review request was throttled by the platform")

// ContentReviewer 是内容审核平台的核心接口。
// 任何具体的审核平台（如阿里云、腾讯云等）都应该实现此接口。
type ContentReviewer interface {
	// ReviewTextBatch 批量审核文本内容。
	//
	// ctx: 用于控制请求的上下文，例如超时或取消。
	// tasks: 一个包含多个审核任务 (TaskData) 的切片。
	//
	// 返回值:
	//   - []ReviewResult: 一个包含每个任务审核结果 (ReviewResult) 的切片。
	//     此切片的顺序必须与输入 tasks 切片的顺序严格对应。
	//     即使某个任务在审核平台侧处理失败，也应该在该位置有一个包含Error信息的ReviewResult。
	//   - error:
	//     - 如果整个批量请求因网络问题、认证失败、或平台整体错误而失败，则返回一个非nil的error。
	//     - 如果是平台限流导致的失败，建议返回一个可识别的错误类型（例如上面注释掉的 ErrReviewThrottled，或者通过 errors.Is 判断）。
	//     - 如果批量请求本身成功，但部分或全部任务在平台侧有各自的问题，则顶层 error 应为 nil，
	//       具体的任务级错误应填充在各自的 ReviewResult.Error 字段中。
	ReviewTextBatch(ctx context.Context, tasks []TaskData) ([]ReviewResult, error)

	// GetPlatformName 返回当前审核平台的名称。
	// 这对于日志记录、监控或在多平台策略中区分来源可能很有用。
	GetPlatformName() string
}
