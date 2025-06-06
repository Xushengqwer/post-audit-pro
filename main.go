package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strings" // <-- 确保导入
	"syscall"

	"github.com/Xushengqwer/go-common/core"
	"github.com/Xushengqwer/post_audit/internal/aliyunclient"
	"github.com/Xushengqwer/post_audit/internal/auditplatform"
	"github.com/Xushengqwer/post_audit/internal/config"
	"github.com/Xushengqwer/post_audit/internal/constants"
	"github.com/Xushengqwer/post_audit/internal/kafka"
	"go.uber.org/zap"
)

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", "internal/config/config.development.yaml", "指定配置文件的路径")
	flag.Parse()

	var cfg config.AppConfig
	if err := core.LoadConfig(configFile, &cfg); err != nil {
		log.Fatalf("致命错误: 加载配置文件 '%s' 失败: %v", configFile, err)
	}

	// --- 手动从环境变量覆盖关键配置 ---
	log.Println("检查环境变量以覆盖文件配置...")
	if platform := os.Getenv("AUDIT_PLATFORM"); platform != "" {
		cfg.AuditPlatform = platform
		log.Printf("通过环境变量覆盖了 AuditPlatform: %s\n", platform)
	}
	if level := os.Getenv("ZAPCONFIG_LEVEL"); level != "" {
		cfg.ZapConfig.Level = level
		log.Printf("通过环境变量覆盖了 ZapConfig.Level: %s\n", level)
	}
	if brokers := os.Getenv("KAFKA_BROKERS"); brokers != "" {
		cfg.Kafka.Brokers = strings.Split(brokers, ",")
		log.Printf("通过环境变量覆盖了 Kafka.Brokers: %v\n", cfg.Kafka.Brokers)
	}
	if keyID := os.Getenv("ALIYUN_AUDIT_ACCESS_KEY_ID"); keyID != "" {
		cfg.AliyunAudit.AccessKeyID = keyID
		log.Printf("通过环境变量覆盖了 AliyunAudit.AccessKeyID\n")
	}
	if keySecret := os.Getenv("ALIYUN_AUDIT_ACCESS_KEY_SECRET"); keySecret != "" {
		cfg.AliyunAudit.AccessKeySecret = keySecret
		log.Printf("通过环境变量覆盖了 AliyunAudit.AccessKeySecret\n")
	}
	if region := os.Getenv("ALIYUN_AUDIT_REGION_ID"); region != "" {
		cfg.AliyunAudit.RegionID = region
		log.Printf("通过环境变量覆盖了 AliyunAudit.RegionID: %s\n", region)
	}
	if endpoint := os.Getenv("ALIYUN_AUDIT_ENDPOINT"); endpoint != "" {
		cfg.AliyunAudit.Endpoint = endpoint
		log.Printf("通过环境变量覆盖了 AliyunAudit.Endpoint: %s\n", endpoint)
	}
	// --- 结束环境变量覆盖 ---

	logger, loggerErr := core.NewZapLogger(cfg.ZapConfig)
	if loggerErr != nil {
		log.Fatalf("致命错误: 初始化 ZapLogger 失败: %v", loggerErr)
	}
	defer func() {
		logger.Info("正在同步所有日志条目...")
		if err := logger.Logger().Sync(); err != nil {
			log.Printf("警告: ZapLogger Sync 操作失败: %v\n", err)
		}
	}()
	logger.Info("Logger 初始化成功。")

	// ... (main 函数的其余部分保持不变) ...
	var moderator auditplatform.ContentReviewer
	var err error

	logger.Info("根据配置初始化审核平台...", zap.String("configured_platform", cfg.AuditPlatform))

	switch cfg.AuditPlatform {
	case "aliyun":
		aliClient, clientErr := aliyunclient.NewAliyunAuditClient(cfg.AliyunAudit, logger)
		if clientErr != nil {
			logger.Fatal("初始化阿里云审核客户端失败", zap.Error(clientErr))
		}
		moderator = aliClient
		logger.Info("审核平台初始化成功: 阿里云")
	default:
		logger.Fatal("未知的或未配置的审核平台",
			zap.String("configured_platform", cfg.AuditPlatform),
			zap.String("supported_platforms", "aliyun"),
		)
	}

	saramaCfg, err := kafka.GetSaramaConfig(cfg.Kafka, constants.ServiceName, logger)
	if err != nil {
		logger.Fatal("创建 Kafka Sarama 配置失败", zap.Error(err))
	}
	logger.Info("Kafka Sarama 配置创建成功。")

	kafkaProd, err := kafka.NewKafkaProducer(cfg.Kafka.Brokers, saramaCfg, cfg.Kafka.Topics, logger)
	if err != nil {
		logger.Fatal("初始化 Kafka 生产者失败", zap.Error(err))
	}
	defer func() {
		logger.Info("正在关闭 Kafka 生产者...")
		if err := kafkaProd.Close(); err != nil {
			logger.Error("关闭 Kafka 生产者失败", zap.Error(err))
		} else {
			logger.Info("Kafka 生产者已成功关闭。")
		}
	}()
	logger.Info("Kafka 生产者初始化成功。")

	auditService := kafka.NewAuditProcessorService(logger, moderator, kafkaProd)
	logger.Info("业务逻辑处理器 (AuditProcessorService) 初始化成功。")

	logger.Info("准备启动 Kafka 消费者组...")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := kafka.StartConsumerGroup(&cfg, logger, auditService, kafkaProd); err != nil {
			logger.Error("Kafka 消费者组运行出错或已停止", zap.Error(err))
		}
	}()
	logger.Info("Kafka 消费者组已在后台goroutine启动（或即将启动）。")

	receivedSignal := <-sigChan
	logger.Info("收到关闭信号，开始优雅关闭服务...", zap.String("信号", receivedSignal.String()))

	logger.Info("服务已成功关闭。")
}
