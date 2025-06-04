package constants

import "time"

const (
	KafkaConsumerBatchSize    = 10              // 示例：一次处理100条消息作为一个批次
	KafkaConsumerBatchTimeout = 5 * time.Minute // 示例：批次最大等待时间

	AliyunAPIMaxRetriesForThrottling = 3               // 例如，针对限流最多重试3次
	AliyunAPIBaseDelayForThrottling  = 1 * time.Second // 基础退避时间
)
