// File: internal/kafka/consumer_group.go
package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/Xushengqwer/go-common/core"
	"go.uber.org/zap"

	// 1. 导入你的 common 包中的 kafkaevents
	// !!! 请确保将 "github.com/Xushengqwer/go-common" 替换为你的 go-common 模块的实际 Go 模块路径 !!!
	"github.com/Xushengqwer/go-common/models/kafkaevents"

	// 2. 导入项目内的包
	"github.com/Xushengqwer/post_audit/internal/config"
	"github.com/Xushengqwer/post_audit/internal/constants"
	// "github.com/Xushengqwer/post_audit/internal/models" // <--- 移除对旧 models 的依赖 (如果它只包含事件定义)
)

// AuditConsumerGroupHandler 实现了 sarama.ConsumerGroupHandler 接口，
// 用于处理来自 Kafka 消费者组的消息。
type AuditConsumerGroupHandler struct {
	logger      *core.ZapLogger
	ready       chan bool     // 用于通知消费者组已准备好开始消费
	postAuditor PostAuditor   // 注入的业务逻辑处理器 (其 ProcessBatch 方法已更新)
	producer    EventProducer // 注入的 Kafka 生产者 (其接口签名已更新)
}

// NewAuditConsumerGroupHandler 创建一个新的 AuditConsumerGroupHandler 实例。
func NewAuditConsumerGroupHandler(logger *core.ZapLogger, auditor PostAuditor, producer EventProducer) *AuditConsumerGroupHandler {
	return &AuditConsumerGroupHandler{
		logger:      logger,
		ready:       make(chan bool),
		postAuditor: auditor,
		producer:    producer,
	}
}

// Setup 在消费者会话开始时被调用。
func (handler *AuditConsumerGroupHandler) Setup(session sarama.ConsumerGroupSession) error {
	handler.logger.Info("Kafka 消费者组: 会话 Setup 已启动",
		zap.Any("声明的分区(claims)", session.Claims()),
		zap.String("成员ID(member_id)", session.MemberID()),
	)
	close(handler.ready) // 通知消费者已准备好
	return nil
}

// Cleanup 在消费者会话结束时被调用。
func (handler *AuditConsumerGroupHandler) Cleanup(session sarama.ConsumerGroupSession) error {
	handler.logger.Info("Kafka 消费者组: 会话 Cleanup 已启动",
		zap.String("成员ID(member_id)", session.MemberID()),
	)
	return nil
}

