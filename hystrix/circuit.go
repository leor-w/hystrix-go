package hystrix

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// CircuitBreaker 断路器，为每个ExecutorPool创建，跟踪控制是否请求以及在环路运行状态过低时拒绝请求
type CircuitBreaker struct {
	// 断路器名称
	Name                   string
	// 断路器开关状态，open 表示关闭断路器
	open                   bool
	// 强制打开
	forceOpen              bool
	// 断路器的锁
	mutex                  *sync.RWMutex
	// 打开或测试的最后时间
	openedOrLastTestedTime int64

	// 执行者池
	executorPool *executorPool
	// 计数指标
	metrics      *metricExchange
}

var (
	circuitBreakersMutex *sync.RWMutex
	circuitBreakers      map[string]*CircuitBreaker
)

func init() {
	circuitBreakersMutex = &sync.RWMutex{}
	circuitBreakers = make(map[string]*CircuitBreaker)
}

// GetCircuit 返回给定命令的断路器以及 本次调用是否创建了它。
func GetCircuit(name string) (*CircuitBreaker, bool, error) {
	circuitBreakersMutex.RLock()
	_, ok := circuitBreakers[name]
	// 如果不存在同名的 断路器 则创建一个新的
	if !ok {
		// 释放 circuitBreakers 的读锁
		circuitBreakersMutex.RUnlock()
		circuitBreakersMutex.Lock()
		defer circuitBreakersMutex.Unlock()
		// 因为前面释放了 circuitBreakers 的读锁，
		// 所以在创建之前需要再次检查是否有其他线程先于此次调用之前创建了同名的断路器。
		if cb, ok := circuitBreakers[name]; ok {
			return cb, false, nil
		}
		circuitBreakers[name] = newCircuitBreaker(name)
	} else {
		defer circuitBreakersMutex.RUnlock()
	}

	return circuitBreakers[name], !ok, nil
}

// Flush 从内存中清除所有断路器和统计信息。
func Flush() {
	circuitBreakersMutex.Lock()
	defer circuitBreakersMutex.Unlock()

	for name, cb := range circuitBreakers {
		cb.metrics.Reset()
		cb.executorPool.Metrics.Reset()
		delete(circuitBreakers, name)
	}
}

// newCircuitBreaker 创建一个指定名称的 CircuitBreaker (断路器)
func newCircuitBreaker(name string) *CircuitBreaker {
	c := &CircuitBreaker{}
	c.Name = name
	c.metrics = newMetricExchange(name)
	c.executorPool = newExecutorPool(name)
	c.mutex = &sync.RWMutex{}

	return c
}

// toggleForceOpen 允许手动为给定名称的所有实例生成回退逻辑。
func (circuit *CircuitBreaker) toggleForceOpen(toggle bool) error {
	circuit, _, err := GetCircuit(circuit.Name)
	if err != nil {
		return err
	}

	circuit.forceOpen = toggle
	return nil
}

// IsOpen 在执行任何命令之前调用，以检查是否应该尝试执行该命令。"Open" 状态是指它被禁用。
func (circuit *CircuitBreaker) IsOpen() bool {
	circuit.mutex.RLock()
	o := circuit.forceOpen || circuit.open
	circuit.mutex.RUnlock()

	if o {
		return true
	}

	if uint64(circuit.metrics.Requests().Sum(time.Now())) < getSettings(circuit.Name).RequestVolumeThreshold {
		return false
	}

	// 检查断路器健康状态
	if !circuit.metrics.IsHealthy(time.Now()) {
		// 故障太多，断开电路
		circuit.setOpen()
		return true
	}

	return false
}

// AllowRequest 在命令执行之前进行检查，以确保断路器状态和指标运行状况允许这样做。
// 当电路断开时，这个调用偶尔会返回true，以测量外部服务是否已恢复。
// 两种状态 1 断路器为可用状态返回true。2 断路器为不可用状态，但是已经超过休眠期，测试性执行。
func (circuit *CircuitBreaker) AllowRequest() bool {
	return !circuit.IsOpen() || circuit.allowSingleTest()
}

func (circuit *CircuitBreaker) allowSingleTest() bool {
	circuit.mutex.RLock()
	defer circuit.mutex.RUnlock()

	now := time.Now().UnixNano()
	openedOrLastTestedTime := atomic.LoadInt64(&circuit.openedOrLastTestedTime)
	// 如果断路器为开启状态，且时间已经过了休眠时间则尝试性的打开请求。以测试外部服务是否已恢复
	if circuit.open && now > openedOrLastTestedTime+getSettings(circuit.Name).SleepWindow.Nanoseconds() {
		swapped := atomic.CompareAndSwapInt64(&circuit.openedOrLastTestedTime, openedOrLastTestedTime, now)
		if swapped {
			log.Printf("hystrix-go: allowing single test to possibly close circuit %v", circuit.Name)
		}
		return swapped
	}

	return false
}

// setOpen 设置断路器为断开状态
func (circuit *CircuitBreaker) setOpen() {
	circuit.mutex.Lock()
	defer circuit.mutex.Unlock()

	if circuit.open {
		return
	}

	log.Printf("hystrix-go: opening circuit %v", circuit.Name)

	// 设置上次测试时间点为当前时间
	circuit.openedOrLastTestedTime = time.Now().UnixNano()
	// 设置断路器为断开
	circuit.open = true
}

// setClose 设置断路器为闭合状态
func (circuit *CircuitBreaker) setClose() {
	circuit.mutex.Lock()
	defer circuit.mutex.Unlock()

	if !circuit.open {
		return
	}

	log.Printf("hystrix-go: closing circuit %v", circuit.Name)

	// 将断路器开关状态设置为闭合状态
	circuit.open = false
	// 计数指标重置
	circuit.metrics.Reset()
}

// ReportEvent 记录断路器计数指标，以跟踪最近的错误率并将数据暴露给仪表板。
// eventTypes 实践类型
// start 事件起始时间
// runDuration 执行时长
func (circuit *CircuitBreaker) ReportEvent(eventTypes []string, start time.Time, runDuration time.Duration) error {
	if len(eventTypes) == 0 {
		return fmt.Errorf("no event types sent for metrics")
	}

	circuit.mutex.RLock()
	o := circuit.open
	circuit.mutex.RUnlock()
	// 如果事件为成功状态，且断路器为断开状态，则设置断路器为闭合状态
	if eventTypes[0] == "success" && o {
		circuit.setClose()
	}

	var concurrencyInUse float64
	// 获取并发使用数，使用 executorPool 活跃数量取模最大数量得到
	if circuit.executorPool.Max > 0 {
		concurrencyInUse = float64(circuit.executorPool.ActiveCount()) / float64(circuit.executorPool.Max)
	}

	// 写入计数指标通道，如果通道堵塞，则抛出满载错误
	select {
	case circuit.metrics.Updates <- &commandExecution{
		Types:            eventTypes,
		Start:            start,
		RunDuration:      runDuration,
		ConcurrencyInUse: concurrencyInUse,
	}:
	default:
		return CircuitError{Message: fmt.Sprintf("metrics channel (%v) is at capacity", circuit.Name)}
	}

	return nil
}
