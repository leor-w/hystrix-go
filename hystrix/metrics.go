package hystrix

import (
	"sync"
	"time"

	"github.com/afex/hystrix-go/hystrix/metric_collector"
	"github.com/afex/hystrix-go/hystrix/rolling"
)

type commandExecution struct {
	Types            []string      `json:"types"`
	Start            time.Time     `json:"start_time"`
	RunDuration      time.Duration `json:"run_duration"`
	ConcurrencyInUse float64       `json:"concurrency_inuse"`
}

type metricExchange struct {
	Name    string
	Updates chan *commandExecution
	Mutex   *sync.RWMutex

	metricCollectors []metricCollector.MetricCollector
}

// newMetricExchange 创建指标统计
func newMetricExchange(name string) *metricExchange {
	m := &metricExchange{}
	m.Name = name

	// 指标更新 channel 默认容量为 2000
	m.Updates = make(chan *commandExecution, 2000)
	m.Mutex = &sync.RWMutex{}
	m.metricCollectors = metricCollector.Registry.InitializeMetricCollectors(name)
	m.Reset()

	go m.Monitor()

	return m
}

// 如果收集器没有按照规范设置，默认的收集器函数将会panic。
func (m *metricExchange) DefaultCollector() *metricCollector.DefaultMetricCollector {
	if len(m.metricCollectors) < 1 {
		panic("No Metric Collectors Registered.")
	}
	collection, ok := m.metricCollectors[0].(*metricCollector.DefaultMetricCollector)
	if !ok {
		panic("Default metric collector is not registered correctly. The default metric collector must be registered first.")
	}
	return collection
}

func (m *metricExchange) Monitor() {
	for update := range m.Updates {
		// 我们只获取一个读锁，以确保Reset()不会更改数字。
		m.Mutex.RLock()

		totalDuration := time.Since(update.Start)
		wg := &sync.WaitGroup{}
		for _, collector := range m.metricCollectors {
			wg.Add(1)
			go m.IncrementMetrics(wg, collector, update, totalDuration)
		}
		wg.Wait()

		m.Mutex.RUnlock()
	}
}

func (m *metricExchange) IncrementMetrics(wg *sync.WaitGroup, collector metricCollector.MetricCollector, update *commandExecution, totalDuration time.Duration) {
	// 细粒度的度量
	r := metricCollector.MetricResult{
		Attempts:         1,
		TotalDuration:    totalDuration,
		RunDuration:      update.RunDuration,
		ConcurrencyInUse: update.ConcurrencyInUse,
	}

	switch update.Types[0] {
	case "success":
		r.Successes = 1
	case "failure":
		r.Failures = 1
		r.Errors = 1
	case "rejected":
		r.Rejects = 1
		r.Errors = 1
	case "short-circuit":
		r.ShortCircuits = 1
		r.Errors = 1
	case "timeout":
		r.Timeouts = 1
		r.Errors = 1
	case "context_canceled":
		r.ContextCanceled = 1
	case "context_deadline_exceeded":
		r.ContextDeadlineExceeded = 1
	}

	// 如果类型字段中有多于1个的内容则表示需要进行fallback计数
	if len(update.Types) > 1 {
		// 回退指标
		if update.Types[1] == "fallback-success" {
			r.FallbackSuccesses = 1
		}
		if update.Types[1] == "fallback-failure" {
			r.FallbackFailures = 1
		}
	}

	collector.Update(r)

	wg.Done()
}

func (m *metricExchange) Reset() {
	m.Mutex.Lock()
	defer m.Mutex.Unlock()

	for _, collector := range m.metricCollectors {
		collector.Reset()
	}
}

func (m *metricExchange) Requests() *rolling.Number {
	m.Mutex.RLock()
	defer m.Mutex.RUnlock()
	return m.requestsLocked()
}

func (m *metricExchange) requestsLocked() *rolling.Number {
	return m.DefaultCollector().NumRequests()
}

func (m *metricExchange) ErrorPercent(now time.Time) int {
	m.Mutex.RLock()
	defer m.Mutex.RUnlock()

	var errPct float64
	reqs := m.requestsLocked().Sum(now)
	errs := m.DefaultCollector().Errors().Sum(now)

	if reqs > 0 {
		errPct = (float64(errs) / float64(reqs)) * 100
	}

	return int(errPct + 0.5)
}

func (m *metricExchange) IsHealthy(now time.Time) bool {
	return m.ErrorPercent(now) < getSettings(m.Name).ErrorPercentThreshold
}
