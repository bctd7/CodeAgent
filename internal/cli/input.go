package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
)

const inputLimit = 1 << 20

type inputResult struct {
	text string
	err  error
}
type inputRequest struct {
	all   bool
	reply chan inputResult
}
type inputReader struct {
	requests   chan inputRequest
	done       chan struct{}
	once       sync.Once
	closeInput func() error
	pending    chan inputResult
}

// Reads are requested, never prefetched: a chat prompt cannot steal approval input.
// An arbitrary io.Reader cannot be forcibly interrupted. Embedders owning a blocking
// reader should provide CloseInput; Main owns stdin for its process lifetime.
func newInput(streams IO) *inputReader {
	r := &inputReader{requests: make(chan inputRequest, 1), done: make(chan struct{}), closeInput: streams.CloseInput}
	go func() {
		scanner := bufio.NewScanner(streams.In)
		scanner.Buffer(make([]byte, 4096), inputLimit)
		for {
			select {
			case <-r.done:
				return
			case request := <-r.requests:
				var result inputResult
				if request.all {
					data, err := io.ReadAll(io.LimitReader(streams.In, inputLimit+1))
					result.text, result.err = string(data), err
					if len(data) > inputLimit {
						result.err = fmt.Errorf("stdin exceeds 1 MiB")
					}
				} else if scanner.Scan() {
					result.text = scanner.Text()
				} else {
					result.err = scanner.Err()
					if result.err == nil {
						result.err = io.EOF
					}
				}
				select {
				case request.reply <- result:
				case <-r.done:
					return
				}
			}
		}
	}()
	return r
}
func (r *inputReader) request(all bool) <-chan inputResult {
	if r.pending != nil {
		return r.pending
	}
	reply := make(chan inputResult, 1)
	r.pending = reply
	select {
	case r.requests <- inputRequest{all, reply}:
	case <-r.done:
		reply <- inputResult{err: io.EOF}
	}
	return reply
}
func (r *inputReader) consumed() { r.pending = nil }
func (r *inputReader) read(ctx context.Context, all bool) (string, error) {
	select {
	case result := <-r.request(all):
		r.consumed()
		return result.text, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func (r *inputReader) stop() {
	r.once.Do(func() {
		close(r.done)
		if r.closeInput != nil {
			_ = r.closeInput()
		}
	})
}
