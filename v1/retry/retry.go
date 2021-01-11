package retry

import (
	"fmt"
	"time"

	"github.com/RichardKnop/machinery/v1/log"
)

// Closure - a useful closure we can use when there is a problem
// connecting to the broker. It uses Fibonacci sequence to space out retry attempts
// 使用闭包，实现一个 fibonacci 递增形式的，时间等待
var Closure = func() func(chan int) {
	retryIn := 0
	//闭包函数
	fibonacci := Fibonacci()
	return func(stopChan chan int) {
		if retryIn > 0 {
			durationString := fmt.Sprintf("%vs", retryIn)
			duration, _ := time.ParseDuration(durationString)

			log.WARNING.Printf("Retrying in %v seconds", retryIn)

			select {
			//如果有停止信号，立马退出
			case <-stopChan:
				break
			//否则等duration时间后退出
			case <-time.After(duration):
				break
			}
		}
		//首次初始化
		retryIn = fibonacci()
	}
}
