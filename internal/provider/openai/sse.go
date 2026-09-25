package openai

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

var donePayload = []byte("[DONE]")

// readSSE 只负责 SSE 分帧：把 reader 里的 data 行拼成完整负载，交给 onData。
// 它不理解 JSON；遇到 [DONE] 就停止。
func readSSE(reader io.Reader, maxEventSize int, onData func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	// 初始缓冲长度，超过扩容，最大长度 maxEventSize
	scanner.Buffer(make([]byte, 4<<10), maxEventSize)

	var dataLines [][]byte // 一个事件可能有多个 data 行，先收集
	var curEventSize int

	// flush 把一个事件的所有 data 行拼成一个完整负载，交给 onData。
	// 返回 done=true 表示遇到 [DONE]（流结束），调用方要停止。
	flush := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		payload := bytes.Join(dataLines, []byte("\n"))
		dataLines = nil
		curEventSize = 0

		if bytes.Equal(bytes.TrimSpace(payload), donePayload) {
			return true, nil // 遇到 [DONE]，结束
		}
		if len(bytes.TrimSpace(payload)) == 0 {
			return false, nil // 空负载，跳过
		}
		return false, onData(payload)
	}

	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte("\r"))
		if len(line) == 0 {
			// 空行 = 一个事件结束
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
		curEventSize += len(right)
		if curEventSize > maxEventSize {
			return fmt.Errorf("event too large: %d > %d", curEventSize, maxEventSize)
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