// ConsumeClaim 是核心的消息处理循环。
func (handler *AuditConsumerGroupHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	handler.logger.Info("Kafka 消费者组: ConsumeClaim 已启动",
		zap.String("主题(topic)", claim.Topic()),
		zap.Int32("分区(partition)", claim.Partition()),
		zap.Int64("初始偏移量(initial_offset)", claim.InitialOffset()),
	)

	// 初始化批次缓冲区
	messageBuffer := make([]*sarama.ConsumerMessage, 0, constants.KafkaConsumerBatchSize)
	// 3. postDataBuffer 现在存储 kafkaevents.PostData
	postDataBuffer := make([]kafkaevents.PostData, 0, constants.KafkaConsumerBatchSize)

	// 批次处理的定时器
	ticker := time.NewTicker(constants.KafkaConsumerBatchTimeout)
	defer ticker.Stop()

	for {
		select {
		case message, ok := <-claim.Messages():
			if !ok {
				handler.logger.Info("Kafka 消费者组: 消息通道已关闭，处理缓冲区中剩余消息并退出 ConsumeClaim。",
					zap.String("主题(topic)", claim.Topic()),
					zap.Int32("分区(partition)", claim.Partition()),
					zap.Int("剩余消息数", len(messageBuffer)),
				)
				// 处理缓冲区中剩余的消息
				if len(messageBuffer) > 0 {
					handler.processMessageBatch(session.Context(), session, messageBuffer, postDataBuffer)
				}
				return nil // channel 关闭
			}

			handler.logger.Debug("Kafka 消费者组: 收到消息",
				zap.String("主题(topic)", message.Topic),
				zap.Int32("分区(partition)", message.Partition),
				zap.Int64("偏移量(offset)", message.Offset),
			)

			// 4. 反序列化消息为 kafkaevents.PostPendingAuditEvent
			var pendingAuditEvent kafkaevents.PostPendingAuditEvent
			err := json.Unmarshal(message.Value, &pendingAuditEvent)
			if err != nil {
				handler.logger.Error("Kafka 消费者组: 反序列化消息到 PostPendingAuditEvent 失败，发送到DLQ",
					zap.Error(err),
					zap.String("主题(topic)", message.Topic),
					zap.Int64("偏移量(offset)", message.Offset),
					zap.ByteString("原始消息体(raw_value)", message.Value),
				)
				// 发送到DLQ
				dlqErr := handler.producer.SendToDLQ(session.Context(), message, "反序列化 PostPendingAuditEvent 失败: "+err.Error())
				if dlqErr != nil {
					handler.logger.Error("Kafka 消费者组: 发送消息到DLQ失败 (因反序列化错误)",
						zap.String("主题(topic)", message.Topic),
						zap.Int64("偏移量(offset)", message.Offset),
						zap.Error(dlqErr),
					)
				}
				session.MarkMessage(message, "") // 标记这条坏消息已处理
				continue                         // 继续处理下一条消息
			}

			// 5. 将提取的 PostData 添加到缓冲区
			messageBuffer = append(messageBuffer, message)
			postDataBuffer = append(postDataBuffer, pendingAuditEvent.Post) // <-- 从事件中提取 PostData

			if len(messageBuffer) >= constants.KafkaConsumerBatchSize {
				handler.logger.Info("Kafka 消费者组: 达到批次大小，准备处理消息批次", zap.Int("当前批次大小", len(messageBuffer)))
				handler.processMessageBatch(session.Context(), session, messageBuffer, postDataBuffer)
				// 重置缓冲区
				messageBuffer = make([]*sarama.ConsumerMessage, 0, constants.KafkaConsumerBatchSize)
				postDataBuffer = make([]kafkaevents.PostData, 0, constants.KafkaConsumerBatchSize)
				ticker.Reset(constants.KafkaConsumerBatchTimeout)
			}

		case <-ticker.C:
			if len(messageBuffer) > 0 {
				handler.logger.Info("Kafka 消费者组: 批次处理超时，准备处理当前缓冲的消息", zap.Int("当前批次大小", len(messageBuffer)))
				handler.processMessageBatch(session.Context(), session, messageBuffer, postDataBuffer)
				// 重置缓冲区
				messageBuffer = make([]*sarama.ConsumerMessage, 0, constants.KafkaConsumerBatchSize)
				postDataBuffer = make([]kafkaevents.PostData, 0, constants.KafkaConsumerBatchSize)
			}
		case <-session.Context().Done():
			handler.logger.Info("Kafka 消费者组: 会话上下文已完成，处理缓冲区中剩余消息并退出 ConsumeClaim。",
				zap.String("主题(topic)", claim.Topic()),
				zap.Int32("分区(partition)", claim.Partition()),
				zap.Int("剩余消息数", len(messageBuffer)),
			)
			if len(messageBuffer) > 0 {
				handler.processMessageBatch(session.Context(), session, messageBuffer, postDataBuffer)
			}
			return nil
		}
	}
}

