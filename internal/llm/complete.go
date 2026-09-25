package llm

import "context"

// Complete 是一次性同步调用的便利包装，内部通过 stream 建立流并等待最终结果。
//
// 调用说明：
// 调用方既要检查返回的 error（表示建流失败或内部状态错误），
// 也要检查返回消息的 State（例如 StateError 表示模型端或网络失败，StateAborted 表示超时或取消）。
// 即使 error 为 nil，消息也可能处于非成功状态，具体失败信息参见 ErrorMessage。
func Complete(ctx context.Context, request Request, stream func(context.Context, Request) (StreamReader, error)) (AssistantMessage, error) {
	eventStream, err := stream(ctx, request)
	if err != nil {
		return AssistantMessage{}, err
	}
	defer eventStream.Close()

	result := eventStream.Result()
	return result.Message, result.InternalErr
}
