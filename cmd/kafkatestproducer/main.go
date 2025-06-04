package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Xushengqwer/go-common/models/kafkaevents"
	"log"
	"time"

	"github.com/IBM/sarama"
	"github.com/Xushengqwer/go-common/core"
	"github.com/Xushengqwer/post_audit/internal/config"
	"github.com/Xushengqwer/post_audit/internal/constants"
	"github.com/Xushengqwer/post_audit/internal/kafka"
	"go.uber.org/zap"
)

func main() {
	var configFile string
	var numMessages int
	var messageKeyPrefix string

	flag.StringVar(&configFile, "config", "internal/config/config.development.yaml", "指定配置文件的路径")
	// ==========================================================================================
	// ===== 主要修改区域：开始 (修改 -n 参数的默认值为 20) =====
	// ==========================================================================================
	flag.IntVar(&numMessages, "n", 20, "要发送的测试消息数量") // <--- 将这里的 3 修改为 20
	// ==========================================================================================
	// ===== 主要修改区域：结束 =====
	// ==========================================================================================
	flag.StringVar(&messageKeyPrefix, "prefix", "test_post_", "消息键的前缀 (ID 将附加在此之后)")
	flag.Parse()

	// 1. 加载配置 (代码保持不变)
	var appCfg config.AppConfig
	if err := core.LoadConfig(configFile, &appCfg); err != nil {
		log.Fatalf("致命错误: 加载配置文件 '%s' 失败: %v", configFile, err)
	}

	// 2. 初始化 Logger (代码保持不变)
	logger, loggerErr := core.NewZapLogger(appCfg.ZapConfig)
	if loggerErr != nil {
		log.Fatalf("致命错误: 初始化 ZapLogger 失败: %v", loggerErr)
	}
	defer func() {
		_ = logger.Logger().Sync()
	}()

	logger.Info("测试生产者启动，配置文件加载成功。")

	// 3. 初始化 Kafka Sarama 配置 (代码保持不变)
	saramaCfg, err := kafka.GetSaramaConfig(appCfg.Kafka, constants.ServiceName+"_test_producer", logger)
	if err != nil {
		logger.Fatal("创建 Kafka Sarama 配置失败", zap.Error(err))
	}
	saramaCfg.Producer.Return.Successes = true
	saramaCfg.Producer.Return.Errors = true

	// 4. 初始化 Kafka 同步生产者 (代码保持不变)
	syncProducer, err := sarama.NewSyncProducer(appCfg.Kafka.Brokers, saramaCfg)
	if err != nil {
		logger.Fatal("创建 Kafka 同步生产者失败",
			zap.Strings("brokers", appCfg.Kafka.Brokers),
			zap.Error(err),
		)
	}
	defer func() {
		logger.Info("正在关闭 Kafka 同步生产者...")
		if err := syncProducer.Close(); err != nil {
			logger.Error("关闭 Kafka 同步生产者失败", zap.Error(err))
		} else {
			logger.Info("Kafka 同步生产者已成功关闭。")
		}
	}()
	logger.Info("Kafka 同步生产者创建成功。")

	// 5. 准备发送到待审核主题 (代码保持不变)
	targetTopic := appCfg.Kafka.Topics.PendingModeration
	if targetTopic == "" {
		logger.Fatal("配置错误: Kafka topics.pending_moderation 未定义")
	}
	logger.Info("将向主题发送消息", zap.String("topic", targetTopic))

	// 6. 定义并发送测试数据 (循环逻辑本身不需要改变，它会根据 numMessages 的值来决定循环次数)
	for i := 1; i <= numMessages; i++ {
		postID := uint64(time.Now().UnixNano()/1000000 + int64(i))
		testPost := kafkaevents.PostData{
			ID:             postID,
			Title:          fmt.Sprintf("测试帖子标题 %d - %s", i, time.Now().Format("15:04:05")),
			Content:        fmt.Sprintf("这是帖子 %d 的内容。它包含一些需要审核的文本，例如：促销、广告、或者一些友好的讨论。当前时间: %s", i, time.Now().String()),
			AuthorID:       fmt.Sprintf("author_%d", (i%5)+1),
			AuthorAvatar:   fmt.Sprintf("http://example.com/avatar/author%d.png", (i%5)+1),
			AuthorUsername: fmt.Sprintf("测试用户%d", (i%5)+1),
			Tags:           []string{"测试", fmt.Sprintf("标签%d", i)},
			Status:         0,
			ViewCount:      int64(i * 10),
			OfficialTag:    0,
			PricePerUnit:   0,
			CreatedAt:      time.Now().Unix(),
			UpdatedAt:      time.Now().Unix(),
		}

		if i%3 == 0 {
			testPost.Title = "包含敏感词的标题"
			testPost.Content += " 这是一条包含 违禁词 的信息，需要严格审核。"
		}
		if i%4 == 0 {
			testPost.Title = "正常内容的帖子标题"
			testPost.Content = "这是一条看起来非常正常的帖子内容，应该可以通过审核。"
		}

		postJSON, err := json.Marshal(testPost)
		if err != nil {
			logger.Error("序列化 FullPostData 失败", zap.Uint64("post_id", testPost.ID), zap.Error(err))
			continue
		}

		msgKey := fmt.Sprintf("%s%d", messageKeyPrefix, testPost.ID)
		producerMessage := &sarama.ProducerMessage{
			Topic: targetTopic,
			Key:   sarama.StringEncoder(msgKey),
			Value: sarama.ByteEncoder(postJSON),
		}

		partition, offset, err := syncProducer.SendMessage(producerMessage)
		if err != nil {
			logger.Error("发送消息到 Kafka 失败",
				zap.String("topic", targetTopic),
				zap.String("key", msgKey),
				zap.Error(err),
			)
		} else {
			logger.Info("成功发送消息到 Kafka",
				zap.String("topic", targetTopic),
				zap.String("key", msgKey),
				zap.Uint64("post_id", testPost.ID),
				zap.Int32("partition", partition),
				zap.Int64("offset", offset),
			)
		}
		time.Sleep(500 * time.Millisecond) // 保持这个延时，避免因发送过快立即触发限流
	}

	logger.Info("所有测试消息已发送完毕。")
}
