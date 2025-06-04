package kafka

import (
	"context"
	"errors"
	"fmt"
	"strings" // 仍然需要 strings
	"time"

	"github.com/Xushengqwer/go-common/core"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Xushengqwer/go-common/models/kafkaevents"

	// 2. 导入项目内的包
	"github.com/Xushengqwer/post_audit/internal/auditplatform" // 通用审核平台接口和模型
	"github.com/Xushengqwer/post_audit/internal/constants"

	"github.com/alibabacloud-go/tea/tea" // 用于可能的SDK错误检查
)

// ProcessedAuditResult 结构体更新以使用 kafkaevents
type ProcessedAuditResult struct {
	OriginalDataID string
	ApprovedEvent  *kafkaevents.PostApprovedEvent // <--- 使用 kafkaevents 类型
	RejectedEvent  *kafkaevents.PostRejectedEvent // <--- 使用 kafkaevents 类型
	Error          error
}

// PostAuditor 接口定义更新以使用 kafkaevents.PostData
type PostAuditor interface {
	ProcessBatch(ctx context.Context, posts []kafkaevents.PostData) ([]ProcessedAuditResult, error) // <--- 输入类型改为 []kafkaevents.PostData
}

type auditProcessorService struct {
	logger    *core.ZapLogger
	moderator auditplatform.ContentReviewer
	producer  EventProducer // 假设 EventProducer 接口签名会更新以匹配 kafkaevents
}

func NewAuditProcessorService(
	logger *core.ZapLogger,
	moderator auditplatform.ContentReviewer,
	producer EventProducer, // 假设 EventProducer 接口签名会更新
) PostAuditor {
	return &auditProcessorService{
		logger:    logger,
		moderator: moderator,
		producer:  producer,
	}
}

