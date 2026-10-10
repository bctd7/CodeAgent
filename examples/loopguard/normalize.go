package loopguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

func normalizeEvent(event Event) (observation, error) {
	if err := validIdentifier("ToolName", event.ToolName, false); err != nil {
		return observation{}, err
	}
	if err := validIdentifier("SourceID", event.SourceID, true); err != nil {
		return observation{}, err
	}
	var status Status
	switch event.Result.Status {
	case Success:
		status = Success
	case Failure:
		status = Failure
	case Exception:
		status = Exception
	default:
		return observation{}, fmt.Errorf("loopguard: 未知结果状态")
	}
	arguments := event.Arguments
	if len(arguments) == 0 {
		arguments = []byte("{}")
	}
	canonical, err := canonicalJSON(arguments, true)
	if err != nil {
		return observation{}, fmt.Errorf("loopguard: 参数: %w", err)
	}
	data := event.Result.Data
	var format ResultFormat
	switch event.Result.Format {
	case TextResult:
		format = TextResult
		// 文本逐字节哈希，不做可能掩盖进展的截断、去空白或模糊错误匹配。
	case JSONResult:
		format = JSONResult
		data, err = canonicalJSON(data, false)
		if err != nil {
			return observation{}, fmt.Errorf("loopguard: JSON 结果: %w", err)
		}
	case FingerprintResult:
		format = FingerprintResult
		if len(data) == 0 || len(data) > MaxIdentifierBytes || !utf8.Valid(data) || len(bytes.TrimSpace(data)) == 0 {
			return observation{}, fmt.Errorf("loopguard: 结果指纹必须为 1..%d 字节的非空白 UTF-8", MaxIdentifierBytes)
		}
	default:
		return observation{}, fmt.Errorf("loopguard: 未知结果格式")
	}
	return observation{
		key: outcome{
			operation: operation{toolName: strings.Clone(event.ToolName), argumentsHash: sha256.Sum256(canonical)},
			status:    status, format: format, resultHash: sha256.Sum256(data),
		},
		sourceID: strings.Clone(event.SourceID), allowRepeat: event.AllowRepeat,
	}, nil
}

func validIdentifier(field, value string, allowEmpty bool) error {
	if allowEmpty && value == "" {
		return nil
	}
	if len(value) > MaxIdentifierBytes || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return fmt.Errorf("loopguard: %s 必须为 1..%d 字节的非空白 UTF-8", field, MaxIdentifierBytes)
	}
	return nil
}

// 参数与 JSON 结果共用同一个规范化函数。UseNumber 保留数字原文，Marshal 稳定排序键。
// 保守保留数字写法：1、1.0、1e0 不合并；不会通过 float64 损失大整数精度。
func canonicalJSON(data []byte, objectOnly bool) ([]byte, error) {
	if len(data) > MaxJSONBytes || !utf8.Valid(data) {
		return nil, fmt.Errorf("JSON 必须为有效 UTF-8 且不超过 %d 字节；大结果请提供稳定指纹", MaxJSONBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("JSON 必须恰好包含一个值")
	}
	if objectOnly {
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("工具参数必须是 JSON 对象")
		}
	}
	return json.Marshal(value)
}

// 使用标准库解析 token，仅额外拒绝重复键和过深嵌套，避免含糊输入被静默合并。
func readJSONValue(decoder *json.Decoder, depth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	if depth >= MaxJSONDepth {
		return nil, fmt.Errorf("JSON 嵌套不能超过 %d 层", MaxJSONDepth)
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := token.(string)
			if !ok {
				return nil, fmt.Errorf("JSON 对象键必须是字符串")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("JSON 对象包含重复键")
			}
			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		_, err := decoder.Token() // 标准库校验匹配的闭括号。
		return object, err
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err := decoder.Token()
		return array, err
	default:
		return nil, fmt.Errorf("JSON 出现意外的闭括号")
	}
}
