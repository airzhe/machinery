package machinery

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/opentracing/opentracing-go"

	"github.com/RichardKnop/machinery/v1/backends/amqp"
	"github.com/RichardKnop/machinery/v1/brokers/errs"
	"github.com/RichardKnop/machinery/v1/log"
	"github.com/RichardKnop/machinery/v1/retry"
	"github.com/RichardKnop/machinery/v1/tasks"
	"github.com/RichardKnop/machinery/v1/tracing"
)

// Worker represents a single worker process
type Worker struct {
	server            *Server
	ConsumerTag       string
	Concurrency       int
	Queue             string
	errorHandler      func(err error)
	preTaskHandler    func(*tasks.Signature)
	postTaskHandler   func(*tasks.Signature)
	preConsumeHandler func(*Worker) bool
}

var (
	// ErrWorkerQuitGracefully is return when worker quit gracefully Worker 优雅的退出
	ErrWorkerQuitGracefully = errors.New("Worker quit gracefully")
	// ErrWorkerQuitGracefully is return when worker quit abruptly
	ErrWorkerQuitAbruptly = errors.New("Worker quit abruptly")
)

// Launch starts a new worker process. The worker subscribes
// to the default queue and processes incoming registered tasks
func (worker *Worker) Launch() error {
	errorsChan := make(chan error)

	// 异步执行
	worker.LaunchAsync(errorsChan)

	// 阻塞
	return <-errorsChan
}

// LaunchAsync is a non blocking version of Launch
func (worker *Worker) LaunchAsync(errorsChan chan<- error) {
	//日志输出相关配置
	cnf := worker.server.GetConfig()
	broker := worker.server.GetBroker()

	// Log some useful information about worker configuration
	log.INFO.Printf("Launching a worker with the following settings:")
	log.INFO.Printf("- Broker: %s", RedactURL(cnf.Broker))
	if worker.Queue == "" {
		log.INFO.Printf("- DefaultQueue: %s", cnf.DefaultQueue)
	} else {
		log.INFO.Printf("- CustomQueue: %s", worker.Queue)
	}
	log.INFO.Printf("- ResultBackend: %s", RedactURL(cnf.ResultBackend))
	//如果配置了rabbitmq
	if cnf.AMQP != nil {
		log.INFO.Printf("- AMQP: %s", cnf.AMQP.Exchange)
		log.INFO.Printf("  - Exchange: %s", cnf.AMQP.Exchange)
		log.INFO.Printf("  - ExchangeType: %s", cnf.AMQP.ExchangeType)
		log.INFO.Printf("  - BindingKey: %s", cnf.AMQP.BindingKey)
		log.INFO.Printf("  - PrefetchCount: %d", cnf.AMQP.PrefetchCount)
	}

	var signalWG sync.WaitGroup
	// Goroutine to start broker consumption and handle retries when broker connection dies
	// 开启for循环，如果返回retry会重试（如复用之前的连接不可用），否则致命错误退出
	go func() {
		for {
			retry, err := broker.StartConsuming(worker.ConsumerTag, worker.Concurrency, worker)

			if retry {
				if worker.errorHandler != nil {
					worker.errorHandler(err)
				} else {
					log.WARNING.Printf("Broker failed with error: %s", err)
				}
			} else {
				// 此时信号量值为0，直接退出?
				signalWG.Wait()
				errorsChan <- err // stop the goroutine
				return
			}
		}
	}()
	if !cnf.NoUnixSignals {
		// 避免反复信号被忽略，如果sig没有缓冲区，多个信号只能接收一，使用Select实现无阻塞读写
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		var signalsReceived uint

		// Goroutine Handle SIGINT and SIGTERM signals
		go func() {
			for s := range sig {
				log.WARNING.Printf("Signal received: %v", s)
				signalsReceived++

				if signalsReceived < 2 {
					// After first Ctrl+C start quitting the worker gracefully
					// 用户第一次 Ctrl+c 开启优雅退出
					log.WARNING.Print("Waiting for running tasks to finish before shutting down")
					// 不明白这里为什么用signalWG
					signalWG.Add(1)
					go func() {
						//worker.server.GetBroker().StopConsuming()
						worker.Quit()
						errorsChan <- ErrWorkerQuitGracefully
						signalWG.Done()
					}()
				} else {
					// Abort the program when user hits Ctrl+C second time in a row
					// 连续两次则立即退出
					errorsChan <- ErrWorkerQuitAbruptly
				}
			}
		}()
	}
}

