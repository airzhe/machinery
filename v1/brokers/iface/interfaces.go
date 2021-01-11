package iface

import (
	"context"

	"github.com/RichardKnop/machinery/v1/config"
	"github.com/RichardKnop/machinery/v1/tasks"
)

// Broker - a common interface for all brokers
type Broker interface {
	GetConfig() *config.Config
	SetRegisteredTaskNames(names []string)
	//判断任务是否注册
	IsTaskRegistered(name string) bool
	//消费任务
	StartConsuming(consumerTag string, concurrency int, p TaskProcessor) (bool, error)
	//停止消费
	StopConsuming()
	//发布任务
	Publish(ctx context.Context, task *tasks.Signature) error
	//获取待消费任务列表
	GetPendingTasks(queue string) ([]*tasks.Signature, error)
	//获取延迟任务列表
	GetDelayedTasks() ([]*tasks.Signature, error)
	AdjustRoutingKey(s *tasks.Signature)
}

// TaskProcessor - can process a delivered task
// This will probably always be a worker instance
type TaskProcessor interface {
	Process(signature *tasks.Signature) error
	CustomQueue() string
	PreConsumeHandler() bool
}
