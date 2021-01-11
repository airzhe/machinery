package redis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/go-redsync/redsync/v4"
	redsyncredis "github.com/go-redsync/redsync/v4/redis/redigo"
	"github.com/gomodule/redigo/redis"

	"github.com/RichardKnop/machinery/v1/brokers/errs"
	"github.com/RichardKnop/machinery/v1/brokers/iface"
	"github.com/RichardKnop/machinery/v1/common"
	"github.com/RichardKnop/machinery/v1/config"
	"github.com/RichardKnop/machinery/v1/log"
	"github.com/RichardKnop/machinery/v1/tasks"
)

// 默认延迟队列key
var redisDelayedTasksKey = "delayed_tasks"

// Broker represents a Redis broker
type Broker struct {
	common.Broker
	common.RedisConnector // 嵌套RedisConnector，后面调用NewPool
	host                  string
	password              string
	db                    int
	pool                  *redis.Pool
	consumingWG           sync.WaitGroup // wait group to make sure whole consumption completes
	processingWG          sync.WaitGroup // use wait group to make sure task processing completes
	delayedWG             sync.WaitGroup
	// If set, path to a socket file overrides hostname
	socketPath string
	redsync    *redsync.Redsync
	redisOnce  sync.Once // 只执行一次
}

// New creates new Broker instance
func New(cnf *config.Config, host, password, socketPath string, db int) iface.Broker {
	b := &Broker{Broker: common.NewBroker(cnf)}
	b.host = host
	b.db = db
	b.password = password
	b.socketPath = socketPath

	if cnf.Redis != nil && cnf.Redis.DelayedTasksKey != "" {
		// 从配置读取到的延迟队列key
		redisDelayedTasksKey = cnf.Redis.DelayedTasksKey
	}

	return b
}

// StartConsuming enters a loop and waits for incoming messages
// consumerTag := "machinery_worker"
func (b *Broker) StartConsuming(consumerTag string, concurrency int, taskProcessor iface.TaskProcessor) (bool, error) {
	b.consumingWG.Add(1)
	defer b.consumingWG.Done()

	if concurrency < 1 {
		//NumCPU：返回当前系统的 CPU 核数量
		concurrency = runtime.NumCPU() * 2
	}

	//调用基础类的StartConsuming方法，初始化 retryFunc 属性
	b.Broker.StartConsuming(consumerTag, concurrency, taskProcessor)

	conn := b.open()
	defer conn.Close()

	// Ping the server to make sure connection is live
	_, err := conn.Do("PING")
	if err != nil {
		//等待多久时间之后，如果 retryStopChan 获得信号，立马返回，后者等待 闭包 fibonacci 方法计算后得到的时间
		b.GetRetryFunc()(b.GetRetryStopChan())

		// Return err if retry is still true.
		// If retry is false, broker.StopConsuming() has been called and
		// therefore Redis might have been stopped. Return nil exit
		// StartConsuming()
		if b.GetRetry() {
			return b.GetRetry(), err
		}
		// 如果重试为false，返回 errors.New("the server has been stopped")
		return b.GetRetry(), errs.ErrConsumerStopped
	}

	// Channel to which we will push tasks ready for processing by worker
	deliveries := make(chan []byte, concurrency)
	pool := make(chan struct{}, concurrency)

	// initialize worker pool with maxWorkers workers
	for i := 0; i < concurrency; i++ {
		pool <- struct{}{}
	}

	// A receiving goroutine keeps popping messages from the queue by BLPOP
	// If the message is valid and can be unmarshaled into a proper structure
	// we send it to the deliveries channel
	go func() {

		log.INFO.Print("[*] Waiting for messages. To exit press CTRL+C")

		for {
			select {
			// A way to stop this goroutine from b.StopConsuming
			case <-b.GetStopChan():
				close(deliveries)
				return
			case <-pool:
				select {
				case <-b.GetStopChan():
					close(deliveries)
					return
				default:
				}

				//预处理
				if taskProcessor.PreConsumeHandler() {
					// 获取下次任务
					task, _ := b.nextTask(getQueue(b.GetConfig(), taskProcessor))
					//TODO: should this error be ignored?
					if len(task) > 0 {
						// 把任务放到deliveries channel待消费， 此处控制并发
						deliveries <- task
					}
				}

				pool <- struct{}{}
			}
		}
	}()

	// A goroutine to watch for delayed tasks and push them to deliveries
	// channel for consumption by the worker
	b.delayedWG.Add(1)
	go func() {
		defer b.delayedWG.Done()

		for {
			select {
			// A way to stop this goroutine from b.StopConsuming
			case <-b.GetStopChan():
				return
			default:
				// 获取下一个该消费的延迟任务，如果出错continue
				task, err := b.nextDelayedTask(redisDelayedTasksKey)
				if err != nil {
					continue
				}

				signature := new(tasks.Signature)
				decoder := json.NewDecoder(bytes.NewReader(task))
				// UseNumber让解码器将数字解组到interface{}中，而不是float64
				decoder.UseNumber()
				if err := decoder.Decode(signature); err != nil {
					log.ERROR.Print(errs.NewErrCouldNotUnmarshalTaskSignature(task, err))
				}
				// 发布延迟任务到正常队列待消费
				if err := b.Publish(context.Background(), signature); err != nil {
					log.ERROR.Print(err)
				}
			}
		}
	}()

	if err := b.consume(deliveries, concurrency, taskProcessor); err != nil {
		return b.GetRetry(), err
	}

	// Waiting for any tasks being processed to finish
	// 等待所有队列处理完毕
	b.processingWG.Wait()

	return b.GetRetry(), nil
}

