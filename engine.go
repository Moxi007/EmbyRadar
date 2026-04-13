package main

import "context"

// CognitionEngine 定义认知引擎的统一接口。
// CognitionServer 直接实现此接口（本地直调，零 HTTP 开销）。
// 所有消费者（ChatHandler、DigestScheduler、Autonomy 等）持有接口引用，
// 不再依赖具体实现细节。
type CognitionEngine interface {
	// Respond 处理一次完整的对话请求（含工具循环）
	Respond(ctx context.Context, req *RespondRequest) (*RespondResponse, error)

	// SendEvent 记录一个认知事件到事件日志
	SendEvent(ctx context.Context, event *CognitionEvent) error

	// TransformText 通用文本转换（知识格式化、摘要提炼、知识合并等）
	TransformText(ctx context.Context, path string, req *TextTransformRequest) (string, error)

	// ParseRequestIntent 从用户文本中提取影视求片意图
	ParseRequestIntent(ctx context.Context, llm LLMConfig, text string) (*RequestIntentResponse, error)

	// ClaimAction 领取一个待执行的计划任务（调度器调用）
	ClaimAction(ctx context.Context) (*PlannedAction, error)

	// SubmitActionResult 提交任务执行结果
	SubmitActionResult(ctx context.Context, actionID string, result *ActionResultRequest) error
}