// processMessageBatch 的 postDataBatch 参数类型更新
func (handler *AuditConsumerGroupHandler) processMessageBatch(
	ctx context.Context,
	session sarama.ConsumerGroupSession,
	messageBatch []*sarama.ConsumerMessage,
	postDataBatch []kafkaevents.PostData, // <--- 参数类型更新
) {
	if len(postDataBatch) == 0 {
		handler.logger.Debug("processMessageBatch: 帖子数据批次为空，无需处理")
		return
	}

	handler.logger.Info("Kafka 消费者组: 开始处理内部消息批次", zap.Int("批次大小", len(postDataBatch)))

	// 调用 postAuditor.ProcessBatch，它现在期望 []kafkaevents.PostData
	processedResults, batchErr := handler.postAuditor.ProcessBatch(ctx, postDataBatch)

	if batchErr != nil {
		handler.logger.Error("Kafka 消费者组: 业务逻辑 ProcessBatch 返回顶层错误，此批次消息将不被标记，等待Kafka重试",
			zap.Error(batchErr),
			zap.Int("批次中的消息数量", len(messageBatch)),
		)
		// 不标记偏移量，以便 Kafka 重新投递整个批次（或导致错误的消息）
		// 注意：这可能导致重复处理，业务逻辑需要能处理幂等性或接受重复
		return
	}

	if len(processedResults) != len(messageBatch) {
		handler.logger.Error("Kafka 消费者组: ProcessBatch 返回的结果数量与原始消息批次数量不匹配，无法安全提交偏移量",
			zap.Int("原始消息批次数量", len(messageBatch)),
			zap.Int("处理结果数量", len(processedResults)),
		)
		// 这是一个严重的不一致，可能不应该标记任何消息，或者需要更复杂的错误处理策略
		return
	}

	allSuccessfullyMarked := true
	for i, result := range processedResults {
		originalMessage := messageBatch[i]

		if result.Error != nil {
			allSuccessfullyMarked = false
			handler.logger.Error("Kafka 消费者组: 单个帖子处理失败，发送到DLQ并标记偏移量",
				zap.String("原始DataID/帖子ID", result.OriginalDataID), // OriginalDataID 仍然是 string
				zap.Error(result.Error),
				zap.String("原始主题", originalMessage.Topic),
				zap.Int64("原始偏移量", originalMessage.Offset),
			)
			dlqErr := handler.producer.SendToDLQ(ctx, originalMessage, "业务处理失败或结果发送失败: "+result.Error.Error())
			if dlqErr != nil {
				handler.logger.Error("Kafka 消费者组: 发送消息到DLQ失败 (因业务处理错误)",
					zap.String("原始DataID/帖子ID", result.OriginalDataID),
					zap.Error(dlqErr),
				)
				// 即使发送到DLQ失败，也应该尝试标记原始消息，避免无限重试
			}
			session.MarkMessage(originalMessage, "") // 标记已处理（尝试过DLQ）
		} else {
			// 成功处理（无论是 approved 还是 rejected 事件已发送）
			session.MarkMessage(originalMessage, "")
			handler.logger.Info("Kafka 消费者组: 单个帖子成功处理，标记偏移量",
				zap.String("原始DataID/帖子ID", result.OriginalDataID),
				zap.String("原始主题", originalMessage.Topic),
				zap.Int64("原始偏移量", originalMessage.Offset),
			)
		}
	}

	if allSuccessfullyMarked && len(messageBatch) > 0 {
		handler.logger.Info("Kafka 消费者组: 消息批次中所有条目均已处理（或发送到DLQ），所有偏移量已标记。",
			zap.Int("批次大小", len(messageBatch)))
	} else if len(messageBatch) > 0 {
		handler.logger.Warn("Kafka 消费者组: 消息批次中存在部分处理失败的情况。失败消息已尝试发送到DLQ并标记偏移量。",
			zap.Int("批次大小", len(messageBatch)))
	}
}

