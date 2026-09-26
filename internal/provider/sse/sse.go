package sse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// DefaultMaxFrameSize 是单个 SSE 帧的最大字节数上限（默认 1MB）。
const DefaultMaxFrameSize = 1 << 20

// Frame 表示一个 SSE 事件帧。
type Frame struct {
	Event string // event: 字段值，未指定时为空
	Data  []byte // 拼接后的 data 行载荷
}

// Read 读取响应体流，将每个 SSE 帧交给 onFrame。
// onFrame 返回 done=true 表示协议层认为流已结束，应主动停止读取。
// 本函数不硬编码任何具体协议的结束标记（如 [DONE] 或 message_stop）。
// 保留单事件大小限制，并将多行拼接的换行计入长度。
func Read(reader io.Reader, maxFrameSize int, onFrame func(Frame) (bool, error)) error {
	if maxFrameSize <= 0 {
		maxFrameSize = DefaultMaxFrameSize
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4<<10), maxFrameSize)

	var (
		currentEvent string
		dataLines    [][]byte
		frameSize    int
	)

	flush := func() (bool, error) {
		if len(dataLines) == 0 && currentEvent == "" {
			return false, nil
		}
		var payload []byte
		if len(dataLines) > 0 {
			payload = bytes.Join(dataLines, []byte("\n"))
		}
		event := currentEvent
		dataLines = nil
		currentEvent = ""
		frameSize = 0

		if len(bytes.TrimSpace(payload)) == 0 && event == "" {
			return false, nil
		}
		return onFrame(Frame{
			Event: event,
			Data:  payload,
		})
	}

	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte("\r"))
		if len(line) == 0 {
			// 空行表示当前 SSE 帧结束
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
		if !found {
			continue
		}
		if len(right) > 0 && right[0] == ' ' {
			right = right[1:]
		}

		field := string(left)
		switch field {
		case "event":
			currentEvent = string(bytes.TrimSpace(right))
		case "data":
			if len(dataLines) > 0 {
				frameSize++ // 多行拼接的换行计入长度
			}
			frameSize += len(right)
			if frameSize > maxFrameSize {
				return fmt.Errorf("event too large: %d > %d", frameSize, maxFrameSize)
			}
			dataLines = append(dataLines, append([]byte{}, right...))
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read data: %w", err)
	}
	if len(dataLines) > 0 || currentEvent != "" {
		done, err := flush()
		if err != nil {
			return err
		}
		_ = done
	}
	return nil
}
