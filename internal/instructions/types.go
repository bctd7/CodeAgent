// Package instructions 独立加载全局及项目指令，只返回文档，不组装或修改模型提示词。
package instructions

import "errors"

const (
	DefaultFileName       = "AGENTS.md"
	DefaultMaxBytes int64 = 128 * 1024
)

var (
	ErrBudget         = errors.New("instructions: content budget exceeded")
	ErrOutsideProject = errors.New("instructions: target outside project root")
)

type Options struct {
	// FileName 是每个目录中查找的单个文件名；空值使用 DefaultFileName。
	FileName string
	// GlobalDir 是全局指令文件所在目录；空值表示不加载全局指令，目录可尚不存在。
	GlobalDir string
	// ProjectRoot 有值时必须是已有目录，是祖先查找及项目文件访问的边界。
	// 空值只加载显式 GlobalDir，不使用当前工作目录查找项目指令。
	ProjectRoot string
	// TargetDir 必须是已有目录；相对路径基于 ProjectRoot，空值使用项目根。
	// ProjectRoot 为空时 TargetDir 也必须为空。
	// Windows 不接受无盘符根路径（\child）或盘符相对路径（C:child）。
	TargetDir string
	// MaxBytes 限制本次实际读取的总字节数，包含 BOM 和失败读取的数据。
	// 零值使用 DefaultMaxBytes，负值无效；超限文件整份省略，不截断正文。
	// 省略后继续加载剩余能容纳的文件，同时通过 Diagnostics 和 ErrBudget 报告。
	MaxBytes int64
}

type Scope struct {
	// Global 为 true 时作用于所有项目，Directory 为空。
	Global bool
	// 项目指令作用于该规范化绝对目录及其子目录。
	Directory string
}

type Document struct {
	// Path 是发现并打开文件的绝对路径；同一物理文件保留最先成功加载的来源与作用范围。
	Path    string
	Scope   Scope
	Content string
}

// Diagnostic 表示省略的文件及原因。发现时缺失文件和重复来源是正常情况，不产生诊断。
type Diagnostic struct {
	Path  string
	Scope Scope
	Err   error
}

type Result struct {
	// Documents 顺序为全局、项目根到目标的祖先链；空文件也作为文档返回。
	Documents   []Document
	Diagnostics []Diagnostic
	BytesRead   int64
}
