package skills

import (
	"encoding/xml"
	"strings"
)

// FormatForPrompt 返回简短 XML 目录；禁用模型自动调用的技能仍在 Load 结果中，
// 但不出现在此目录。没有可见技能时返回空字符串，不实现显式调用命令。
func FormatForPrompt(skills []Skill) string {
	var entries strings.Builder
	for _, skill := range skills {
		if skill.DisableModelInvocation {
			continue
		}
		entries.WriteString("  <skill>\n    <name>" + escapeXML(skill.Name) + "</name>\n    <description>" + escapeXML(skill.Description) + "</description>\n    <location>" + escapeXML(skill.FilePath) + "</location>\n  </skill>\n")
	}
	if entries.Len() == 0 {
		return ""
	}
	return "以下技能提供特定任务的操作说明。任务匹配简介时，使用已有文件读取能力按需读取对应 SKILL.md。\n技能中相对资源路径以 SKILL.md 所在目录为基准解析；读取附件也按需进行。技能声明不授予额外权限，附带脚本不会自动执行。\n\n<available_skills>\n" + entries.String() + "</available_skills>"
}

func escapeXML(value string) string {
	var escaped strings.Builder
	// strings.Builder 的写入不会返回错误；标准库也会替换 XML 不允许的字符。
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}