// CustomQueue returns Custom Queue of the running worker process
func (worker *Worker) CustomQueue() string {
	return worker.Queue
}

// Quit tears down the running worker process
func (worker *Worker) Quit() {
	worker.server.GetBroker().StopConsuming()
}

// Process handles received tasks and triggers success/error callbacks
func (worker *Worker) Process(signature *tasks.Signature) error {
	// If the task is not registered with this worker, do not continue
	// but only return nil as we do not want to restart the worker process
	// 如果task 没有注册过，直接返回nil
	if !worker.server.IsTaskRegistered(signature.Name) {
		return nil
	}

	// 根据signature名称，获取taskFunc
	taskFunc, err := worker.server.GetRegisteredTask(signature.Name)
	if err != nil {
		return nil
	}

	// Update task state to RECEIVED
	// 在Backend 更新task状态
	if err = worker.server.GetBackend().SetStateReceived(signature); err != nil {
		return fmt.Errorf("Set state to 'received' for task %s returned error: %s", signature.UUID, err)
	}

	// Prepare task for processing
	// 根据 taskFunc 和签名，反射得到task对象
	task, err := tasks.NewWithSignature(taskFunc, signature)
	// if this failed, it means the task is malformed, probably has invalid
	// signature, go directly to task failed without checking whether to retry
	if err != nil {
		worker.taskFailed(signature, err)
		return err
	}

	// try to extract trace span from headers and add it to the function context
	// so it can be used inside the function if it has context.Context as the first
	// argument. Start a new span if it isn't found.
	taskSpan := tracing.StartSpanFromHeaders(signature.Headers, signature.Name)
	tracing.AnnotateSpanWithSignatureInfo(taskSpan, signature)
	task.Context = opentracing.ContextWithSpan(task.Context, taskSpan)

	// Update task state to STARTED
	// 如果 backend 为 null，执行 SetStateStarted 什么都不会做，参考v1/backends/null.go
	if err = worker.server.GetBackend().SetStateStarted(signature); err != nil {
		return fmt.Errorf("Set state to 'started' for task %s returned error: %s", signature.UUID, err)
	}

	//Run handler before the task is called
	//执行预处理函数
	if worker.preTaskHandler != nil {
		worker.preTaskHandler(signature)
	}

	//Defer run handler for the end of the task
	//通过defer执行后置函数
	if worker.postTaskHandler != nil {
		defer worker.postTaskHandler(signature)
	}

	// Call the task
	// 通过反射执行task
	// error 不为空存在两种情况: 反射执行panic，比如缺少参数，task 最后一个参数是一个 non-nil error
	results, err := task.Call()
	if err != nil {
		// If a tasks.ErrRetryTaskLater was returned from the task,
		// retry the task after specified duration
		// 如果返回了 ErrRetryTaskLater 错误，重试
		retriableErr, ok := interface{}(err).(tasks.ErrRetryTaskLater)
		if ok {
			//
			return worker.retryTaskIn(signature, retriableErr.RetryIn())
		}

		// Otherwise, execute default retry logic based on signature.RetryCount
		// and signature.RetryTimeout values
		// 如果重试次数大于 0,执行taskRetry
		if signature.RetryCount > 0 {
			return worker.taskRetry(signature)
		}

		return worker.taskFailed(signature, err)
	}

	return worker.taskSucceeded(signature, results)
}

// retryTask decrements RetryCount counter and republishes the task to the queue
// 根据RetryCount 和 RetryTimeout 进行重试，每次重试后 RetryCount--
func (worker *Worker) taskRetry(signature *tasks.Signature) error {
	// Update task state to RETRY
	if err := worker.server.GetBackend().SetStateRetry(signature); err != nil {
		return fmt.Errorf("Set state to 'retry' for task %s returned error: %s", signature.UUID, err)
	}

	// Decrement the retry counter, when it reaches 0, we won't retry again
	signature.RetryCount--

	// Increase retry timeout
	// 每次重试时间比之前时间成斐波纳契式递增
	signature.RetryTimeout = retry.FibonacciNext(signature.RetryTimeout)

	// Delay task by signature.RetryTimeout seconds
	eta := time.Now().UTC().Add(time.Second * time.Duration(signature.RetryTimeout))
	signature.ETA = &eta

	log.WARNING.Printf("Task %s failed. Going to retry in %d seconds.", signature.UUID, signature.RetryTimeout)

	// Send the task back to the queue
	_, err := worker.server.SendTask(signature)
	return err
}

