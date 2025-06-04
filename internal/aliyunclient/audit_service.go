// File: internal/aliyunclient/audit_service.go
package aliyunclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	imageaudit "github.com/alibabacloud-go/imageaudit-20191230/v3/client"
	util "github.com/alibabacloud-go/tea-utils/v2/service"
	"github.com/alibabacloud-go/tea/tea"

	"github.com/Xushengqwer/go-common/core"
	"github.com/Xushengqwer/post_audit/internal/config"
	// 导入新的通用审核平台模型和接口包
	"github.com/Xushengqwer/post_audit/internal/auditplatform" // 确保路径正确
	"go.uber.org/zap"
)

// AliyunAuditClient 封装了与阿里云内容安全服务的交互逻辑
type AliyunAuditClient struct {
	client     *imageaudit.Client
	logger     *core.ZapLogger
	scanLabels []*imageaudit.ScanTextRequestLabels // 从配置中获取的审核标签 (用于请求)
	apiTimeout time.Duration
}

// NewAliyunAuditClient 创建一个新的 AliyunAuditClient 实例
func NewAliyunAuditClient(cfg config.AliyunConfig, logger *core.ZapLogger) (*AliyunAuditClient, error) {
	if cfg.AccessKeyID == "" || cfg.AccessKeySecret == "" {
		logger.Error("阿里云内容安全配置不完整: AccessKeyID 或 AccessKeySecret 为空")
		return nil, fmt.Errorf("阿里云配置不完整: AccessKeyID 和 AccessKeySecret 不能为空")
	}
	if cfg.Endpoint == "" {
		logger.Error("阿里云内容安全配置不完整: Endpoint 为空")
		return nil, fmt.Errorf("阿里云配置不完整: Endpoint 不能为空")
	}
	if len(cfg.Scenes) == 0 { // Scenes 对应阿里云的 Labels
		logger.Error("阿里云内容安全配置不完整: Scenes (审核标签/场景列表) 为空。")
		return nil, fmt.Errorf("阿里云配置不完整: Scenes 不能为空")
	}

	openapiConfig := &openapi.Config{
		AccessKeyId:     tea.String(cfg.AccessKeyID),
		AccessKeySecret: tea.String(cfg.AccessKeySecret),
		Endpoint:        tea.String(cfg.Endpoint),
	}

	client, err := imageaudit.NewClient(openapiConfig)
	if err != nil {
		logger.Error("创建阿里云 imageaudit 客户端失败 (NewClient)", zap.Error(err))
		return nil, fmt.Errorf("创建阿里云 imageaudit 客户端失败: %w", err)
	}

	var scanLabels []*imageaudit.ScanTextRequestLabels
	for _, sceneLabel := range cfg.Scenes { // cfg.Scenes 就是我们要请求阿里云审核的 Labels
		scanLabels = append(scanLabels, &imageaudit.ScanTextRequestLabels{
			Label: tea.String(sceneLabel),
		})
	}

	logger.Info("阿里云 imageaudit 客户端初始化成功",
		zap.String("endpoint", cfg.Endpoint),
		zap.Int("请求的审核标签数量", len(scanLabels)),
	)

	return &AliyunAuditClient{
		client:     client,
		logger:     logger,
		scanLabels: scanLabels,
		apiTimeout: time.Duration(cfg.TimeoutMs) * time.Millisecond,
	}, nil
}

// GetPlatformName 实现 auditplatform.ContentReviewer 接口
func (c *AliyunAuditClient) GetPlatformName() string {
	return "aliyun"
}