// StopConsuming quits the loop
func (b *Broker) StopConsuming() {
	b.Broker.StopConsuming()
	// Waiting for the delayed tasks goroutine to have stopped
	b.delayedWG.Wait()
	// Waiting for consumption to finish
	b.consumingWG.Wait()
	// Wait for currently processing tasks to finish as well.
	b.processingWG.Wait()

	if b.pool != nil {
		b.pool.Close()
	}
}

// Publish places a new message on the default queue
// 发布任务到默认队列
func (b *Broker) Publish(ctx context.Context, signature *tasks.Signature) error {
	// Adjust routing key (this decides which queue the message will be published to)
	b.Broker.AdjustRoutingKey(signature)

	msg, err := json.Marshal(signature)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %s", err)
	}

	conn := b.open()
	defer conn.Close()

	// Check the ETA signature field, if it is set and it is in the future,
	// delay the task
	if signature.ETA != nil {
		now := time.Now().UTC()

		//如果t代表的时间点在u之后，返回真；否则返回假。
		if signature.ETA.After(now) {
			score := signature.ETA.UnixNano()
			_, err = conn.Do("ZADD", redisDelayedTasksKey, score, msg)
			return err
		}
	}

	_, err = conn.Do("RPUSH", signature.RoutingKey, msg)
	return err
}

// GetPendingTasks returns a slice of task signatures waiting in the queue
// 获取等待消费的任务列表
func (b *Broker) GetPendingTasks(queue string) ([]*tasks.Signature, error) {
	conn := b.open()
	defer conn.Close()

	if queue == "" {
		queue = b.GetConfig().DefaultQueue
	}
	dataBytes, err := conn.Do("LRANGE", queue, 0, -1)
	if err != nil {
		return nil, err
	}
	results, err := redis.ByteSlices(dataBytes, err)
	if err != nil {
		return nil, err
	}

	taskSignatures := make([]*tasks.Signature, len(results))
	for i, result := range results {
		signature := new(tasks.Signature)
		decoder := json.NewDecoder(bytes.NewReader(result))
		decoder.UseNumber()
		if err := decoder.Decode(signature); err != nil {
			return nil, err
		}
		taskSignatures[i] = signature
	}
	return taskSignatures, nil
}

