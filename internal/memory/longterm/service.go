package longterm

import (
	"context"
	"fmt"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/memory/longterm/storage"
)

type Service struct {
	store   *storage.Store
	options Options
}

// List、Read、Search 只访问已提交内容；不调用 Execute，不创建整理批次。
func (s *Service) List(ctx context.Context) ([]string, error) { return s.store.List(ctx) }
func (s *Service) Read(ctx context.Context, name string) (Document, error) {
	return s.store.Read(ctx, name)
}
func (s *Service) Search(ctx context.Context, query string, limit int) ([]Match, error) {
	return s.store.Search(ctx, query, limit)
}

// ReadTools 返回独立的三工具目录，不包含写权限，不依赖 Agent。
func (s *Service) ReadTools() []llm.Tool { return definitionsCatalog(toolDefinitions[:3]) }

// CallReadTool 与后台批次没有共享的暂存、调用计数或失败状态。
func (s *Service) CallReadTool(ctx context.Context, call llm.ContentBlock) (*llm.ToolResultMessage, error) {
	if ctx == nil {
		return nil, fmt.Errorf("longterm: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if call.Type != llm.TypeToolCall || !storage.ValidID(call.ID, 256) {
		return nil, fmt.Errorf("longterm: invalid call")
	}
	args, err := decodeToolArguments(call, toolDefinitions[:3])
	if err != nil {
		return nil, err
	}
	var value any
	switch call.Name {
	case "memory_list":
		value, err = s.List(ctx)
	case "memory_read":
		value, err = s.Read(ctx, args.Path)
	case "memory_search":
		value, err = s.Search(ctx, args.Query, args.Limit)
	}
	if err != nil {
		return nil, fmt.Errorf("longterm: %s: %w", call.Name, err)
	}
	return toolResult(call, value)
}

// NewService 创建长期记忆服务；存储层在实际数据操作时加锁并恢复提交。
func NewService(directory string, options Options) (*Service, error) {
	limits, err := defaultLimits(options.Limits)
	if err != nil {
		return nil, err
	}
	options.Limits = limits
	store, err := storage.New(directory, limits.Storage)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, options: options}, nil
}

func (s *Service) Consolidate(ctx context.Context, batch Batch) (ConsolidationResult, error) {
	var result ConsolidationResult
	sources, sourceIDs, err := messageClean(ctx, batch.Sources)
	if err != nil {
		return result, err
	}
	stored, err := s.store.UpdateBatch(ctx, batch.ID, func(tx *storage.Transaction) error {
		if s.options.Execute == nil {
			return ErrNoExecutor
		}
		names, err := tx.Files()
		if err != nil {
			return err
		}
		index, err := tx.ReadCommittedIndex()
		if err != nil {
			return err
		}
		task, err := requestPrepare(index, names, sources, s.options.Limits.Execution)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		tools := &batchTools{ctx: ctx, transaction: tx, callLimit: task.ExecutionLimits.ToolCalls, trusted: sourceIDs,
			evidence: &readEvidence{files: map[string]bool{}}, seenCalls: map[string]bool{}}
		task.CallTool = tools.call
		execution, executeErr := s.options.Execute(ctx, task)
		result.Execution, result.ToolCalls = execution, tools.calls
		if err := ctx.Err(); err != nil {
			return err
		}
		if executeErr != nil {
			return executeErr
		}
		if tools.failure != nil {
			return tools.failure
		}
		if execution.StopReason != llm.StopStop {
			return fmt.Errorf("longterm: invalid task stop %q", execution.StopReason)
		}
		if execution.Turns <= 0 || !validUsage(execution.Usage) {
			return fmt.Errorf("longterm: invalid task statistics")
		}
		if execution.Turns > task.ExecutionLimits.Turns {
			return fmt.Errorf("%w: model turns", ErrBudget)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Changed, result.Duplicate = stored.Changed, stored.Duplicate
	return result, nil
}

// Close 不与操作并发，释放存储对象持有的目录句柄。
func (s *Service) Close() error { return s.store.Close() }

func validUsage(usage llm.Usage) bool {
	for _, count := range []int64{usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite, usage.Reasoning, usage.Total} {
		if count < 0 {
			return false
		}
	}
	for _, cost := range []float64{usage.Cost.Input, usage.Cost.Output, usage.Cost.CacheRead, usage.Cost.CacheWrite, usage.Cost.Total} {
		if cost < 0 {
			return false
		}
	}
	return true
}
