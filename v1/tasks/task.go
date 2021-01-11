package tasks

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"

	opentracing "github.com/opentracing/opentracing-go"
	opentracing_ext "github.com/opentracing/opentracing-go/ext"
	opentracing_log "github.com/opentracing/opentracing-go/log"

	"github.com/RichardKnop/machinery/v1/log"
)

// ErrTaskPanicked ...
var ErrTaskPanicked = errors.New("Invoking task caused a panic")

// Task wraps a signature and methods used to reflect task arguments and
// return values after invoking the task
type Task struct {
	TaskFunc   reflect.Value
	UseContext bool
	Context    context.Context
	Args       []reflect.Value
}

type signatureCtxType struct{}

var signatureCtx signatureCtxType

// SignatureFromContext gets the signature from the context
// 从context获取signature
func SignatureFromContext(ctx context.Context) *Signature {
	if ctx == nil {
		return nil
	}

	// 为了避免多个包同时使用context而带来冲突，key不建议使用string或其他内置类型，所以建议自定义key类型.
	v := ctx.Value(signatureCtx)
	if v == nil {
		return nil
	}

	signature, _ := v.(*Signature)
	return signature
}

// NewWithSignature is the same as New but injects the signature
// taskFunc是server.registeredTasks.Load(name)返回的数据
func NewWithSignature(taskFunc interface{}, signature *Signature) (*Task, error) {
	args := signature.Args
	ctx := context.Background()
	//ctx添加value
	ctx = context.WithValue(ctx, signatureCtx, signature)
	task := &Task{
		TaskFunc: reflect.ValueOf(taskFunc),
		Context:  ctx,
	}

	taskFuncType := reflect.TypeOf(taskFunc)
	//NumIn 返回func类型的参数个数，如果不是函数，将会panic
	if taskFuncType.NumIn() > 0 {
		// 返回func类型的第i个参数的类型，如非函数或者i不在[0, NumIn())内将会panic
		arg0Type := taskFuncType.In(0)
		// 如果地一个参数是 context
		if IsContextType(taskFuncType.In(0)) {
			task.UseContext = true
		}
	}

	if err := task.ReflectArgs(args); err != nil {
		return nil, fmt.Errorf("Reflect task args error: %s", err)
	}

	return task, nil
}

// New tries to use reflection to convert the function and arguments
// into a reflect.Value and prepare it for invocation
func New(taskFunc interface{}, args []Arg) (*Task, error) {
	task := &Task{
		TaskFunc: reflect.ValueOf(taskFunc),
		Context:  context.Background(),
	}

	taskFuncType := reflect.TypeOf(taskFunc)
	if taskFuncType.NumIn() > 0 {
		arg0Type := taskFuncType.In(0)
		if IsContextType(arg0Type) {
			task.UseContext = true
		}
	}

	if err := task.ReflectArgs(args); err != nil {
		return nil, fmt.Errorf("Reflect task args error: %s", err)
	}

	return task, nil
}

// Call attempts to call the task with the supplied arguments.
//
// `err` is set in the return value in two cases:
// 1. The reflected function invocation panics (e.g. due to a mismatched
//    argument list).
// 2. The task func itself returns a non-nil error.
// 反射执行panic，比如缺少参数， task 返回了一个 non-nil error
func (t *Task) Call() (taskResults []*TaskResult, err error) {
	// retrieve the span from the task's context and finish it as soon as this function returns
	if span := opentracing.SpanFromContext(t.Context); span != nil {
		defer span.Finish()
	}

	defer func() {
		// Recover from panic and set err.
		if e := recover(); e != nil {
			switch e := e.(type) {
			default:
				err = ErrTaskPanicked
			case error:
				err = e
			case string:
				err = errors.New(e)
			}

			// mark the span as failed and dump the error and stack trace to the span
			if span := opentracing.SpanFromContext(t.Context); span != nil {
				opentracing_ext.Error.Set(span, true)
				span.LogFields(
					opentracing_log.Error(err),
					opentracing_log.Object("stack", string(debug.Stack())),
				)
			}

			// Print stack trace
			log.ERROR.Printf("%s", debug.Stack())
		}
	}()

	args := t.Args

	if t.UseContext {
		ctxValue := reflect.ValueOf(t.Context)
		args = append([]reflect.Value{ctxValue}, args...)
	}

	// Invoke the task
	// 通过反射调用TaskFun
	results := t.TaskFunc.Call(args)

	// Task must return at least a value
	if len(results) == 0 {
		return nil, ErrTaskReturnsNoValue
	}

	// Last returned value
	// 最后一个返回值
	lastResult := results[len(results)-1]

	// If the last returned value is not nil, it has to be of error type, if that
	// is not the case, return error message, otherwise propagate the task error
	// to the caller
	// IsNil报告v持有的值是否为nil。v持有的值的分类必须是通道、函数、接口、映射、指针、切片之一；
	if !lastResult.IsNil() {
		// If the result implements Retriable interface, return instance of Retriable
		retriableErrorInterface := reflect.TypeOf((*Retriable)(nil)).Elem()
		if lastResult.Type().Implements(retriableErrorInterface) {
			return nil, lastResult.Interface().(ErrRetryTaskLater)
		}

		// Otherwise, check that the result implements the standard error interface,
		// if not, return ErrLastReturnValueMustBeError error
		errorInterface := reflect.TypeOf((*error)(nil)).Elem()
		if !lastResult.Type().Implements(errorInterface) {
			return nil, ErrLastReturnValueMustBeError
		}

		// Return the standard error
		return nil, lastResult.Interface().(error)
	}

	// Convert reflect values to task results
	taskResults = make([]*TaskResult, len(results)-1)
	for i := 0; i < len(results)-1; i++ {
		val := results[i].Interface()
		typeStr := reflect.TypeOf(val).String()
		// type 返回结果的type类型
		// value 返回结果转接口类型
		taskResults[i] = &TaskResult{Type: typeStr, Value: val}
	}

	return taskResults, err
}

// ReflectArgs converts []TaskArg to []reflect.Value
// 转换 signature.Args 为 []reflect.Value
func (t *Task) ReflectArgs(args []Arg) error {
	argValues := make([]reflect.Value, len(args))

	// 遍历args
	for i, arg := range args {
		// 如果argValue是切片  通过创建 theValue = reflect.MakeSlice(theType, strs.Len(), strs.Len()) 循环 theValue.Index(i).SetString(strValue)
		// 如果argValue非切片，如果int型，通过 theValue := reflect.New(theType)，theValue.Elem().SetInt(intValue), 返回 theValue.Elem()
		// argValue 是 reflect.Value类型
		/*
			Args: []tasks.Arg{
				{
					Type:  "int64",
					Value: 1,
				},
				{
					Type:  "int64",
					Value: 1,
				},
			}
			Args: []tasks.Arg{
				{
					Type:  "[]int64",
					Value: []int64{1, 2},
				},
			},
		*/
		argValue, err := ReflectValue(arg.Type, arg.Value)
		if err != nil {
			return err
		}
		argValues[i] = argValue
	}

	t.Args = argValues
	return nil
}