// StartConsumerGroup 函数保持不变，它负责设置和启动消费者组。
// 它依赖于 NewAuditConsumerGroupHandler 和 GetSaramaConfig 等。
func StartConsumerGroup(
	appCfg *config.AppConfig,
	zapLogger *core.ZapLogger,
	auditor PostAuditor, // PostAuditor 接口已更新
	producer EventProducer, // EventProducer 接口已更新
) error {
	kafkaCfg := appCfg.Kafka

	saramaConfig, err := GetSaramaConfig(kafkaCfg, constants.ServiceName, zapLogger)
	if err != nil {
		zapLogger.Fatal("创建 Sarama 消费者组配置失败", zap.Error(err))
		return fmt.Errorf("创建 Sarama 配置失败: %w", err)
	}
	// ... (其余配置检查和日志保持不变) ...
	if saramaConfig.Consumer.Offsets.AutoCommit.Enable {
		zapLogger.Warn("Sarama 配置指示消费者启用了自动提交。对于 ConsumerGroupHandler，推荐手动提交。")
	}
	if !saramaConfig.Version.IsAtLeast(sarama.V0_10_2_0) {
		msg := "配置中的 Kafka 版本不支持消费者组 (需要 >= 0.10.2.0)"
		zapLogger.Fatal(msg, zap.String("配置的版本(configured_version)", saramaConfig.Version.String()))
		return errors.New(msg)
	}

	handler := NewAuditConsumerGroupHandler(zapLogger, auditor, producer)

	consumerGroup, err := sarama.NewConsumerGroup(kafkaCfg.Brokers, kafkaCfg.ConsumerGroupID, saramaConfig)
	if err != nil {
		zapLogger.Fatal("创建 Kafka 消费者组客户端失败",
			zap.Strings("brokers", kafkaCfg.Brokers),
			zap.String("组ID(group_id)", kafkaCfg.ConsumerGroupID),
			zap.Error(err),
		)
		return fmt.Errorf("创建消费者组客户端失败: %w", err)
	}
	zapLogger.Info("Kafka 消费者组客户端创建成功",
		zap.Strings("brokers", kafkaCfg.Brokers),
		zap.String("组ID(group_id)", kafkaCfg.ConsumerGroupID),
	)
	defer func() {
		zapLogger.Info("正在尝试关闭 Kafka 消费者组客户端 (defer)...")
		if err := consumerGroup.Close(); err != nil {
			zapLogger.Error("关闭 Kafka 消费者组客户端失败 (defer)", zap.Error(err))
		} else {
			zapLogger.Info("Kafka 消费者组客户端已成功关闭 (defer)。")
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 确保这里消费的是正确的 "待审核" 主题名称
		// 假设 kafkaCfg.Topics.PendingModeration 存储的是 "post_pending_audit"
		topics := []string{kafkaCfg.Topics.PendingModeration}
		zapLogger.Info("启动 Kafka 消费者组消费...",
			zap.Strings("订阅主题(topics)", topics),
			zap.String("组ID(group_id)", kafkaCfg.ConsumerGroupID),
		)

		for {
			if err := consumerGroup.Consume(ctx, topics, handler); err != nil {
				if errors.Is(err, sarama.ErrClosedConsumerGroup) {
					zapLogger.Info("Kafka 消费者组 Consume 循环优雅退出 (ErrClosedConsumerGroup)。", zap.Error(err))
					return
				}
				zapLogger.Error("Kafka 消费者组 Consume 过程中发生错误",
					zap.Error(err),
					zap.Strings("订阅主题(topics)", topics),
					zap.String("组ID(group_id)", kafkaCfg.ConsumerGroupID),
				)
				if ctx.Err() != nil {
					zapLogger.Info("上下文已取消 (在Consume错误后检查)，停止消费者组消费。", zap.Error(ctx.Err()))
					return
				}
				zapLogger.Info("等待后重试消费者组 Consume...", zap.Duration("重试间隔(retry_after)", 5*time.Second))
				select {
				case <-time.After(5 * time.Second):
				case <-ctx.Done():
					zapLogger.Info("上下文在重试等待期间取消，停止消费者组。", zap.Error(ctx.Err()))
					return
				}
			}
			// 检查上下文是否在 Consume 调用之后但在下一次循环之前被取消
			if ctx.Err() != nil {
				zapLogger.Info("Consume 返回后上下文已取消，停止消费者组。", zap.Error(ctx.Err()))
				return
			}
		}
	}()

	<-handler.ready // 等待 Setup 完成
	zapLogger.Info("Kafka 消费者组已启动并运行！")

	select {
	case <-ctx.Done():
		zapLogger.Info("父上下文已取消，正在关闭消费者组...")
	case s := <-sigterm:
		zapLogger.Info("收到关闭信号，正在关闭消费者组...", zap.String("信号(signal)", s.String()))
	}

	cancel()  // 通知 goroutine 停止
	wg.Wait() // 等待 goroutine 完成

	zapLogger.Info("Kafka 消费者组处理流程已完成关闭。")
	return nil
}
