package hystrix

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type runFunc func() error
type fallbackFunc func(error) error
type runFuncC func(context.Context) error
type fallbackFuncC func(context.Context, error) error

// CircuitError 是一种错误，它对各种执行失败状态进行建模，例如断路器的断开或超时。
type CircuitError struct {
	Message string
}

func (e CircuitError) Error() string {
	return "hystrix: " + e.Message
}

// command 对断路器中单个执行的状态进行建模。"hystrix command" 通常用于描述运行/回退函数与断路器的配对。
type command struct {
	sync.Mutex

	ticket      *struct{}
	start       time.Time
	errChan     chan error
	finished    chan bool
	circuit     *CircuitBreaker
	run         runFuncC
	fallback    fallbackFuncC
	runDuration time.Duration
	events      []string
}

var (
	// ErrMaxConcurrency 当同时执行太多相同命名的命令时发生。
	ErrMaxConcurrency = CircuitError{Message: "max concurrency"}
	// ErrCircuitOpen 当执行尝试“短路”时返回。这是由于电路被测量为不健康造成的。
	ErrCircuitOpen = CircuitError{Message: "circuit open"}
	// ErrTimeout 当提供的函数执行时间过长时发生。
	ErrTimeout = CircuitError{Message: "timeout"}
)

// Go 运行函数，同时跟踪以前对该函数的调用的运行状况。
// 如果你的功能开始放缓或反复失败，将会阻塞对它的新调用给依赖的服务时间进行修复。
//
// runC: 进行指标统计的待执行函数
// fallback: 在中断期间执行的代码段
func Go(name string, run runFunc, fallback fallbackFunc) chan error {
	runC := func(ctx context.Context) error {
		return run()
	}
	var fallbackC fallbackFuncC
	if fallback != nil {
		fallbackC = func(ctx context.Context, err error) error {
			return fallback(err)
		}
	}
	return GoC(context.Background(), name, runC, fallbackC)
}

// GoC 运行函数的同时跟踪以前对该函数的调用的运行状况。
// 如果您的函数开始变慢或反复失败，我们将阻止对它的新调用，以便给依赖的服务时间来修复。
//
// 如果希望定义一些代码在中断期间执行，则定义一个回退函数。
func GoC(ctx context.Context, name string, run runFuncC, fallback fallbackFuncC) chan error {
	cmd := &command{
		run:      run,
		fallback: fallback,
		start:    time.Now(),
		errChan:  make(chan error, 1),
		finished: make(chan bool, 1),
	}

	// 不要使用带有显式参数和返回的方法，让数据自然地输入和输出，
	// 就像任何闭包显式错误一样，返回给我们的位置来终止切换操作(回退)

	circuit, _, err := GetCircuit(name)
	if err != nil {
		cmd.errChan <- err
		return cmd.errChan
	}
	cmd.circuit = circuit
	ticketCond := sync.NewCond(cmd)
	ticketChecked := false
	// 当调用者从返回的errChan中提取错误时，假定票据已经返回到executorPool。
	// 因此，returnTicket()不能在cmd.errorWithFallback()之后运行。
	// 需要先执行 returnTicket() 之后再调用 cmd.errorWithFallback()
	returnTicket := func() {
		cmd.Lock()
		// 避免在获得一张票之前释放。
		// 在状态未标记为未检查状态之前 票据 一直为等待状态。
		for !ticketChecked {
			ticketCond.Wait()
		}
		cmd.circuit.executorPool.Return(cmd.ticket)
		cmd.Unlock()
	}
	// 由以下两个goroutines共享。它确保只有较快的goroutine运行errWithFallback()和reportAllEvent()。
	returnOnce := &sync.Once{}
	reportAllEvent := func() {
		err := cmd.circuit.ReportEvent(cmd.events, cmd.start, cmd.runDuration)
		if err != nil {
			log.Printf(err.Error())
		}
	}

	go func() {
		defer func() { cmd.finished <- true }()

		// 判断断路器是否被打开？
		if !cmd.circuit.AllowRequest() {
			// 断路器打开状态，执行失败
			cmd.Lock()
			// 让另一个 goroutine 继续发放 nil ticket 是安全的。
			ticketChecked = true
			ticketCond.Signal()
			cmd.Unlock()
			returnOnce.Do(func() {
				// 返回令牌到令牌桶
				returnTicket()
				// 失败执行 fallback函数
				cmd.errorWithFallback(ctx, ErrCircuitOpen)
				// 报告失败的事件
				reportAllEvent()
			})
			return
		}

		// 由于后端不稳定，请求需要更长的时间，但并不总是失败。
		//
		// 当请求变慢但传入请求的速率保持不变时，您必须一次运行更多的请求才能跟上。
		// 通过在这些情况下控制并发性，您可以减少由于活动命令与传入请求的比例不断增加而累积的负载。
		cmd.Lock()
		select {
		case cmd.ticket = <-circuit.executorPool.Tickets:
			// 能拿到令牌
			ticketChecked = true
			ticketCond.Signal()
			cmd.Unlock()
		default:
			// 拿不到令牌，报告错误并返回令牌，报告错误
			ticketChecked = true
			ticketCond.Signal()
			cmd.Unlock()
			returnOnce.Do(func() {
				returnTicket()
				cmd.errorWithFallback(ctx, ErrMaxConcurrency)
				reportAllEvent()
			})
			return
		}

		// 执行 run 函数并记录执行时长
		runStart := time.Now()
		runErr := run(ctx)
		returnOnce.Do(func() {
			defer reportAllEvent()
			cmd.runDuration = time.Since(runStart)
			returnTicket()
			// run 函数报错则返回错误报告
			if runErr != nil {
				cmd.errorWithFallback(ctx, runErr)
				return
			}
			// 无异常则返回执行成功
			cmd.reportEvent("success")
		})
	}()

	go func() {
		timer := time.NewTimer(getSettings(name).Timeout)
		defer timer.Stop()

		select {
		case <-cmd.finished:
			// returnOnce 已经在另一个goroutine中执行
		case <-ctx.Done():
			// 上下文退出返回错误
			returnOnce.Do(func() {
				returnTicket()
				cmd.errorWithFallback(ctx, ctx.Err())
				reportAllEvent()
			})
			return
		case <-timer.C:
			// 执行超时
			returnOnce.Do(func() {
				returnTicket()
				cmd.errorWithFallback(ctx, ErrTimeout)
				reportAllEvent()
			})
			return
		}
	}()

	return cmd.errChan
}

