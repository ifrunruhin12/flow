package sync

import (
	"context"
	"errors"
	"sync"
)

var errExecutorClosed = errors.New("synchronization executor closed")

type execution struct {
	ctx    context.Context
	run    func(context.Context) (any, error)
	result chan executionResult
}

type executionResult struct {
	value any
	err   error
}

type executor struct {
	commands chan execution
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

func newExecutor() *executor {
	executor := &executor{commands: make(chan execution), stop: make(chan struct{}), done: make(chan struct{})}
	go executor.loop()
	return executor
}

func (e *executor) loop() {
	defer close(e.done)
	for {
		select {
		case command := <-e.commands:
			value, err := command.run(command.ctx)
			select {
			case command.result <- executionResult{value: value, err: err}:
			case <-command.ctx.Done():
			}
		case <-e.stop:
			return
		}
	}
}

func (e *executor) submit(ctx context.Context, run func(context.Context) (any, error)) (any, error) {
	command := execution{ctx: ctx, run: run, result: make(chan executionResult, 1)}
	select {
	case e.commands <- command:
	case <-e.stop:
		return nil, errExecutorClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case result := <-command.result:
		return result.value, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *executor) close() {
	e.once.Do(func() { close(e.stop); <-e.done })
}
