package kafka

import (
	"crypto/tls"
	"crypto/x509" // 需要导入以处理 CA 和客户端证书
	"fmt"
	"os" // 需要导入以读取证书文件和获取主机名
	"time"

	"github.com/IBM/sarama" // 根据你的环境，这里可能是 IBM/sarama 或 Shopify/sarama
	// 导入你的 config 包以使用 KafkaConfig 结构体
	"github.com/Xushengqwer/post_audit/internal/config"
	// 导入你的公共日志模块
	"github.com/Xushengqwer/go-common/core"
	// 导入你的常量包
	"github.com/Xushengqwer/post_audit/internal/constants"
	"go.uber.org/zap" // 导入 zap 以便在适配器和函数内部使用 zap.Field
)

// zapToSaramaAdapter 将 core.ZapLogger 适配为 sarama.StdLogger 接口
type zapToSaramaAdapter struct {
	logger *core.ZapLogger
}

// NewZapToSaramaAdapter 创建一个新的适配器实例
func NewZapToSaramaAdapter(logger *core.ZapLogger) sarama.StdLogger {
	if logger == nil {
		// 如果传入的 logger 为 nil，可以返回一个空操作的 logger 或 panic，
		// 或者让 sarama.Logger 保持未设置状态 (它会默认使用 log.New(os.Stderr, "[Sarama] ", log.LstdFlags)))
		// 为简单起见，如果外部不传入 logger，Sarama 会使用其默认 logger。
		return nil
	}
	return &zapToSaramaAdapter{logger: logger}
}

// Print 实现 sarama.StdLogger 接口 (通常对应 Info 级别)
func (a *zapToSaramaAdapter) Print(v ...interface{}) {
	if a.logger != nil {
		a.logger.Info("Sarama", zap.String("internal_log", fmt.Sprint(v...)))
	}
}

// Printf 实现 sarama.StdLogger 接口 (通常对应 Info 级别)
func (a *zapToSaramaAdapter) Printf(format string, v ...interface{}) {
	if a.logger != nil {
		a.logger.Info("Sarama", zap.String("internal_log", fmt.Sprintf(format, v...)))
	}
}

// Println 实现 sarama.StdLogger 接口 (通常对应 Info 级别)
func (a *zapToSaramaAdapter) Println(v ...interface{}) {
	if a.logger != nil {
		a.logger.Info("Sarama", zap.String("internal_log", fmt.Sprintln(v...)))
	}
}

