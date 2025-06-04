package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IBM/sarama"
	"github.com/Xushengqwer/go-common/core"
	"github.com/google/uuid" // 用于为DLQ事件生成唯一ID
	"go.uber.org/zap"

	"github.com/Xushengqwer/go-common/models/kafkaevents"

	// 2. 导入项目内的包
	"github.com/Xushengqwer/post_audit/internal/config"
	"github.com/Xushengqwer/post_audit/internal/constants"
	"github.com/Xushengqwer/post_audit/internal/models" // DeadLetterEvent 仍使用此处的定义
)

// EventProducer 定义了发送审核结果事件和死信事件到 Kafka 的接口。
type EventProducer interface {
	// SendApprovedEvent 发送帖子审核通过事件。
	// ctx 用于传递请求范围的上下文，例如追踪信息。
	// event 是要发送的审核通过事件对象。
	// 注意：参数类型已更新为 kafkaevents.PostApprovedEvent
	SendApprovedEvent(ctx context.Context, event *kafkaevents.PostApprovedEvent) error

	// SendRejectedEvent 发送帖子审核不通过事件。
	// ctx 用于传递请求范围的上下文。
	// event 是要发送的审核不通过事件对象。
	// 注意：参数类型已更新为 kafkaevents.PostRejectedEvent
	SendRejectedEvent(ctx context.Context, event *kafkaevents.PostRejectedEvent) error

	// SendToDLQ 将处理失败的原始消息发送到死信队列。
	// ctx 用于传递请求范围的上下文。
	// originalMessage 是从 Kafka 消费到的原始消息。
	// failureReason 描述了处理失败的原因。
	SendToDLQ(ctx context.Context, originalMessage *sarama.ConsumerMessage, failureReason string) error

	// Close 关闭生产者并释放资源。
	Close() error
}

// kafkaProducer 实现了 EventProducer 接口，使用 Sarama 同步生产者。
type kafkaProducer struct {
	producer sarama.SyncProducer
	topics   config.KafkaTopics // 保存主题名称配置
	logger   *core.ZapLogger
}

// NewKafkaProducer 创建一个新的 kafkaProducer 实例。
// brokers: Kafka broker 地址列表。
// saramaCfg: Sarama 客户端配置 (通常由 GetSaramaConfig 生成，并针对生产者进行调整)。
// appTopics: 应用的 Kafka 主题配置。
// logger: 日志记录器。
func NewKafkaProducer(brokers []string, saramaCfg *sarama.Config, appTopics config.KafkaTopics, logger *core.ZapLogger) (EventProducer, error) {
	// 确保生产者配置适合同步生产者
	// 对于同步生产者，Return.Successes 和 Return.Errors 必须都设置为 true。
	// GetSaramaConfig 中应该已经处理了这一点。
	if !saramaCfg.Producer.Return.Successes || !saramaCfg.Producer.Return.Errors {
		logger.Error("Kafka生产者配置错误: 对于同步生产者, Return.Successes 和 Return.Errors 必须都为 true")
		return nil, fmt.Errorf("kafka生产者配置错误: 同步生产者需要 Return.Successes=true 和 Return.Errors=true")
	}

	producer, err := sarama.NewSyncProducer(brokers, saramaCfg)
	if err != nil {
		logger.Error("创建 Kafka 同步生产者失败",
			zap.Strings("brokers", brokers),
			zap.Error(err),
		)
		return nil, fmt.Errorf("创建 Kafka 同步生产者失败: %w", err)
	}
	logger.Info("Kafka 同步生产者创建成功", zap.Strings("brokers", brokers))

	return &kafkaProducer{
		producer: producer,
		topics:   appTopics,
		logger:   logger,
	}, nil
}

