package auditplatform

// TaskData 代表一个通用的内容审核任务单元。
// 它包含了执行审核所需的最基本信息。
type TaskData struct {
	// ID 是此审核任务的唯一标识符，通常对应您系统中的原始内容ID（例如帖子ID）。
	ID string `json:"id"`

	// Content 是需要审核的文本内容。
	// 它可以是帖子标题、正文的组合，或其他需要审核的文本片段。
	Content string `json:"content"`

	// ContentType 指示内容类型，例如 "text", "image_url", "audio_url"。
	// 初期我们主要关注文本，但设计上可以预留扩展性。
	// 对于纯文本审核，此字段可以暂时不强制使用或默认为 "text"。
	ContentType string `json:"contentType,omitempty"`

	// UserID 是内容发布者的用户ID (可选)。
	// 某些审核平台可能会利用用户信息进行更精准的判断。
	UserID string `json:"userId,omitempty"`

	// CallbackURL 是一个可选字段，用于指定审核完成后接收异步通知的URL。
	// 对于同步API调用模式，此字段可能不被使用。
	CallbackURL string `json:"callbackUrl,omitempty"`

	// Extra 是一个可选字段，用于传递一些额外的、审核平台可能需要的自定义参数。
	// 使用 map[string]interface{} 可以提供灵活性。
	Extra map[string]interface{} `json:"extra,omitempty"`
}

// RejectionDetail 封装了内容审核未通过时，具体的风险或拒绝原因。
type RejectionDetail struct {
	// Label 是审核命名的主要风险类别或标签。
	// 例如："spam", "porn", "ad", "abuse"。
	// 这对应阿里云返回结果中顶层的 "Label"。
	Label string `json:"label"`

	// Suggestion 是针对此具体风险标签的建议 (通常来源于审核平台的直接建议)。
	Suggestion string `json:"suggestion,omitempty"`

	// Score 是此风险标签的置信度得分 (通常在 0.0 到 100.0 之间)。
	Score float64 `json:"score,omitempty"`

	// MatchedContent 是文本中实际命中风险规则的具体内容片段或关键词。
	// 这通常从审核平台返回的更细致的详情中提取 (例如阿里云的 Results[].Details[].Contexts)。
	MatchedContent []string `json:"matchedContent,omitempty"`
}

// ReviewResult 代表单个审核任务的处理结果。
type ReviewResult struct {
	// OriginalTaskID 对应提交审核时 TaskData 中的 ID。
	OriginalTaskID string `json:"originalTaskId"`

	// ProviderTaskID 是审核平台为此任务生成的唯一ID (可选, 用于追踪)。
	ProviderTaskID string `json:"providerTaskId,omitempty"`

	// Suggestion 是审核平台对该内容的总体建议。
	// 常见值:
	// - "pass": 内容审核通过，无风险或风险可接受。
	// - "review": 内容存在潜在风险，建议人工复审。
	// - "block": 内容存在明确风险，建议直接拒绝或屏蔽。
	Suggestion string `json:"suggestion"`

	// Details 包含了审核未通过或需要复审时的具体风险详情。
	// 如果 Suggestion 是 "pass"，此列表通常为空。
	Details []RejectionDetail `json:"details,omitempty"`

	// Error 记录了在处理此单个审核任务时发生的错误。
	// 注意：这个字段通常不在JSON序列化中，主要用于程序内部错误传递。
	Error error `json:"-"`
}
