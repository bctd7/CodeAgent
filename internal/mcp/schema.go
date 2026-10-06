package mcp

import "encoding/json"

// SDK 在 TTL 过期后仍参考缓存 schema；受管理调用拒绝此扩展，
// 包括当前连接中曾使用此扩展的工具名。
func schemaHasHeader(schema any) (bool, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return false, err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return false, err
	}
	return containsHeader(value), nil
}

func containsHeader(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "x-mcp-header" || containsHeader(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if containsHeader(item) {
				return true
			}
		}
	}
	return false
}