// GetDelayedTasks returns a slice of task signatures that are scheduled, but not yet in the queue
// 获取延迟任务队列列表
func (b *Broker) GetDelayedTasks() ([]*tasks.Signature, error) {
	conn := b.open()
	defer conn.Close()

	dataBytes, err := conn.Do("ZRANGE", redisDelayedTasksKey, 0, -1)
	if err != nil {
		return nil, err
	}
	results, err := redis.ByteSlices(dataBytes, err)
	if err != nil {
		return nil, err
	}

	taskSignatures := make([]*tasks.Signature, len(results))
	for i, result := range results {
		signature := new(tasks.Signature)
		decoder := json.NewDecoder(bytes.NewReader(result))
		decoder.UseNumber()
		if err := decoder.Decode(signature); err != nil {
			return nil, err
		}
		taskSignatures[i] = signature
	}
	return taskSignatures, nil
}

// consume takes delivered messages from the channel and manages a worker pool
// to process tasks concurrently
//参数consumerTag在AMQP作为Broker时有意义；参数concurrency用来实现任务并发调度的控制。
func (b *Broker) consume(deliveries <-chan []byte, concurrency int, taskProcessor iface.TaskProcessor) error {
	errorsChan := make(chan error, concurrency*2)
	pool := make(chan struct{}, concurrency)

	// init pool for Worker tasks execution, as many slots as Worker concurrency param
	// 通过带缓冲的pool控制并发
	go func() {
		for i := 0; i < concurrency; i++ {
			pool <- struct{}{}
		}
	}()

	for {
		select {
		case err := <-errorsChan:
			return err
		case d, open := <-deliveries:
			if !open {
				return nil
			}
			if concurrency > 0 {
				// get execution slot from pool (blocks until one is available)
				select {
				//
				case <-b.GetStopChan():
					// 如果收到停止信号，消息重新入队
					b.requeueMessage(d, taskProcessor)
					continue
				case <-pool:
				}
			}

			b.processingWG.Add(1)

			// Consume the task inside a goroutine so multiple tasks
			// can be processed concurrently
			go func() {
				//同步执行阻塞
				if err := b.consumeOne(d, taskProcessor); err != nil {
					errorsChan <- err
				}

				b.processingWG.Done()

				if concurrency > 0 {
					// give slot back to pool
					// 执行完成 pool 写入数据
					pool <- struct{}{}
				}
			}()
		}
	}
}

// consumeOne processes a single message using TaskProcessor
// 消费一个任务
func (b *Broker) consumeOne(delivery []byte, taskProcessor iface.TaskProcessor) error {
	signature := new(tasks.Signature)
	decoder := json.NewDecoder(bytes.NewReader(delivery))
	decoder.UseNumber()
	if err := decoder.Decode(signature); err != nil {
		return errs.NewErrCouldNotUnmarshalTaskSignature(delivery, err)
	}

	// If the task is not registered, we requeue it,
	// there might be different workers for processing specific tasks
	// 如果任务名称没有注册过报错返回
	if !b.IsTaskRegistered(signature.Name) {
		if signature.IgnoreWhenTaskNotRegistered {
			return nil
		}
		log.INFO.Printf("Task not registered with this worker. Requeuing message: %s", delivery)
		b.requeueMessage(delivery, taskProcessor)
		return nil
	}

	log.DEBUG.Printf("Received new message: %s", delivery)

	// 执行worker的Process方法
	return taskProcessor.Process(signature)
}

