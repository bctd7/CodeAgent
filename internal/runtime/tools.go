package runtime

import (
	"context"
	"fmt"
	"path/filepath"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/tools/basic"
)

// assembleTools 从现有构造函数取得稳定定义；执行时使用首次绑定的目录。
// 未绑定时的占位路径仅用于构造说明和 Schema，不会用于执行。
func (r *Runtime) assembleTools() ([]agent.Tool, error) {
	if r.manager.config.Tools != nil {
		return r.manager.config.Tools, nil
	}
	root := filepath.VolumeName(r.manager.database) + string(filepath.Separator)
	definitions, err := basic.New(basic.Config{WorkingDir: root})
	if err != nil {
		return nil, err
	}
	for i := range definitions {
		name := definitions[i].Name
		definitions[i].Description += "\nExecution uses the session's bound working directory; an unbound session cannot execute this tool."
		definitions[i].Execute = func(ctx context.Context, callID string, args map[string]any, progress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			r.mu.Lock()
			dir := r.info.WorkingDir
			r.mu.Unlock()
			if dir == "" {
				return nil, fmt.Errorf("会话尚未绑定工作目录，不能执行 %s", name)
			}
			tools, err := basic.New(basic.Config{WorkingDir: dir})
			if err != nil {
				return nil, err
			}
			for _, t := range tools {
				if t.Name == name {
					return t.Execute(ctx, callID, args, progress)
				}
			}
			return nil, fmt.Errorf("基础工具 %s 不存在", name)
		}
	}
	return definitions, nil
}
