package config

// AliyunConfig 包含了阿里云内容安全审核服务的配置
type AliyunConfig struct {
	AccessKeyID           string   `mapstructure:"access_key_id"`
	AccessKeySecret       string   `mapstructure:"access_key_secret"`
	RegionID              string   `mapstructure:"region_id"`               // 例如 "cn-shanghai"
	Endpoint              string   `mapstructure:"endpoint"`                // 例如 "green-cip.cn-shanghai.aliyuncs.com"
	TextModerationService string   `mapstructure:"text_moderation_service"` // 关键: 阿里云控制台配置的文本审核服务代码，例如 "comment_detection" 或自定义的服务代码
	TimeoutMs             int64    `mapstructure:"timeout_ms"`              // 调用阿里云接口的超时时间 (毫秒)
	Scenes                []string `mapstructure:"scenes"`                  // 审核场景/标签列表, 例如 ["ad", "spam", "porn"] <--- 确保此字段存在
}
