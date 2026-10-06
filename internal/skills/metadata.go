package skills

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const maxFrontmatterBytes = 64 * 1024

var validName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// readFrontmatter 读取有界头部，正文不进入元数据结果。
func readFrontmatter(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := bufio.NewReader(io.LimitReader(f, maxFrontmatterBytes+1))
	opening, err := reader.ReadString('\n')
	if err != nil || strings.TrimPrefix(trimLineEnding(opening), "\uFEFF") != "---" {
		return nil, fmt.Errorf("文件必须以 YAML frontmatter 分隔行 --- 开始")
	}
	consumed := len(opening)
	var header strings.Builder
	for {
		raw, err := reader.ReadString('\n')
		consumed += len(raw)
		// LimitReader 的人工 EOF 不能把被截断的 ---xxx 误识别为结束行。
		if consumed > maxFrontmatterBytes {
			return nil, fmt.Errorf("frontmatter 超过 64 KiB")
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		line := trimLineEnding(raw)
		if line == "---" {
			if !utf8.ValidString(header.String()) {
				return nil, fmt.Errorf("frontmatter 必须为 UTF-8")
			}
			return []byte(header.String()), nil
		}
		header.WriteString(line)
		header.WriteByte('\n')
		if err == io.EOF {
			break
		}
	}
	return nil, fmt.Errorf("frontmatter 缺少结束分隔行")
}

// loadMetadata 从文件读取有界头部，解析并校验技能元数据。
func loadMetadata(path string) (Skill, error) {
	skill := Skill{FilePath: path, BaseDir: filepath.Dir(path)}
	data, err := readFrontmatter(path)
	if err != nil {
		return skill, err
	}
	var fields map[string]any
	if err := yaml.Unmarshal(data, &fields); err != nil {
		return skill, fmt.Errorf("YAML: %w", err)
	}
	for _, field := range []struct {
		key    string
		target *string
	}{
		{"name", &skill.Name}, {"description", &skill.Description},
		{"license", &skill.License}, {"compatibility", &skill.Compatibility}, {"allowed-tools", &skill.AllowedTools},
	} {
		key, target := field.key, field.target
		if value, exists := fields[key]; exists {
			text, ok := value.(string)
			if !ok {
				return skill, fmt.Errorf("%s 必须是字符串", key)
			}
			*target = text
		}
	}
	if skill.Name == "" {
		// 与 Pi 兼容：仅缺失或空字符串名称使用所属目录名，类型错误仍拒绝。
		skill.Name = filepath.Base(skill.BaseDir)
	}
	if !validName.MatchString(skill.Name) || len(skill.Name) > 64 {
		return skill, fmt.Errorf("name 必须是 1–64 个小写 ASCII 字母、数字或单个分隔连字符")
	}
	if strings.TrimSpace(skill.Description) == "" || utf8.RuneCountInString(skill.Description) > 1024 {
		return skill, fmt.Errorf("description 必须非空且不超过 1024 个字符")
	}
	if _, exists := fields["compatibility"]; exists && (strings.TrimSpace(skill.Compatibility) == "" || utf8.RuneCountInString(skill.Compatibility) > 500) {
		return skill, fmt.Errorf("compatibility 必须非空且不超过 500 个字符")
	}
	if value, exists := fields["disable-model-invocation"]; exists {
		flag, ok := value.(bool)
		if !ok {
			return skill, fmt.Errorf("disable-model-invocation 必须是布尔值")
		}
		skill.DisableModelInvocation = flag
	}
	if value, exists := fields["metadata"]; exists {
		mapping, ok := value.(map[string]any)
		if !ok {
			return skill, fmt.Errorf("metadata 必须是字符串到字符串的映射")
		}
		skill.Metadata = make(map[string]string, len(mapping))
		keys := make([]string, 0, len(mapping))
		for key := range mapping {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := mapping[key]
			text, ok := value.(string)
			if !ok {
				return skill, fmt.Errorf("metadata.%s 必须是字符串", key)
			}
			skill.Metadata[key] = text
		}
	}
	return skill, nil
}

func trimLineEnding(line string) string {
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}
