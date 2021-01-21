package metricCollector

import (
	"sync"
	"time"
)

// Registry 是断路器将用于收集有关断路器运行状况的统计信息的默认 metricCollectorRegistry。
var Registry = metricCollectorRegistry{
	lock: &sync.RWMutex{},
	registry: []func(name string) MetricCollector{
		newDefaultMetricCollector,
	},
}

type metricCollectorRegistry struct {
	lock     *sync.RWMutex
	registry []func(name string) MetricCollector
}

// InitializeMetricCollectors 运行已注册的MetricCollector初始化器以创建 MetricCollector 数组。
func (m *metricCollectorRegistry) InitializeMetricCollectors(name string) []MetricCollector {
	m.lock.RLock()
	defer m.lock.RUnlock()

	metrics := make([]MetricCollector, len(m.registry))
	for i, metricCollectorInitializer := range m.registry {
		metrics[i] = metricCollectorInitializer(name)
	}
	return metrics
}

// Register 在此metricCollectorRegistry维护的注册表中添加 MetricCollector 初始化器。
func (m *metricCollectorRegistry) Register(initMetricCollector func(string) MetricCollector) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.registry = append(m.registry, initMetricCollector)
}

type MetricResult struct {
	Attempts                float64
	Errors                  float64
	Successes               float64
	Failures                float64
	Rejects                 float64
	ShortCircuits           float64
	Timeouts                float64
	FallbackSuccesses       float64
	FallbackFailures        float64
	ContextCanceled         float64
	ContextDeadlineExceeded float64
	TotalDuration           time.Duration
	RunDuration             time.Duration
	ConcurrencyInUse        float64
}

// MetricCollector 表示所有收集器在收集电路统计信息时必须履行的契约。这个接口的实现不必维护其数据存储的锁定，只要它们没有在hystrix上下文之外被修改。
type MetricCollector interface {
	// Update 从远程添加的命令执行中接受一组指标
	Update(MetricResult)
	// Reset 重置内部计数器和计时器。
	Reset()
}
