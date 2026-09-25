package openai

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

var sseDoneMarker = []byte("[DONE]")

// readSSEFrames 读取响应体流，将每个 SSE 帧的 data 行拼接后交给 onFrameData。
// SSE 帧是网络数据边界，与 llm.Event 内部通知不同。
// 它不理解 JSON；遇到 [DONE] 就停止。
func readSSEFrames(reader io.Reader, maxFrameSize int, onFrameData func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	// 初始缓冲长度，超过扩容，最大长度 maxFrameSize
	scanner.Buffer(make([]byte, 4<<10), maxFrameSize)

	var dataLines [][]byte // 一个 SSE 帧可能有多个 data 行，先收集
	var frameSize int

	// flush 把当前 SSE 帧的 data 行拼成完整负载，交给 onFrameData。
	// 返回 done=true 表示遇到 [DONE]（流结束），调用方要停止。
	flush := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		payload := bytes.Join(dataLines, []byte("\n"))
		dataLines = nil
		frameSize = 0

		if bytes.Equal(bytes.TrimSpace(payload), sseDoneMarker) {
			return true, nil // 遇到 [DONE]，结束
		}
		if len(bytes.TrimSpace(payload)) == 0 {
			return false, nil // 空负载，跳过
		}
		return false, onFrameData(payload)
	}

	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte("\r"))
		if len(line) == 0 {
			// 空行表示当前 SSE 帧结束，不表示整条模型消息结束。
			done, err := flush()
			if err != nil || done {
				return err
			}
			continue
		}
		if line[0] == ':' {
			// 注释行跳过
			continue
		}
		left, right, found := bytes.Cut(line, []byte(":"))
		if !found || !bytes.Equal(left, []byte("data")) {
			continue
		}
		if len(right) > 0 && right[0] == ' ' {
			right = right[1:] // 去掉冒号后的一个空格
		}
		frameSize += len(right)
		if frameSize > maxFrameSize {
			return fmt.Errorf("event too large: %d > %d", frameSize, maxFrameSize)
		}
		dataLines = append(dataLines, append([]byte{}, right...))
	}

	// 收尾：先检查扫描错误，再 flush 残留数据
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read data: %w", err)
	}
	if _, err := flush(); err != nil {
		return err
	}
	return nil
}