// SendApprovedEvent 实现 EventProducer 接口。
// 注意：参数类型已更新为 kafkaevents.PostApprovedEvent
func (p *kafkaProducer) SendApprovedEvent(ctx context.Context, event *kafkaevents.PostApprovedEvent) error {
	if event == nil {
		p.logger.Warn("SendApprovedEvent: 尝试发送空的审核通过事件")
		return fmt.Errorf("审核通过事件不能为空")
	}
	// 假设 p.topics.Approved 存储的是 "post_audit_approved" 这个统一后的 Topic 名称
	if p.topics.Approved == "" {
		p.logger.Error("SendApprovedEvent: 'approved' (审核通过) 主题未在配置中定义")
		return fmt.Errorf("'approved' (审核通过) 主题未配置")
	}

	// 将事件序列化为 JSON
	eventJSON, err := json.Marshal(event)
	if err != nil {
		p.logger.Error("SendApprovedEvent: 序列化 PostApprovedEvent 失败",
			zap.String("事件ID(event_id)", event.EventID),
			zap.Uint64("帖子ID(post_id)", event.Post.ID), // 从 event.Post.ID 获取
			zap.Error(err),
		)
		return fmt.Errorf("序列化 PostApprovedEvent 失败: %w", err)
	}

	// 创建生产者消息
	// 使用事件ID或帖子ID作为消息的Key，有助于Kafka进行分区（如果分区策略基于Key）和日志压缩（如果启用）。
	msg := &sarama.ProducerMessage{
		Topic: p.topics.Approved,
		Key:   sarama.StringEncoder(event.EventID), // 使用事件ID作为Key
		Value: sarama.ByteEncoder(eventJSON),
	}

	p.logger.Debug("准备发送审核通过事件到 Kafka",
		zap.String("主题(topic)", msg.Topic),
		zap.String("消息键(key)", event.EventID),
		// zap.ByteString("消息体(value)", eventJSON), // 消息体可能很大，Debug级别按需开启
	)

	// 发送消息
	partition, offset, err := p.producer.SendMessage(msg)
	if err != nil {
		p.logger.Error("SendApprovedEvent: 发送消息到 Kafka 'approved' 主题失败",
			zap.String("事件ID(event_id)", event.EventID),
			zap.Uint64("帖子ID(post_id)", event.Post.ID), // 从 event.Post.ID 获取
			zap.Error(err),
		)
		return fmt.Errorf("发送消息到 Kafka 'approved' 主题失败: %w", err)
	}

	p.logger.Info("成功发送审核通过事件到 Kafka",
		zap.String("主题(topic)", msg.Topic),
		zap.String("事件ID(event_id)", event.EventID),
		zap.Uint64("帖子ID(post_id)", event.Post.ID), // 从 event.Post.ID 获取
		zap.Int32("分区(partition)", partition),
		zap.Int64("偏移量(offset)", offset),
	)
	return nil
}

// SendRejectedEvent 实现 EventProducer 接口。
// 注意：参数类型已更新为 kafkaevents.PostRejectedEvent
func (p *kafkaProducer) SendRejectedEvent(ctx context.Context, event *kafkaevents.PostRejectedEvent) error {
	if event == nil {
		p.logger.Warn("SendRejectedEvent: 尝试发送空的审核不通过事件")
		return fmt.Errorf("审核不通过事件不能为空")
	}
	// 假设 p.topics.Rejected 存储的是 "post_audit_rejected" 这个统一后的 Topic 名称
	if p.topics.Rejected == "" {
		p.logger.Error("SendRejectedEvent: 'rejected' (审核不通过) 主题未在配置中定义")
		return fmt.Errorf("'rejected' (审核不通过) 主题未配置")
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		p.logger.Error("SendRejectedEvent: 序列化 PostRejectedEvent 失败",
			zap.String("事件ID(event_id)", event.EventID),
			zap.Uint64("帖子ID(post_id)", event.PostID),
			zap.Error(err),
		)
		return fmt.Errorf("序列化 PostRejectedEvent 失败: %w", err)
	}

	msg := &sarama.ProducerMessage{
		Topic: p.topics.Rejected,
		Key:   sarama.StringEncoder(event.EventID), // 使用事件ID作为Key
		Value: sarama.ByteEncoder(eventJSON),
	}

	p.logger.Debug("准备发送审核不通过事件到 Kafka",
		zap.String("主题(topic)", msg.Topic),
		zap.String("消息键(key)", event.EventID),
	)

	partition, offset, err := p.producer.SendMessage(msg)
	if err != nil {
		p.logger.Error("SendRejectedEvent: 发送消息到 Kafka 'rejected' 主题失败",
			zap.String("事件ID(event_id)", event.EventID),
			zap.Uint64("帖子ID(post_id)", event.PostID),
			zap.Error(err),
		)
		return fmt.Errorf("发送消息到 Kafka 'rejected' 主题失败: %w", err)
	}

	p.logger.Info("成功发送审核不通过事件到 Kafka",
		zap.String("主题(topic)", msg.Topic),
		zap.String("事件ID(event_id)", event.EventID),
		zap.Uint64("帖子ID(post_id)", event.PostID),
		zap.Int32("分区(partition)", partition),
		zap.Int64("偏移量(offset)", offset),
	)
	return nil
}