// nextTask pops next available task from the default queue
//通过Blpop获取下一个任务
func (b *Broker) nextTask(queue string) (result []byte, err error) {
	conn := b.open()
	defer conn.Close()

	pollPeriodMilliseconds := 1000 // default poll period for normal tasks
	if b.GetConfig().Redis != nil {
		configuredPollPeriod := b.GetConfig().Redis.NormalTasksPollPeriod
		if configuredPollPeriod > 0 {
			pollPeriodMilliseconds = configuredPollPeriod
		}
	}
	pollPeriod := time.Duration(pollPeriodMilliseconds) * time.Millisecond

	// Issue 548: BLPOP expects an integer timeout expresses in seconds.
	// The call will if the value is a float. Convert to integer using
	// math.Ceil():
	//   math.Ceil(0.0) --> 0 (block indefinitely)
	//   math.Ceil(0.2) --> 1 (timeout after 1 second)
	pollPeriodSeconds := math.Ceil(pollPeriod.Seconds())

	items, err := redis.ByteSlices(conn.Do("BLPOP", queue, pollPeriodSeconds))
	if err != nil {
		return []byte{}, err
	}

	// items[0] - the name of the key where an element was popped
	// items[1] - the value of the popped element
	if len(items) != 2 {
		return []byte{}, redis.ErrNil
	}

	result = items[1]

	return result, nil
}

// nextDelayedTask pops a value from the ZSET key using WATCH/MULTI/EXEC commands.
// https://github.com/gomodule/redigo/blob/master/redis/zpop_example_test.go
// 获取有序集合里，值比当前时间大的任务返回，然后删除集合元素
func (b *Broker) nextDelayedTask(key string) (result []byte, err error) {
	conn := b.open()
	defer conn.Close()

	defer func() {
		// Return connection to normal state on error.
		// https://redis.io/commands/discard
		// https://redis.io/commands/unwatch
		if err == redis.ErrNil {
			conn.Do("UNWATCH")
		} else if err != nil {
			conn.Do("DISCARD")
		}
	}()

	var (
		items [][]byte
		reply interface{}
	)

	pollPeriod := 500 // default poll period for delayed tasks
	if b.GetConfig().Redis != nil {
		configuredPollPeriod := b.GetConfig().Redis.DelayedTasksPollPeriod
		// the default period is 0, which bombards redis with requests, despite
		// our intention of doing the opposite
		if configuredPollPeriod > 0 {
			pollPeriod = configuredPollPeriod
		}
	}

	for {
		// Space out queries to ZSET so we don't bombard redis
		// server with relentless ZRANGEBYSCOREs
		time.Sleep(time.Duration(pollPeriod) * time.Millisecond)
		if _, err = conn.Do("WATCH", key); err != nil {
			return
		}

		now := time.Now().UTC().UnixNano()

		// https://redis.io/commands/zrangebyscore
		items, err = redis.ByteSlices(conn.Do(
			"ZRANGEBYSCORE",
			key,
			0,
			now,
			"LIMIT",
			0,
			1,
		))
		if err != nil {
			return
		}
		if len(items) != 1 {
			err = redis.ErrNil
			return
		}

		_ = conn.Send("MULTI")
		_ = conn.Send("ZREM", key, items[0])
		reply, err = conn.Do("EXEC")
		if err != nil {
			return
		}

		if reply != nil {
			result = items[0]
			break
		}
	}

	return
}

// open returns or creates instance of Redis connection
// 从连接池里获取一个连接
func (b *Broker) open() redis.Conn {
	b.redisOnce.Do(func() {
		b.pool = b.NewPool(b.socketPath, b.host, b.password, b.db, b.GetConfig().Redis, b.GetConfig().TLSConfig)
		b.redsync = redsync.New(redsyncredis.NewPool(b.pool))
	})

	return b.pool.Get()
}

func getQueue(config *config.Config, taskProcessor iface.TaskProcessor) string {
	customQueue := taskProcessor.CustomQueue()
	if customQueue == "" {
		return config.DefaultQueue
	}
	return customQueue
}

// 重新入队
func (b *Broker) requeueMessage(delivery []byte, taskProcessor iface.TaskProcessor) {
	conn := b.open()
	defer conn.Close()
	conn.Do("RPUSH", getQueue(b.GetConfig(), taskProcessor), delivery)
}
