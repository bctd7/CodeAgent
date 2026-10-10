// 本文件负责共享 JSON 校验、解码与形状判断。
package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// decodeJSON 用于外部 JSON 入口。已经通过验证的嵌套数据
// 使用 unmarshalJSON，避免重复遍历键。
func decodeJSON(data []byte, dest any, strict bool) error {
	if err := validateJSON(data); err != nil {
		return err
	}
	return unmarshalJSON(data, dest, strict)
}

// unmarshalJSON 解码已验证数据，按 strict 决定是否拒绝未知字段。
func unmarshalJSON(data []byte, dest any, strict bool) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if strict {
		d.DisallowUnknownFields()
	}
	return d.Decode(dest)
}

// validateJSON 检查 UTF-8、重复对象键、嵌套深度及多余尾部内容。
func validateJSON(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("JSON must be UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 128 {
			return fmt.Errorf("JSON nesting exceeds 128")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				token, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || keys[key] {
					return fmt.Errorf("duplicate or invalid JSON key %v", token)
				}
				keys[key] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}

func isJSONNull(raw []byte) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func isJSONObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}

func isJSONArray(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '[' && json.Valid(raw)
}
