package longterm

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"CodeAgent/internal/llm"
)

func nilReader(reader llm.StreamReader) bool {
	if reader == nil {
		return true
	}
	v := reflect.ValueOf(reader)
	switch v.Kind() {
	case reflect.Pointer, reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func (s *Service) respond(ctx context.Context, request llm.Request, remaining int) (llm.AssistantMessage, error) {
	var latest llm.AssistantMessage
	if err := ctx.Err(); err != nil {
		return latest, err
	}
	reader, err := s.options.Stream(ctx, request)
	if nilReader(reader) {
		if cause := ctx.Err(); cause != nil {
			return latest, cause
		}
		if err == nil {
			err = fmt.Errorf("longterm: nil reader")
		}
		return latest, err
	}
	var once sync.Once
	closeReader := func() { once.Do(reader.Close) }
	defer closeReader()
	if cause := ctx.Err(); cause != nil {
		return latest, cause
	}
	if err != nil {
		return latest, err
	}
	stop := context.AfterFunc(ctx, closeReader)
	defer stop()
	streamed := 0
	for {
		event, ok := reader.Next(ctx)
		if !ok {
			break
		}
		for _, snapshot := range []*llm.AssistantMessage{event.Snapshot, event.FinalMessage} {
			if snapshot == nil {
				continue
			}
			latest = *snapshot
			encoded, err := json.Marshal(snapshot.Content)
			if err != nil {
				return latest, err
			}
			if len(encoded) > remaining {
				return latest, fmt.Errorf("%w: stream snapshot", ErrBudget)
			}
		}
		if len(event.Delta) > remaining-streamed {
			return latest, fmt.Errorf("%w: stream delta", ErrBudget)
		}
		streamed += len(event.Delta)
		if err := ctx.Err(); err != nil {
			return latest, err
		}
	}
	if err := ctx.Err(); err != nil {
		return latest, err
	}
	result := reader.Result()
	if err := ctx.Err(); err != nil {
		return result.Message, err
	}
	return result.Message, result.InternalErr
}

func sumUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		Input: a.Input + b.Input, Output: a.Output + b.Output, CacheRead: a.CacheRead + b.CacheRead, CacheWrite: a.CacheWrite + b.CacheWrite, Reasoning: a.Reasoning + b.Reasoning, Total: a.Total + b.Total,
		Cost: llm.Cost{Input: a.Cost.Input + b.Cost.Input, Output: a.Cost.Output + b.Cost.Output, CacheRead: a.Cost.CacheRead + b.Cost.CacheRead, CacheWrite: a.Cost.CacheWrite + b.Cost.CacheWrite, Total: a.Cost.Total + b.Cost.Total},
	}
}
