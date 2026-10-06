// Package skills 发现技能元数据并生成按需读取目录，不执行技能或附带脚本。
package skills

// Source 是调用方显式指定的目录及来源标签；按切片顺序确定优先级。
// Path 相对于当前工作目录解释，不展开 ~ 或环境变量。
type Source struct {
	Path string
	Name string
}

// Skill 仅保存 frontmatter 和定位信息，正文及附件由已有文件读取能力按需读取。
type Skill struct {
	Name                   string
	Description            string
	FilePath               string
	BaseDir                string
	Source                 Source
	License                string
	Compatibility          string
	Metadata               map[string]string
	AllowedTools           string // 仅记录声明，不授予工具权限。
	DisableModelInvocation bool
}

// Diagnostic 记录发现、格式校验或冲突问题；冲突时 RelatedPath 指向保留项。
type Diagnostic struct {
	Code        string
	Path        string
	Source      Source
	Message     string
	RelatedPath string
}

// Result 保留成功项和诊断，使单个损坏技能不会阻断其他来源。
type Result struct {
	Skills      []Skill
	Diagnostics []Diagnostic
}