func (s *auditProcessorService) ProcessBatch(ctx context.Context, posts []kafkaevents.PostData) ([]ProcessedAuditResult, error) {
	if len(posts) == 0 {
		s.logger.Info("ProcessBatch: 收到空的帖子批次，无需处理")
		return []ProcessedAuditResult{}, nil
	}

	s.logger.Info("ProcessBatch: 开始处理帖子批次",
		zap.Int("批次大小(batch_size)", len(posts)),
		zap.String("审核平台(moderation_platform)", s.moderator.GetPlatformName()),
	)

	tasksToModerate := make([]auditplatform.TaskData, len(posts))
	for i, post := range posts { // post 现在是 kafkaevents.PostData
		contentToReview := post.Title + "\n" + post.Content
		tasksToModerate[i] = auditplatform.TaskData{
			ID:      fmt.Sprintf("%d", post.ID), // 使用 post.ID
			Content: contentToReview,
			// ContentType: "text", // 可选
			// UserID: post.AuthorID, // 可选
		}
	}

	var moderationResults []auditplatform.ReviewResult
	var errCallModerator error
	var attemptCount int

	// API 调用和重试逻辑保持不变
	for attemptCount = 1; attemptCount <= constants.AliyunAPIMaxRetriesForThrottling; attemptCount++ {
		if attemptCount > 1 {
			delay := constants.AliyunAPIBaseDelayForThrottling * time.Duration(1<<(attemptCount-2))
			s.logger.Info("ProcessBatch: 因审核平台API错误/限流，执行退避等待后重试",
				zap.String("审核平台", s.moderator.GetPlatformName()),
				zap.Int("当前API调用尝试次数", attemptCount),
				zap.Duration("退避时间", delay),
				zap.Error(errCallModerator),
			)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				s.logger.Info("ProcessBatch: Context 在审核平台API调用退避重试期间被取消", zap.Error(ctx.Err()))
				processedResultsForError := make([]ProcessedAuditResult, len(posts))
				for i, post_item := range posts { // post_item 是 kafkaevents.PostData
					processedResultsForError[i] = ProcessedAuditResult{
						OriginalDataID: fmt.Sprintf("%d", post_item.ID),
						Error:          fmt.Errorf("处理批次时上下文取消 (因审核平台API调用重试中): %w", ctx.Err()),
					}
				}
				return processedResultsForError, fmt.Errorf("处理批次时上下文取消 (因审核平台API调用重试中): %w", ctx.Err())
			}
		}

		s.logger.Info("ProcessBatch: 尝试调用审核平台API",
			zap.String("审核平台", s.moderator.GetPlatformName()),
			zap.Int("尝试次数", attemptCount),
		)
		moderationResults, errCallModerator = s.moderator.ReviewTextBatch(ctx, tasksToModerate)

		if errCallModerator == nil {
			s.logger.Info("ProcessBatch: 调用审核平台 ReviewTextBatch 成功",
				zap.String("审核平台", s.moderator.GetPlatformName()),
				zap.Int("API调用尝试次数", attemptCount),
			)
			break
		}

		var sdkErr *tea.SDKError
		if (errors.As(errCallModerator, &sdkErr) && strings.Contains(strings.ToLower(tea.StringValue(sdkErr.Code)), "throttling")) ||
			(strings.Contains(strings.ToLower(errCallModerator.Error()), "throttling")) ||
			(strings.Contains(strings.ToLower(errCallModerator.Error()), "限流")) {
			s.logger.Warn("ProcessBatch: 遭遇审核平台限流",
				zap.String("审核平台", s.moderator.GetPlatformName()),
				zap.Int("API调用尝试次数", attemptCount),
				zap.Error(errCallModerator),
			)
			if attemptCount < constants.AliyunAPIMaxRetriesForThrottling {
				continue
			} else {
				s.logger.Error("ProcessBatch: 达到最大重试次数后，审核平台API仍处于限流状态，放弃当前批次处理",
					zap.String("审核平台", s.moderator.GetPlatformName()),
					zap.Error(errCallModerator))
				break // 结束重试
			}
		} else {
			s.logger.Error("ProcessBatch: 调用审核平台 ReviewTextBatch 失败 (非限流错误，不进行内部重试)",
				zap.String("审核平台", s.moderator.GetPlatformName()),
				zap.Error(errCallModerator),
				zap.Int("API调用尝试次数", attemptCount))
			break // 结束重试
		}
	}

	if errCallModerator != nil {
		s.logger.Error("ProcessBatch: 调用审核平台 ReviewTextBatch 最终失败",
			zap.String("审核平台", s.moderator.GetPlatformName()),
			zap.Error(errCallModerator),
			zap.Int("总API调用尝试次数", attemptCount),
		)
		processedResults := make([]ProcessedAuditResult, len(posts))
		for i, post_item := range posts { // post_item 是 kafkaevents.PostData
			processedResults[i] = ProcessedAuditResult{
				OriginalDataID: fmt.Sprintf("%d", post_item.ID),
				Error:          fmt.Errorf("审核平台API最终失败: %w", errCallModerator),
			}
		}
		return processedResults, fmt.Errorf("审核平台API最终失败: %w", errCallModerator)
	}

	if len(moderationResults) != len(tasksToModerate) {
		errMsg := "ProcessBatch: 审核平台返回的审核结果数量与发送的任务数量不匹配"
		s.logger.Error(errMsg,
			zap.String("审核平台", s.moderator.GetPlatformName()),
			zap.Int("发送数量(sent_count)", len(tasksToModerate)),
			zap.Int("接收数量(received_count)", len(moderationResults)),
		)
		processedResults := make([]ProcessedAuditResult, len(posts))
		for i, post_item := range posts { // post_item 是 kafkaevents.PostData
			processedResults[i] = ProcessedAuditResult{
				OriginalDataID: fmt.Sprintf("%d", post_item.ID),
				Error:          errors.New(errMsg),
			}
		}
		return processedResults, errors.New(errMsg)
	}

	finalProcessedResults := make([]ProcessedAuditResult, len(posts))
	for i, modResult := range moderationResults {
		originalPost := posts[i] // originalPost 现在是 kafkaevents.PostData
		currentProcessedResult := ProcessedAuditResult{
			OriginalDataID: modResult.OriginalTaskID,
		}

		if modResult.OriginalTaskID != fmt.Sprintf("%d", originalPost.ID) {
			s.logger.Error("ProcessBatch: 审核平台结果的OriginalTaskID与原始帖子ID不匹配，跳过此结果",
				zap.String("期望OriginalTaskID", fmt.Sprintf("%d", originalPost.ID)),
				zap.String("收到OriginalTaskID", modResult.OriginalTaskID),
				zap.Int("索引(index)", i),
			)
			currentProcessedResult.Error = fmt.Errorf("审核结果OriginalTaskID不匹配: 期望 %d, 收到 %s", originalPost.ID, modResult.OriginalTaskID)
			finalProcessedResults[i] = currentProcessedResult
			continue
		}

		if modResult.Error != nil {
			s.logger.Error("ProcessBatch: 审核平台处理单个任务时返回错误",
				zap.String("审核平台", s.moderator.GetPlatformName()),
				zap.String("帖子ID(post_id_str)", modResult.OriginalTaskID),
				zap.Error(modResult.Error),
			)
			currentProcessedResult.Error = modResult.Error
			finalProcessedResults[i] = currentProcessedResult
			continue
		}

		eventID := uuid.NewString()
		eventTimestamp := time.Now() // 使用当前时间作为事件时间戳

		if modResult.Suggestion == "pass" {
			// 创建 kafkaevents.PostApprovedEvent
			approvedEvent := &kafkaevents.PostApprovedEvent{
				EventID:   eventID,
				Timestamp: eventTimestamp,
				Post:      originalPost, // originalPost 已经是 kafkaevents.PostData 类型
			}
			var sendErr error
		SendApprovedLoop:
			for attempt := 1; attempt <= constants.KafkaProducerMaxSendRetries; attempt++ {
				sendErr = s.producer.SendApprovedEvent(ctx, approvedEvent) // 假设 producer 接口已更新
				if sendErr == nil {
					s.logger.Info("ProcessBatch: 帖子审核通过，并已成功发送结果事件",
						zap.String("帖子ID(post_id_str)", modResult.OriginalTaskID),
						zap.String("事件ID(event_id)", approvedEvent.EventID),
						zap.Int("发送尝试次数(attempt)", attempt),
					)
					currentProcessedResult.ApprovedEvent = approvedEvent
					break SendApprovedLoop
				}
				s.logger.Warn("ProcessBatch: 发送 PostApprovedEvent 到 Kafka 失败，准备重试",
					zap.String("事件ID(event_id)", approvedEvent.EventID),
					zap.String("帖子ID(post_id_str)", modResult.OriginalTaskID),
					zap.Int("尝试次数(attempt)", attempt),
					zap.Error(sendErr),
				)
				if attempt < constants.KafkaProducerMaxSendRetries {
					select {
					case <-time.After(constants.KafkaProducerSendRetryDelay):
					case <-ctx.Done():
						sendErr = fmt.Errorf("发送审核通过事件到Kafka时上下文取消: %w", ctx.Err())
						break SendApprovedLoop
					}
				}
			}
			if sendErr != nil {
				currentProcessedResult.Error = fmt.Errorf("多次尝试后发送审核通过事件到Kafka失败: %w", sendErr)
			}
		} else { // "block" or "review"
			// 将 auditplatform.RejectionDetail 转换为 kafkaevents.RejectionDetail
			commonDetails := make([]kafkaevents.RejectionDetail, len(modResult.Details))
			for j, detail := range modResult.Details { // detail 是 auditplatform.RejectionDetail
				commonDetails[j] = kafkaevents.RejectionDetail{ // 手动映射字段
					Label:          detail.Label,
					Suggestion:     detail.Suggestion,
					Score:          detail.Score,
					MatchedContent: detail.MatchedContent,
				}
			}

			// 创建 kafkaevents.PostRejectedEvent
			rejectedEvent := &kafkaevents.PostRejectedEvent{
				EventID:    eventID,
				Timestamp:  eventTimestamp,
				PostID:     originalPost.ID,
				Suggestion: modResult.Suggestion,
				Details:    commonDetails, // 使用转换后的 Details
			}
			var sendErr error
		SendRejectedLoop:
			for attempt := 1; attempt <= constants.KafkaProducerMaxSendRetries; attempt++ {
				sendErr = s.producer.SendRejectedEvent(ctx, rejectedEvent) // 假设 producer 接口已更新
				if sendErr == nil {
					s.logger.Info("ProcessBatch: 帖子审核未通过/需复审，并已成功发送结果事件",
						zap.String("帖子ID(post_id_str)", modResult.OriginalTaskID),
						zap.String("事件ID(event_id)", rejectedEvent.EventID),
						zap.Int("发送尝试次数(attempt)", attempt),
					)
					currentProcessedResult.RejectedEvent = rejectedEvent
					break SendRejectedLoop
				}
				s.logger.Warn("ProcessBatch: 发送 PostRejectedEvent 到 Kafka 失败，准备重试",
					zap.String("事件ID(event_id)", rejectedEvent.EventID),
					zap.String("帖子ID(post_id_str)", modResult.OriginalTaskID),
					zap.Int("尝试次数(attempt)", attempt),
					zap.Error(sendErr),
				)
				if attempt < constants.KafkaProducerMaxSendRetries {
					select {
					case <-time.After(constants.KafkaProducerSendRetryDelay):
					case <-ctx.Done():
						sendErr = fmt.Errorf("发送审核拒绝事件到Kafka时上下文取消: %w", ctx.Err())
						break SendRejectedLoop
					}
				}
			}
			if sendErr != nil {
				currentProcessedResult.Error = fmt.Errorf("多次尝试后发送审核拒绝事件到Kafka失败: %w", sendErr)
			}
		}
		finalProcessedResults[i] = currentProcessedResult
	}
	return finalProcessedResults, nil
}