// ReviewTextBatch 实现 auditplatform.ContentReviewer 接口。
func (c *AliyunAuditClient) ReviewTextBatch(ctx context.Context, tasks []auditplatform.TaskData) ([]auditplatform.ReviewResult, error) {
	if len(tasks) == 0 {
		c.logger.Info("ReviewTextBatch (Aliyun): 任务列表为空，无需调用阿里云")
		return []auditplatform.ReviewResult{}, nil
	}

	c.logger.Info("ReviewTextBatch (Aliyun): 开始调用阿里云进行批量文本内容审核 (ScanText API)",
		zap.Int("任务数量(task_count)", len(tasks)),
	)

	// 1. 将通用的 []auditplatform.TaskData 转换为阿里云的 []*imageaudit.ScanTextRequestTasks
	apiTasks := make([]*imageaudit.ScanTextRequestTasks, 0, len(tasks))
	for i, task := range tasks {
		c.logger.Debug("ReviewTextBatch (Aliyun): 准备批量审核任务",
			zap.Int("任务索引(task_index)", i),
			zap.String("data_id", task.ID),
			zap.Int("内容长度(content_length)", len(task.Content)),
		)
		apiTasks = append(apiTasks, &imageaudit.ScanTextRequestTasks{
			Content: tea.String(task.Content),
			// 阿里云的 ScanText API 的 Task 结构中没有直接的 DataId 字段。
			// 我们需要依赖响应 Elements 的顺序，并使用返回的 TaskId。
		})
	}

	// 2. 创建 ScanTextRequest 请求对象
	scanTextRequest := &imageaudit.ScanTextRequest{
		Tasks:  apiTasks,
		Labels: c.scanLabels, // 使用初始化时根据 cfg.Scenes 生成的 scanLabels
	}

	// 3. 设置运行时选项
	runtimeOptions := &util.RuntimeOptions{}
	if c.apiTimeout > 0 {
		runtimeOptions.ReadTimeout = tea.Int(int(c.apiTimeout.Milliseconds()))
		runtimeOptions.ConnectTimeout = tea.Int(int(c.apiTimeout.Milliseconds()))
	}

	c.logger.Debug("ReviewTextBatch (Aliyun): 发送给阿里云的批量 ScanText 请求",
		zap.Int("任务数量(task_count)", len(scanTextRequest.Tasks)),
		zap.Any("labels_to_scan", scanTextRequest.Labels), // 日志中明确是请求的标签
		zap.Int64("configured_timeout_ms", c.apiTimeout.Milliseconds()),
	)

	// 4. 发起API调用
	response, err := c.client.ScanTextWithOptions(scanTextRequest, runtimeOptions)

	if err != nil {
		c.logger.Error("ReviewTextBatch (Aliyun): 调用阿里云批量 ScanText API 失败", zap.Error(err))
		var sdkErr *tea.SDKError
		if errors.As(err, &sdkErr) && tea.StringValue(sdkErr.Code) == "Throttling" {
			//  auditplatform.ErrReviewThrottled (如果定义了)
			return nil, fmt.Errorf("阿里云API限流: %w", err)
		}
		return nil, fmt.Errorf("调用阿里云批量 ScanText API 失败: %w", err)
	}

	// 5. 处理响应
	if response == nil || response.Body == nil {
		c.logger.Error("ReviewTextBatch (Aliyun): 阿里云批量 ScanText API 返回了空的响应体")
		return nil, errors.New("阿里云批量 ScanText API 返回了空的响应体")
	}

	requestId := tea.StringValue(response.Body.RequestId)
	responseData := response.Body.Data

	c.logger.Info("ReviewTextBatch (Aliyun): 收到阿里云批量 ScanText API 响应",
		zap.Int32("http_status_code", tea.Int32Value(response.StatusCode)),
		zap.String("request_id", requestId),
	)

	if tea.Int32Value(response.StatusCode) != 200 {
		errMsg := fmt.Sprintf("阿里云批量 ScanText API 返回非200 HTTP状态: Code %d, RequestID: %s",
			tea.Int32Value(response.StatusCode), requestId)
		c.logger.Error(errMsg)
		return nil, errors.New(errMsg)
	}

	batchResults := make([]auditplatform.ReviewResult, len(tasks))

	if responseData == nil || responseData.Elements == nil || len(responseData.Elements) == 0 {
		c.logger.Warn("ReviewTextBatch (Aliyun): API 返回成功，但结果数据 (Data 或 Elements) 为空或数量为0。所有任务将默认为'pass'。",
			zap.String("request_id", requestId))
		for i, originalTask := range tasks {
			batchResults[i] = auditplatform.ReviewResult{
				OriginalTaskID: originalTask.ID,
				Suggestion:     "pass",
			}
		}
		return batchResults, nil
	}

	if len(responseData.Elements) != len(tasks) {
		errMsg := fmt.Sprintf("阿里云批量 ScanText API 返回的 Elements 数量 (%d) 与发送的 Tasks 数量 (%d) 不匹配, RequestID: %s",
			len(responseData.Elements), len(tasks), requestId)
		c.logger.Error(errMsg)
		return nil, errors.New(errMsg) // 顶层错误，因为无法安全匹配结果
	}

	for i, elementResult := range responseData.Elements { // elementResult 是 *imageaudit.ScanTextResponseBodyDataElements
		originalTask := tasks[i]
		currentResult := auditplatform.ReviewResult{
			OriginalTaskID: originalTask.ID,
			ProviderTaskID: tea.StringValue(elementResult.TaskId), // 阿里云的任务ID
		}

		if elementResult == nil {
			c.logger.Warn("ReviewTextBatch (Aliyun): Elements 数组中某个元素为空",
				zap.Int("任务索引(element_index)", i),
				zap.String("原始TaskID(original_task_id)", originalTask.ID),
				zap.String("request_id", requestId))
			currentResult.Suggestion = "review" // 标记为需要人工复审
			currentResult.Error = errors.New("阿里云返回的对应任务结果为空")
			batchResults[i] = currentResult
			continue
		}

		// 默认建议为 "pass"
		finalSuggestionForTask := "pass"
		var foundBlockForTask bool // 用于确保 block 建议优先于 review
		var detailsForTask []auditplatform.RejectionDetail

		// elementResult.Results 是 []*imageaudit.ScanTextResponseBodyDataElementsResults
		if elementResult.Results != nil && len(elementResult.Results) > 0 {
			for _, aliyunLabelResult := range elementResult.Results { // aliyunLabelResult 是 *imageaudit.ScanTextResponseBodyDataElementsResults
				if aliyunLabelResult == nil {
					continue
				}

				itemSuggestion := strings.ToLower(tea.StringValue(aliyunLabelResult.Suggestion))
				itemMainLabel := tea.StringValue(aliyunLabelResult.Label) // 这是阿里云返回的顶层Label, e.g., "spam", "porn"
				itemRate := float64(tea.Float32Value(aliyunLabelResult.Rate))

				// 更新整体建议
				if itemSuggestion == "block" {
					finalSuggestionForTask = "block"
					foundBlockForTask = true
				} else if itemSuggestion == "review" && !foundBlockForTask {
					finalSuggestionForTask = "review"
				}

				// 仅当阿里云建议非 "pass" 时，才记录为风险详情
				if itemSuggestion != "pass" {
					var matchedContents []string
					// aliyunLabelResult.Details 是 []*imageaudit.ScanTextResponseBodyDataElementsResultsDetails
					if aliyunLabelResult.Details != nil {
						for _, aliyunDetailItem := range aliyunLabelResult.Details {
							if aliyunDetailItem != nil {
								// 注意：根据您提供的阿里云 ScanTextResponseBodyDataElementsResults 结构，
								// 其 Details 内部不再有更细的 Label 字段，主要是 Contexts。
								// 所以我们的 auditplatform.RejectionDetail.Label 将使用 itemMainLabel。
								if aliyunDetailItem.Contexts != nil {
									for _, contextObj := range aliyunDetailItem.Contexts {
										if contextObj != nil && contextObj.Context != nil {
											matchedContents = append(matchedContents, tea.StringValue(contextObj.Context))
										}
									}
								}
							}
						}
					}

					detail := auditplatform.RejectionDetail{
						Label:          itemMainLabel, // 使用阿里云此结果条目的Label
						Suggestion:     itemSuggestion,
						Score:          itemRate,
						MatchedContent: matchedContents,
					}
					detailsForTask = append(detailsForTask, detail)
				}
			}
		}
		currentResult.Suggestion = finalSuggestionForTask
		currentResult.Details = detailsForTask
		batchResults[i] = currentResult
	}

	c.logger.Info("ReviewTextBatch (Aliyun): 阿里云批量文本审核完成 (ScanText API)",
		zap.Int("处理的任务数量(processed_task_count)", len(batchResults)),
		zap.String("request_id", requestId),
	)

	return batchResults, nil
}

// 确保 AliyunAuditClient 实现了 ContentReviewer 接口
var _ auditplatform.ContentReviewer = (*AliyunAuditClient)(nil)