// taskRetryIn republishes the task to the queue with ETA of now + retryIn.Seconds()
// 延迟retryIn时间后，重新加入队列,会不断重试， 这个任务的停止交给用户控制？
func (worker *Worker) retryTaskIn(signature *tasks.Signature, retryIn time.Duration) error {
	// Update task state to RETRY
	if err := worker.server.GetBackend().SetStateRetry(signature); err != nil {
		return fmt.Errorf("Set state to 'retry' for task %s returned error: %s", signature.UUID, err)
	}

	// Delay task by retryIn duration
	eta := time.Now().UTC().Add(retryIn)
	signature.ETA = &eta

	log.WARNING.Printf("Task %s failed. Going to retry in %.0f seconds.", signature.UUID, retryIn.Seconds())

	// Send the task back to the queue
	_, err := worker.server.SendTask(signature)
	return err
}

// taskSucceeded updates the task state and triggers success callbacks or a
// chord callback if this was the last task of a group with a chord callback
/*
type TaskResult struct {
  Type  string      `bson:"type"`  //reflect.TypeOf(val).String()
  Value interface{} `bson:"value"` //reflect.ValueOf(val).Interface()
}
*/
func (worker *Worker) taskSucceeded(signature *tasks.Signature, taskResults []*tasks.TaskResult) error {
	// Update task state to SUCCESS
	// 更新状态，并把结果写入 TaskState 结构体
	if err := worker.server.GetBackend().SetStateSuccess(signature, taskResults); err != nil {
		return fmt.Errorf("Set state to 'success' for task %s returned error: %s", signature.UUID, err)
	}

	// Log human readable results of the processed task
	// 把结果写入日志
	var debugResults = "[]"
	results, err := tasks.ReflectTaskResults(taskResults)
	if err != nil {
		log.WARNING.Print(err)
	} else {
		debugResults = tasks.HumanReadableResults(results)
	}
	log.DEBUG.Printf("Processed task %s. Results = %s", signature.UUID, debugResults)

	// Trigger success callbacks
	// 触发成功回调
	for _, successTask := range signature.OnSuccess {
		// 把当前结果追加到 successTask.Args 里，chain模式下可能用到，前一个任务的结果当作后一个任务的参数
		if signature.Immutable == false {
			// Pass results of the task to success callbacks
			for _, taskResult := range taskResults {
				successTask.Args = append(successTask.Args, tasks.Arg{
					Type:  taskResult.Type,
					Value: taskResult.Value,
				})
			}
		}

		worker.server.SendTask(successTask)
	}

	// If the task was not part of a group, just return
	if signature.GroupUUID == "" {
		return nil
	}

	// 单个任务或者分组分组到此返回，下面处理 chord 任务
	// There is no chord callback, just return
	if signature.ChordCallback == nil {
		return nil
	}

	// Check if all task in the group has completed
	// 判断分组任务是否完成，完成包括成功或失败
	groupCompleted, err := worker.server.GetBackend().GroupCompleted(
		signature.GroupUUID,
		signature.GroupTaskCount,
	)
	if err != nil {
		return fmt.Errorf("Completed check for group %s returned error: %s", signature.GroupUUID, err)
	}

	// If the group has not yet completed, just return
	if !groupCompleted {
		return nil
	}

	// Defer purging of group meta queue if we are using AMQP backend
	if worker.hasAMQPBackend() {
		defer worker.server.GetBackend().PurgeGroupMeta(signature.GroupUUID)
	}

	// Trigger chord callback
	// 判断 ChordTriggered 是否为true,为true表示已经触发过了，直接返回
	shouldTrigger, err := worker.server.GetBackend().TriggerChord(signature.GroupUUID)
	if err != nil {
		return fmt.Errorf("Triggering chord for group %s returned error: %s", signature.GroupUUID, err)
	}

	// Chord has already been triggered
	if !shouldTrigger {
		return nil
	}

	// Get task states
	// 通过group 元信息获取task uuid对应的taskstate
	/*
		type TaskResult struct {
			Type  string      `bson:"type"`
			Value interface{} `bson:"value"`
		}

		type TaskState struct {
			TaskUUID  string        `bson:"_id"`
			State     string        `bson:"state"`
			Results   []*TaskResult `bson:"results"`
			Error     string        `bson:"error"`
		}
	*/
	taskStates, err := worker.server.GetBackend().GroupTaskStates(
		signature.GroupUUID,
		signature.GroupTaskCount,
	)
	if err != nil {
		log.ERROR.Printf(
			"Failed to get tasks states for group:[%s]. Task count:[%d]. The chord may not be triggered. Error:[%s]",
			signature.GroupUUID,
			signature.GroupTaskCount,
			err,
		)
		return nil
	}

	// Append group tasks' return values to chord task if it's not immutable
	for _, taskState := range taskStates {
		if !taskState.IsSuccess() {
			return nil
		}

		// 把分组任务的结果，加到 ChordCallback.Args 里
		if signature.ChordCallback.Immutable == false {
			// Pass results of the task to the chord callback
			for _, taskResult := range taskState.Results {
				signature.ChordCallback.Args = append(signature.ChordCallback.Args, tasks.Arg{
					Type:  taskResult.Type,
					Value: taskResult.Value,
				})
			}
		}
	}

	// Send the chord task
	// 触发 chord task
	_, err = worker.server.SendTask(signature.ChordCallback)
	if err != nil {
		return err
	}

	return nil
}

