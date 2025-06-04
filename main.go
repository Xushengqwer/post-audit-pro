// File: main.go
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Xushengqwer/go-common/core"
	"github.com/Xushengqwer/post_audit/internal/aliyunclient"
	"github.com/Xushengqwer/post_audit/internal/auditplatform" // <--- 新增导入: 通用审核平台接口
	"github.com/Xushengqwer/post_audit/internal/config"
	"github.com/Xushengqwer/post_audit/internal/constants"
	"github.com/Xushengqwer/post_audit/internal/kafka"
	"go.uber.org/zap"
	// 如果未来有其他平台客户端，也需要在这里导入
	// "github.com/Xushengqwer/post_audit/internal/tencentclient" // 例如
)

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", "internal/config/config.development.yaml", "指定配置文件的路径")
	flag.Parse()

	var cfg config.AppConfig
	if err := core.LoadConfig(configFile, &cfg); err != nil {
		log.Fatalf("致命错误: 加载配置文件 '%s' 失败: %v", configFile, err)
	}

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

	// --- 审核平台初始化 (根据配置选择) ---
	var moderator auditplatform.ContentReviewer // <--- 使用接口类型
	var err error                               // 用于捕获客户端初始化错误

	logger.Info("根据配置初始化审核平台...", zap.String("configured_platform", cfg.AuditPlatform))

	switch cfg.AuditPlatform {
	case "aliyun":
		aliClient, clientErr := aliyunclient.NewAliyunAuditClient(cfg.AliyunAudit, logger)
		if clientErr != nil {
			logger.Fatal("初始化阿里云审核客户端失败", zap.Error(clientErr))
		}
		moderator = aliClient // AliyunAuditClient 实现了 ContentReviewer 接口
		logger.Info("审核平台初始化成功: 阿里云")
	// case "tencent": // 示例：如果未来添加腾讯云审核
	//  tencentCfg := cfg.TencentAudit // 假设 AppConfig 中有 TencentAudit 配置
	// 	tencentClient, clientErr := tencentclient.NewTencentAuditClient(tencentCfg, logger)
	// 	if clientErr != nil {
	// 		logger.Fatal("初始化腾讯云审核客户端失败", zap.Error(clientErr))
	// 	}
	// 	moderator = tencentClient
	//  logger.Info("审核平台初始化成功: 腾讯云")
	default:
		logger.Fatal("未知的或未配置的审核平台",
			zap.String("configured_platform", cfg.AuditPlatform),
			zap.String("supported_platforms", "aliyun"), // 列出当前支持的
		)
	}
	// moderator 现在持有了选定的审核平台客户端实例

	// --- Kafka 相关初始化 ---
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

	// --- 业务逻辑处理器初始化 ---
	// 将抽象的 moderator 注入到 auditService
	auditService := kafka.NewAuditProcessorService(logger, moderator, kafkaProd) // <--- 修改: 注入接口
	logger.Info("业务逻辑处理器 (AuditProcessorService) 初始化成功。")

	// --- 启动 Kafka 消费者组 ---
	logger.Info("准备启动 Kafka 消费者组...")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := kafka.StartConsumerGroup(&cfg, logger, auditService, kafkaProd); err != nil {
			logger.Error("Kafka 消费者组运行出错或已停止", zap.Error(err))
			// 可以考虑在这里发送信号以关闭主程序
			// sigChan <- syscall.SIGINT
		}
	}()
	logger.Info("Kafka 消费者组已在后台goroutine启动（或即将启动）。")

	receivedSignal := <-sigChan
	logger.Info("收到关闭信号，开始优雅关闭服务...", zap.String("信号", receivedSignal.String()))

	// StartConsumerGroup 内部会处理 context 的取消，生产者和日志的关闭已通过 defer 处理。
	logger.Info("服务已成功关闭。")
}
