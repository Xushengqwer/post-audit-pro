package constants

import "time"

const (
	KafkaProducerMaxSendRetries = 3               // 发送结果到Kafka的最大重试次数
	KafkaProducerSendRetryDelay = 1 * time.Second // 每次重试的延迟时间
)
