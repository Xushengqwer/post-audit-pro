package models

// DeadLetterEvent 定义了发送到死信队列的消息结构。
type DeadLetterEvent struct {
	DLQEventID           string `json:"dlq_event_id"`                   // 死信队列事件自身的唯一ID (由发送方生成)
	OriginalTopic        string `json:"original_topic"`                 // 原始消息所在的主题
	OriginalPartition    int32  `json:"original_partition"`             // 原始消息所在的分区
	OriginalOffset       int64  `json:"original_offset"`                // 原始消息的偏移量
	OriginalMessageKey   string `json:"original_message_key,omitempty"` // 原始消息的Key (如果存在，转换为字符串)
	OriginalMessageValue string `json:"original_message_value"`         // 原始消息体 (可以是JSON字符串或Base64编码的字节)
	FailureReason        string `json:"failure_reason"`                 // 处理失败的原因
	FailedAt             int64  `json:"failed_at"`                      // 失败发生的时间戳 (Unix Milliseconds)
	ProcessingService    string `json:"processing_service"`             // 处理失败的服务名称 (例如 "post-audit-service")
	// 可以添加其他诊断信息，如尝试次数、错误堆栈的简要信息等
	// AttemptCount int `json:"attempt_count,omitempty"`
	// ErrorStack   string `json:"error_stack,omitempty"`
}