// SendToDLQ 实现 EventProducer 接口，将处理失败的原始消息发送到死信队列。
// DeadLetterEvent 仍使用 post_audit/internal/models 中的定义
func (p *kafkaProducer) SendToDLQ(ctx context.Context, originalMessage *sarama.ConsumerMessage, failureReason string) error {
	if originalMessage == nil {
		p.logger.Warn("SendToDLQ: 尝试发送空的原始消息到DLQ")
		return fmt.Errorf("发送到DLQ的原始消息不能为空")
	}
	// 假设 p.topics.DeadLetterQueue 存储的是死信队列的 Topic 名称
	if p.topics.DeadLetterQueue == "" {
		p.logger.Error("SendToDLQ: 'DeadLetterQueue' (死信队列) 主题未在配置中定义")
		return fmt.Errorf("'DeadLetterQueue' (死信队列) 主题未配置")
	}

	dlqEventID := uuid.NewString() // 为DLQ事件生成一个新的唯一ID

	originalValueStr := string(originalMessage.Value)

	// 使用 internal/models.DeadLetterEvent
	dlqEvent := models.DeadLetterEvent{
		DLQEventID:           dlqEventID,
		OriginalTopic:        originalMessage.Topic,
		OriginalPartition:    originalMessage.Partition,
		OriginalOffset:       originalMessage.Offset,
		OriginalMessageKey:   string(originalMessage.Key),
		OriginalMessageValue: originalValueStr,
		FailureReason:        failureReason,
		FailedAt:             time.Now().UnixMilli(),
		ProcessingService:    constants.ServiceName,
	}

	eventJSON, err := json.Marshal(dlqEvent)
	if err != nil {
		p.logger.Error("SendToDLQ: 序列化 DeadLetterEvent 失败",
			zap.String("DLQ事件ID(dlq_event_id)", dlqEventID),
			zap.String("原始主题(original_topic)", originalMessage.Topic),
			zap.Int64("原始偏移量(original_offset)", originalMessage.Offset),
			zap.Error(err),
		)
		return fmt.Errorf("序列化 DeadLetterEvent 失败: %w", err)
	}

	msg := &sarama.ProducerMessage{
		Topic: p.topics.DeadLetterQueue,
		Key:   sarama.StringEncoder(dlqEventID),
		Value: sarama.ByteEncoder(eventJSON),
	}

	p.logger.Debug("准备发送消息到死信队列 (DLQ)",
		zap.String("主题(topic)", msg.Topic),
		zap.String("消息键(key)", dlqEventID),
	)

	partition, offset, err := p.producer.SendMessage(msg)
	if err != nil {
		p.logger.Error("SendToDLQ: 发送消息到 Kafka DLQ 主题失败",
			zap.String("DLQ事件ID(dlq_event_id)", dlqEventID),
			zap.String("原始主题(original_topic)", originalMessage.Topic),
			zap.Error(err),
		)
		return fmt.Errorf("发送消息到 Kafka DLQ 主题失败: %w", err)
	}

	p.logger.Info("成功发送消息到死信队列 (DLQ)",
		zap.String("主题(topic)", msg.Topic),
		zap.String("DLQ事件ID(dlq_event_id)", dlqEventID),
		zap.String("原始主题(original_topic)", originalMessage.Topic),
		zap.Int64("原始偏移量(original_offset)", originalMessage.Offset),
		zap.Int32("DLQ分区(dlq_partition)", partition),
		zap.Int64("DLQ偏移量(dlq_offset)", offset),
	)
	return nil
}

// Close 实现 EventProducer 接口，关闭同步生产者。
func (p *kafkaProducer) Close() error {
	if p.producer != nil {
		p.logger.Info("正在关闭 Kafka 同步生产者...")
		if err := p.producer.Close(); err != nil {
			p.logger.Error("关闭 Kafka 同步生产者失败", zap.Error(err))
			return err
		}
		p.logger.Info("Kafka 同步生产者已成功关闭。")
	}
	return nil
}