// taskFailed updates the task state and triggers error callbacks
// 失败回调
func (worker *Worker) taskFailed(signature *tasks.Signature, taskErr error) error {
	// Update task state to FAILURE
	if err := worker.server.GetBackend().SetStateFailure(signature, taskErr.Error()); err != nil {
		return fmt.Errorf("Set state to 'failure' for task %s returned error: %s", signature.UUID, err)
	}

	if worker.errorHandler != nil {
		worker.errorHandler(taskErr)
	} else {
		log.ERROR.Printf("Failed processing task %s. Error = %v", signature.UUID, taskErr)
	}

	// Trigger error callbacks
	for _, errorTask := range signature.OnError {
		// Pass error as a first argument to error callbacks
		args := append([]tasks.Arg{{
			Type:  "string",
			Value: taskErr.Error(),
		}}, errorTask.Args...)
		errorTask.Args = args
		worker.server.SendTask(errorTask)
	}

	if signature.StopTaskDeletionOnError {
		return errs.ErrStopTaskDeletion
	}

	return nil
}

// Returns true if the worker uses AMQP backend
func (worker *Worker) hasAMQPBackend() bool {
	_, ok := worker.server.GetBackend().(*amqp.Backend)
	return ok
}

// SetErrorHandler sets a custom error handler for task errors
// A default behavior is just to log the error after all the retry attempts fail
func (worker *Worker) SetErrorHandler(handler func(err error)) {
	worker.errorHandler = handler
}

//SetPreTaskHandler sets a custom handler func before a job is started
func (worker *Worker) SetPreTaskHandler(handler func(*tasks.Signature)) {
	worker.preTaskHandler = handler
}

//SetPostTaskHandler sets a custom handler for the end of a job
func (worker *Worker) SetPostTaskHandler(handler func(*tasks.Signature)) {
	worker.postTaskHandler = handler
}

//SetPreConsumeHandler sets a custom handler for the end of a job
func (worker *Worker) SetPreConsumeHandler(handler func(*Worker) bool) {
	worker.preConsumeHandler = handler
}

//GetServer returns server
func (worker *Worker) GetServer() *Server {
	return worker.server
}

//
func (worker *Worker) PreConsumeHandler() bool {
	if worker.preConsumeHandler == nil {
		return true
	}

	return worker.preConsumeHandler(worker)
}

func RedactURL(urlString string) string {
	u, err := url.Parse(urlString)
	if err != nil {
		return urlString
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}