// Do 以同步的方式运行你的函数，直到你的函数成功或返回一个错误，包括hystrix电路错误
func Do(name string, run runFunc, fallback fallbackFunc) error {
	runC := func(ctx context.Context) error {
		return run()
	}
	var fallbackC fallbackFuncC
	if fallback != nil {
		fallbackC = func(ctx context.Context, err error) error {
			return fallback(err)
		}
	}
	return DoC(context.Background(), name, runC, fallbackC)
}

// DoC 以同步的方式运行你的函数，直到你的函数成功或返回一个错误，包括hystrix电路错误
func DoC(ctx context.Context, name string, run runFuncC, fallback fallbackFuncC) error {
	done := make(chan struct{}, 1)

	r := func(ctx context.Context) error {
		err := run(ctx)
		if err != nil {
			return err
		}

		done <- struct{}{}
		return nil
	}

	f := func(ctx context.Context, e error) error {
		err := fallback(ctx, e)
		if err != nil {
			return err
		}

		done <- struct{}{}
		return nil
	}

	var errChan chan error
	if fallback == nil {
		errChan = GoC(ctx, name, r, nil)
	} else {
		errChan = GoC(ctx, name, r, f)
	}

	select {
	case <-done:
		return nil
	case err := <-errChan:
		return err
	}
}

// reportEvent 记录统计指标事件
func (c *command) reportEvent(eventType string) {
	c.Lock()
	defer c.Unlock()

	c.events = append(c.events, eventType)
}

// errorWithFallback 在报告对应的统计指标事件时触发 fallback 函数。
func (c *command) errorWithFallback(ctx context.Context, err error) {
	eventType := "failure"
	if err == ErrCircuitOpen {
		eventType = "short-circuit"
	} else if err == ErrMaxConcurrency {
		eventType = "rejected"
	} else if err == ErrTimeout {
		eventType = "timeout"
	} else if err == context.Canceled {
		eventType = "context_canceled"
	} else if err == context.DeadlineExceeded {
		eventType = "context_deadline_exceeded"
	}

	c.reportEvent(eventType)
	fallbackErr := c.tryFallback(ctx, err)
	if fallbackErr != nil {
		c.errChan <- fallbackErr
	}
}

func (c *command) tryFallback(ctx context.Context, err error) error {
	if c.fallback == nil {
		// If we don't have a fallback return the original error.
		return err
	}

	fallbackErr := c.fallback(ctx, err)
	if fallbackErr != nil {
		c.reportEvent("fallback-failure")
		return fmt.Errorf("fallback failed with '%v'. run error was '%v'", fallbackErr, err)
	}

	c.reportEvent("fallback-success")

	return nil
}
