package common

import (
	"errors"
	"sync"

	"github.com/RichardKnop/machinery/v1/brokers/iface"
	"github.com/RichardKnop/machinery/v1/config"
	"github.com/RichardKnop/machinery/v1/log"
	"github.com/RichardKnop/machinery/v1/retry"
	"github.com/RichardKnop/machinery/v1/tasks"
)

// 已注册任务名称列表
type registeredTaskNames struct {
	sync.RWMutex
	items []string
}

// Broker represents a base broker structure
// 基础Broker
type Broker struct {
	cnf                 *config.Config
	registeredTaskNames registeredTaskNames
	retry               bool
	retryFunc           func(chan int)
	retryStopChan       chan int
	stopChan            chan int
}

// NewBroker creates new Broker instance
func NewBroker(cnf *config.Config) Broker {
	return Broker{
		cnf:           cnf,
		retry:         true,
		stopChan:      make(chan int),
		retryStopChan: make(chan int),
	}
}

// GetConfig returns config
func (b *Broker) GetConfig() *config.Config {
	return b.cnf
}

// GetRetry ... 是否重试
func (b *Broker) GetRetry() bool {
	return b.retry
}

// GetRetryFunc ...
// retryFunc 用来实现等待重试是等待时间计算
func (b *Broker) GetRetryFunc() func(chan int) {
	return b.retryFunc
}

// GetRetryStopChan ...
// 停止重试 channel
func (b *Broker) GetRetryStopChan() chan int {
	return b.retryStopChan
}

// GetStopChan ...
// 获取停止Channel
func (b *Broker) GetStopChan() chan int {
	return b.stopChan
}

// Publish places a new message on the default queue
// 发布消息，需要子类实现
func (b *Broker) Publish(signature *tasks.Signature) error {
	return errors.New("Not implemented")
}

// SetRegisteredTaskNames sets registered task names
// 加锁设置已注册任务名称，用于之后判断任务是否注册过
func (b *Broker) SetRegisteredTaskNames(names []string) {
	b.registeredTaskNames.Lock()
	defer b.registeredTaskNames.Unlock()
	b.registeredTaskNames.items = names
}

// IsTaskRegistered returns true if the task is registered with this broker
// 加锁判断任务是否已经注册过
func (b *Broker) IsTaskRegistered(name string) bool {
	b.registeredTaskNames.RLock()
	defer b.registeredTaskNames.RUnlock()
	for _, registeredTaskName := range b.registeredTaskNames.items {
		if registeredTaskName == name {
			return true
		}
	}
	return false
}

// GetPendingTasks returns a slice of task.Signatures waiting in the queue
// 等待子类实现
func (b *Broker) GetPendingTasks(queue string) ([]*tasks.Signature, error) {
	return nil, errors.New("Not implemented")
}

// GetDelayedTasks returns a slice of task.Signatures that are scheduled, but not yet in the queue
// 等待子类实现
func (b *Broker) GetDelayedTasks() ([]*tasks.Signature, error) {
	return nil, errors.New("Not implemented")
}

// StartConsuming is a common part of StartConsuming method
// StartConsuming 初始化了 retryFunc 属性
func (b *Broker) StartConsuming(consumerTag string, concurrency int, taskProcessor iface.TaskProcessor) {
	if b.retryFunc == nil {
		b.retryFunc = retry.Closure()
	}

}

// StopConsuming is a common part of StopConsuming
// 发送停止消费信号
func (b *Broker) StopConsuming() {
	// Do not retry from now on
	b.retry = false
	// Stop the retry closure earlier
	select {
	case b.retryStopChan <- 1:
		log.WARNING.Print("Stopping retry closure.")
	default:
	}
	// Notifying the stop channel stops consuming of messages
	close(b.stopChan)
	log.WARNING.Print("Stop channel")
}

// GetRegisteredTaskNames returns registered tasks names
// 获取注册的task名称列表
func (b *Broker) GetRegisteredTaskNames() []string {
	b.registeredTaskNames.RLock()
	defer b.registeredTaskNames.RUnlock()
	items := b.registeredTaskNames.items
	return items
}

// AdjustRoutingKey makes sure the routing key is correct.
// If the routing key is an empty string:
// a) set it to binding key for direct exchange type
// b) set it to default queue name
// 调整RoutingKey
func (b *Broker) AdjustRoutingKey(s *tasks.Signature) {
	if s.RoutingKey != "" {
		return
	}

	s.RoutingKey = b.GetConfig().DefaultQueue
}
