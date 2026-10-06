package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load 按来源顺序发现并解析技能，保留首个有效名称和诊断顺序。
func Load(sources []Source) Result {
	var result Result
	names := make(map[string]string)
	var files []os.FileInfo
	for _, source := range sources {
		addDiagnostic := func(code, path, message string) {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: code, Path: path, Source: source, Message: message})
		}
		if strings.TrimSpace(source.Path) == "" {
			addDiagnostic("path", "", "技能来源目录不能为空")
			continue
		}
		absolute, err := filepath.Abs(source.Path)
		if err != nil {
			addDiagnostic("path", source.Path, err.Error())
			continue
		}
		source.Path = absolute
		root, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			addDiagnostic("path", absolute, err.Error())
			continue
		}
		discover(root, func(file string, info os.FileInfo) {
			for _, prior := range files {
				if os.SameFile(prior, info) {
					return
				}
			}
			// 文件链接不改变技能目录及相对资源基准。
			skill, err := loadMetadata(file)
			if err != nil {
				addDiagnostic("format", file, err.Error())
				return
			}
			files = append(files, info)
			skill.Source = source
			if directoryName := filepath.Base(skill.BaseDir); skill.Name != directoryName {
				addDiagnostic("warning", file, fmt.Sprintf("显式 name %q 与技能目录名 %q 不一致，保留显式名称", skill.Name, directoryName))
			}
			if winner, exists := names[skill.Name]; exists {
				result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "collision", Path: file, Source: source, Message: fmt.Sprintf("技能 %q 重名，保留首个来源", skill.Name), RelatedPath: winner})
				return
			}
			names[skill.Name] = file
			result.Skills = append(result.Skills, skill)
		}, addDiagnostic)
	}
	return result
}