// GetSaramaConfig 是一个辅助函数，用于将我们定义的 config.KafkaConfig 转换为 sarama.Config
// cfg: 应用的 Kafka 配置
// appName: 应用名称，主要用于 ClientID (尽管我们现在主要使用 constants.ServiceName)
// zapLogger: 你自定义的 ZapLogger 实例，用于记录此函数内的消息以及桥接 Sarama 内部日志
func GetSaramaConfig(cfg config.KafkaConfig, appName string, zapLogger *core.ZapLogger) (*sarama.Config, error) {
	saramaLogAdapter := NewZapToSaramaAdapter(zapLogger)
	if saramaLogAdapter != nil {
		sarama.Logger = saramaLogAdapter // 设置 Sarama 内部日志记录器
	}

	saramaCfg := sarama.NewConfig()

	// --- Kafka 版本设置 ---
	if cfg.Version == "" {
		if zapLogger != nil {
			zapLogger.Error("Kafka配置错误: kafka.version 未指定", zap.String("advice", "Sarama 需要明确的版本以确保兼容性"))
		}
		return nil, fmt.Errorf("kafka.version 未在配置中指定，Sarama 需要明确的版本以确保兼容性")
	}
	version, err := sarama.ParseKafkaVersion(cfg.Version)
	if err != nil {
		if zapLogger != nil {
			zapLogger.Error("Kafka配置错误: 无效的 kafka.version", zap.String("configured_version", cfg.Version), zap.Error(err))
		}
		return nil, fmt.Errorf("无效的 Kafka 版本配置 '%s': %w", cfg.Version, err)
	}
	saramaCfg.Version = version
	if zapLogger != nil {
		zapLogger.Info("Sarama Kafka 客户端协议版本已设置", zap.String("version", version.String()))
	}

	// --- 生产者配置 ---
	currentRequiredAcks := sarama.WaitForAll // 用于后续幂等性检查的默认值
	switch cfg.Producer.RequiredAcks {
	case "no_response":
		saramaCfg.Producer.RequiredAcks = sarama.NoResponse
		currentRequiredAcks = sarama.NoResponse
	case "wait_for_local":
		saramaCfg.Producer.RequiredAcks = sarama.WaitForLocal
		currentRequiredAcks = sarama.WaitForLocal
	case "wait_for_all":
		saramaCfg.Producer.RequiredAcks = sarama.WaitForAll
		currentRequiredAcks = sarama.WaitForAll
	default:
		saramaCfg.Producer.RequiredAcks = sarama.WaitForAll // 默认 WaitForAll
		currentRequiredAcks = sarama.WaitForAll
		if zapLogger != nil {
			zapLogger.Warn("生产者 RequiredAcks 配置无效或未提供",
				zap.String("configured_acks", cfg.Producer.RequiredAcks),
				zap.String("using_default", "WaitForAll"))
		}
	}
	saramaCfg.Producer.Timeout = cfg.Producer.TimeoutMs * time.Millisecond
	saramaCfg.Producer.Return.Successes = cfg.Producer.ReturnSuccesses
	saramaCfg.Producer.Return.Errors = cfg.Producer.ReturnErrors
	if cfg.Producer.MaxMessageBytes > 0 {
		saramaCfg.Producer.MaxMessageBytes = cfg.Producer.MaxMessageBytes
	}

	// --- 幂等生产者 (Idempotent Producer) ---
	// 对于审核结果这类重要事件，启用幂等性是推荐的，以确保消息在 Broker 端精确写入一次。
	// 要求:
	//   1. Producer.RequiredAcks 必须设置为 sarama.WaitForAll (acks=-1)。
	//   2. Kafka Broker 版本 >= 0.11.0.0。
	//   3. 为确保幂等性，Sarama 的 Net.MaxOpenRequests (每个连接的最大未完成请求数) 应设置为 1。
	if currentRequiredAcks == sarama.WaitForAll {
		if !saramaCfg.Version.IsAtLeast(sarama.V0_11_0_0) { // 检查 Kafka 版本是否支持幂等性
			if zapLogger != nil {
				zapLogger.Warn("幂等生产者启用条件检查: Kafka 版本过低",
					zap.String("configured_version", saramaCfg.Version.String()),
					zap.String("required_version", ">= 0.11.0.0"),
					zap.String("advice", "幂等生产者将不会启用"))
			}
		} else {
			saramaCfg.Producer.Idempotent = true
			// 当 Producer.Idempotent 设置为 true 时，Sarama 会自动将 Producer.Retry.Max 设置为 math.MaxInt32 (如果用户未设置)。
			// 同时，为了完全符合 Kafka 协议对幂等生产者的要求 (保证消息顺序和精确一次)，
			// 每个连接的未完成请求数 (MaxInFlightRequests) 应为1。
			saramaCfg.Net.MaxOpenRequests = 1 // Sarama 中等效于 MaxInFlightRequestsPerConnection
			if zapLogger != nil {
				zapLogger.Info("幂等生产者已启用",
					zap.Bool("idempotent", true),
					zap.Int("net.max_open_requests", saramaCfg.Net.MaxOpenRequests))
			}
		}
	} else {
		if zapLogger != nil {
			// 如果用户期望幂等性但 acks 不满足，这里可以加一个更明确的警告。
			// 不过，我们是基于 acks=all 才考虑启用，所以这里主要是记录未启用的情况。
			zapLogger.Info("幂等生产者未启用 (原因: Producer.RequiredAcks 未设置为 'all')",
				zap.String("current_acks_setting", fmt.Sprintf("%d", currentRequiredAcks)))
		}
	}

	// --- 消费者配置 ---
	initialOffset := sarama.OffsetNewest // 默认从最新开始
	if cfg.Consumer.Offsets.Initial == "earliest" {
		initialOffset = sarama.OffsetOldest
	} else if cfg.Consumer.Offsets.Initial != "latest" && cfg.Consumer.Offsets.Initial != "" {
		if zapLogger != nil {
			zapLogger.Warn("消费者 Offsets.Initial 配置无效",
				zap.String("configured_initial_offset", cfg.Consumer.Offsets.Initial),
				zap.String("advice", "将使用 'latest'。审核服务通常建议使用 'earliest'。"))
		}
	}
	// 业务需求: 审核服务必须从 "earliest" 开始消费
	if initialOffset != sarama.OffsetOldest {
		if zapLogger != nil {
			zapLogger.Info("业务需求覆盖: 审核服务将强制从 'earliest' (OffsetOldest) 开始消费",
				zap.String("original_configured_offset", cfg.Consumer.Offsets.Initial))
		}
	}
	saramaCfg.Consumer.Offsets.Initial = sarama.OffsetOldest // 强制 earliest

	saramaCfg.Consumer.Offsets.AutoCommit.Enable = cfg.Consumer.Offsets.AutoCommitEnable
	if cfg.Consumer.Offsets.AutoCommitEnable {
		saramaCfg.Consumer.Offsets.AutoCommit.Interval = cfg.Consumer.Offsets.AutoCommitIntervalMs * time.Millisecond
		if zapLogger != nil {
			zapLogger.Warn("消费者偏移量自动提交已启用",
				zap.Duration("interval", saramaCfg.Consumer.Offsets.AutoCommit.Interval),
				zap.String("recommendation", "对于可靠处理，建议禁用自动提交并手动管理。"))
		}
	}

	saramaCfg.Consumer.Group.Session.Timeout = cfg.Consumer.SessionTimeoutMs * time.Millisecond
	if cfg.Consumer.HeartbeatIntervalMs > 0 {
		saramaCfg.Consumer.Group.Heartbeat.Interval = cfg.Consumer.HeartbeatIntervalMs * time.Millisecond
	} else if saramaCfg.Consumer.Group.Session.Timeout > 0 {
		saramaCfg.Consumer.Group.Heartbeat.Interval = saramaCfg.Consumer.Group.Session.Timeout / 3
	} else {
		if zapLogger != nil {
			zapLogger.Warn("消费者 SessionTimeoutMs 和 HeartbeatIntervalMs 均未有效配置",
				zap.String("advice", "将依赖 Sarama 默认值。建议显式配置。"))
		}
	}

	// 消费者组重平衡策略: 使用 Sticky 策略
	saramaCfg.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategySticky()}
	if zapLogger != nil {
		// 日志信息可以保持不变，因为它仍然反映了使用的策略类型
		zapLogger.Info("消费者重平衡策略已设置", zap.String("strategy", "Sticky (via GroupStrategies)"))
	}

	// --- 网络与安全配置 (SASL/TLS) ---
	if cfg.EnableSASL {
		saramaCfg.Net.SASL.Enable = true
		saramaCfg.Net.SASL.User = cfg.SASLUser
		saramaCfg.Net.SASL.Password = cfg.SASLPassword
		switch cfg.SASLMechanism {
		case "PLAIN":
			saramaCfg.Net.SASL.Mechanism = sarama.SASLTypePlaintext
		case "SCRAM-SHA-256":
			saramaCfg.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA256
		case "SCRAM-SHA-512":
			saramaCfg.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA512
		default:
			if cfg.SASLMechanism == "" && cfg.SASLUser != "" {
				saramaCfg.Net.SASL.Mechanism = sarama.SASLTypePlaintext
				if zapLogger != nil {
					zapLogger.Info("SASL 已启用但未指定机制，将默认使用 PLAIN (基于用户名存在)")
				}
			} else if cfg.SASLMechanism != "" {
				if zapLogger != nil {
					zapLogger.Error("不支持的 SASL 机制", zap.String("configured_mechanism", cfg.SASLMechanism))
				}
				return nil, fmt.Errorf("不支持的 SASL 机制: '%s'", cfg.SASLMechanism)
			} else {
				if zapLogger != nil {
					zapLogger.Warn("SASL 已启用，但 SASLUser 和 SASLMechanism 均未配置。SASL 可能无法正常工作。")
				}
			}
		}
		if zapLogger != nil && saramaCfg.Net.SASL.Mechanism != "" {
			zapLogger.Info("SASL 已配置", zap.String("mechanism", string(saramaCfg.Net.SASL.Mechanism)))
		}
	}

	if cfg.EnableTLS {
		saramaCfg.Net.TLS.Enable = true
		tlsConfig := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		if cfg.TLSInsecureSkipVerify {
			tlsConfig.InsecureSkipVerify = true
			if zapLogger != nil {
				zapLogger.Warn("TLS InsecureSkipVerify 已启用。存在安全风险，不应在生产环境中使用。")
			}
		}
		if cfg.TLSCaFile != "" {
			caCert, err := os.ReadFile(cfg.TLSCaFile)
			if err != nil {
				if zapLogger != nil {
					zapLogger.Error("读取 CA 证书文件失败", zap.String("ca_file", cfg.TLSCaFile), zap.Error(err))
				}
				return nil, fmt.Errorf("读取 CA 证书文件失败 %s: %w", cfg.TLSCaFile, err)
			}
			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caCert) {
				if zapLogger != nil {
					zapLogger.Error("无法将 CA 证书添加到证书池", zap.String("ca_file", cfg.TLSCaFile), zap.String("advice", "请检查文件内容是否为有效的 PEM 格式。"))
				}
				return nil, fmt.Errorf("无法将 CA 证书添加到证书池 (文件: %s)", cfg.TLSCaFile)
			}
			tlsConfig.RootCAs = caCertPool
		}
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			clientCert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
			if err != nil {
				if zapLogger != nil {
					zapLogger.Error("加载客户端 TLS 证书和密钥失败", zap.String("cert_file", cfg.TLSCertFile), zap.String("key_file", cfg.TLSKeyFile), zap.Error(err))
				}
				return nil, fmt.Errorf("加载客户端证书和密钥失败: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{clientCert}
		} else if (cfg.TLSCertFile != "" && cfg.TLSKeyFile == "") || (cfg.TLSCertFile == "" && cfg.TLSKeyFile != "") {
			if zapLogger != nil {
				zapLogger.Error("TLS 客户端认证配置错误", zap.String("advice", "TLSCertFile 和 TLSKeyFile 必须同时提供或同时不提供"))
			}
			return nil, fmt.Errorf("TLS 客户端认证配置错误: TLSCertFile 和 TLSKeyFile 必须同时提供或同时不提供")
		}
		saramaCfg.Net.TLS.Config = tlsConfig
		if zapLogger != nil {
			zapLogger.Info("TLS 已启用和配置")
		}
	}

	// --- ClientID ---
	saramaCfg.ClientID = constants.ServiceName
	if appName != constants.ServiceName && appName != "" {
		if zapLogger != nil {
			zapLogger.Info("GetSaramaConfig 收到的 appName 与常量 ServiceName 不同",
				zap.String("parameter_app_name", appName),
				zap.String("constant_service_name", constants.ServiceName),
				zap.String("client_id_used", saramaCfg.ClientID))
		}
	}
	if saramaCfg.ClientID == "" {
		defaultClientID := "sarama-audit-client"
		if zapLogger != nil {
			zapLogger.Warn("ClientID 为空 (可能常量 ServiceName 未设置)，将使用默认值", zap.String("default_client_id", defaultClientID))
		}
		saramaCfg.ClientID = defaultClientID
	}
	if zapLogger != nil {
		zapLogger.Info("Sarama ClientID 已设置", zap.String("client_id", saramaCfg.ClientID))
	}

	return saramaCfg, nil
}
