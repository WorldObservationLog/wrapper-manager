package main

import (
	"context"
	"fmt"
	"sync"
)

var SchedulerInstance *Scheduler

type TaskGroupKey struct {
	AdamId string
	Key    string
}

type Task struct {
	AdamId  string
	Key     string
	Payload []byte
	Result  chan *Result
}

type Result struct {
	Success bool
	Data    []byte
	Error   error
}

// AtomicCounter 包含一个互斥锁，以确保 IncIfLess 操作的原子性
type AtomicCounter struct {
	value int32
	mutex sync.Mutex
}

// IncIfLess 原子地检查当前值是否小于 max，如果是，则加一并返回 true。
// 否则，返回 false。
func (c *AtomicCounter) IncIfLess(max int32) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.value < max {
		c.value++
		return true
	}
	return false
}

// Dec 原子地减一
func (c *AtomicCounter) Dec() int32 {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.value--
	return c.value
}

// Get 原子地获取当前值
func (c *AtomicCounter) Get() int32 {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.value
}

type Scheduler struct {
	taskQueues      sync.Map // map[TaskGroupKey]chan *Task
	processingCount sync.Map // map[TaskGroupKey]*AtomicCounter
	instances       chan *DecryptInstance
	instanceMap     sync.Map // map[string]*DecryptInstance
	maxConcurrent   int32
	ctx             context.Context
	cancel          context.CancelFunc
}

func NewScheduler(maxConcurrent int32) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		instances:     make(chan *DecryptInstance, 1000),
		maxConcurrent: maxConcurrent,
		ctx:           ctx,
		cancel:        cancel,
	}
}

// getTaskQueue 获取或创建与 groupKey 关联的任务队列
func (s *Scheduler) getTaskQueue(groupKey TaskGroupKey) (chan *Task, bool) {
	queueVal, loaded := s.taskQueues.LoadOrStore(groupKey, make(chan *Task, 1000))
	return queueVal.(chan *Task), loaded
}

// getCounter 获取或创建与 groupKey 关联的并发计数器
func (s *Scheduler) getCounter(groupKey TaskGroupKey) *AtomicCounter {
	counterVal, _ := s.processingCount.LoadOrStore(groupKey, &AtomicCounter{})
	return counterVal.(*AtomicCounter)
}

func (s *Scheduler) Submit(task *Task) {
	groupKey := TaskGroupKey{AdamId: task.AdamId, Key: task.Key}
	taskQueue, _ := s.getTaskQueue(groupKey)
	counter := s.getCounter(groupKey) // 获取无锁计数器

	// 1. 总是先把任务放入队列
	taskQueue <- task

	// 2. *** 核心性能修复 ***
	// 只要当前活跃的 worker 数 *小于* 最大并发数，
	// 我们就尝试启动一个新的 'trySchedule' 协程来“扩大” worker 规模。
	// 'trySchedule' 内部的 'IncIfLess' 会原子地处理“竞态”，确保 worker 总数不会超过上限。
	// 一旦 counter 达到 maxConcurrent, 'Submit' 将停止创建新的 goroutine，
	// 完全依赖 'process' 协程的 'defer' 链来维持运行。
	if counter.Get() < s.maxConcurrent {
		go s.trySchedule(groupKey)
	}
}

// process 是实际的工作协程
// 它被调用时，就假定并发名额已经 *预定* 成功
func (s *Scheduler) process(instance *DecryptInstance, taskQueue chan *Task, groupKey TaskGroupKey, counter *AtomicCounter) {
	var isBroken bool // 标记实例是否已损坏（如网络断开）

	defer func() {
		// 1. 释放并发名额
		counter.Dec()

		if isBroken {
			// 2a. 实例已损坏，将其从系统中彻底移除
			_ = s.RemoveInstance(instance.id)
		} else {
			// 2b. 实例正常，归还到池中 (如果它没有在别处被移除)
			if _, exists := s.instanceMap.Load(instance.id); exists {
				s.instances <- instance
			}
		}

		// 3. 总是尝试触发一次新的调度
		//    以检查队列中是否还有剩余任务需要处理
		go s.trySchedule(groupKey)
	}()

	// --- 上下文切换 ---
	// 检查是否需要切换 Key
	if instance.currentKey == nil || *instance.currentKey != groupKey {
		if err := instance.switchContext(groupKey); err != nil {
			// 切换上下文失败，通常意味着连接已断开
			isBroken = true // 标记实例已损坏
			return          // defer 将处理后续
		}
		// switchContext 成功，在 decrypt_instance.go 中已更新 instance.currentKey
	}

	// --- 任务处理循环 ---
	// 循环处理，直到队列变空
	for {
		select {
		case <-s.ctx.Done(): // 调度器关闭
			isBroken = true // 认为实例已失效
			return
		case <-instance.stopCh: // 实例被移除
			isBroken = true // 实例已被外部移除
			return
		case task := <-taskQueue:
			// 再次检查实例是否存活
			if _, ok := s.instanceMap.Load(instance.id); !ok {
				isBroken = true // 实例已被移除
				task.Result <- &Result{Success: false, Error: fmt.Errorf("instance %s removed during processing", instance.id)}
				// 将任务放回队列头部 (或者也可以选择失败)
				// go s.Submit(task) // 重新提交
				return
			}

			// 执行解密
			result, err := instance.decrypt(task.Payload)
			if err != nil {
				// 解密失败（例如 I/O 错误），标记实例损坏
				isBroken = true
				task.Result <- &Result{Success: false, Error: err}
				return // 退出 process, defer 会处理
			}

			// 任务成功
			task.Result <- &Result{Success: true, Data: result}

		default:
			// taskQueue 已空，此 worker 协程完成其工作
			return
		}
	}
}

// trySchedule 是调度器，负责并发控制和分发工作
func (s *Scheduler) trySchedule(groupKey TaskGroupKey) {
	taskQueue, exists := s.getTaskQueue(groupKey)
	if !exists {
		return // 不应该发生，但作为保护
	}

	// 1. (轻量) 检查：如果队列为空，直接退出，避免不必要的锁竞争
	if len(taskQueue) == 0 {
		return
	}

	counter := s.getCounter(groupKey)

	// 2. 核心并发控制：原子地尝试获取一个“名额”
	if !counter.IncIfLess(s.maxConcurrent) {
		// 已达到此 Key 的并发上限
		// 正在运行的 process 协程在结束后会调用 trySchedule，所以现在退出是安全的
		return
	}

	// --- 成功获取名额 ---
	// 现在我们有责任启动一个 'process' 协程
	// 'process' 协程将负责在退出时调用 counter.Dec()

	// 3. 阻塞循环，直到获取一个 *有效* 且 *适用* 的实例
	for {
		select {
		case <-s.ctx.Done(): // 响应 scheduler 关闭
			counter.Dec() // 归还名额，因为我们没有启动 process
			return
		case instance := <-s.instances:
			// 成功从池中获取一个实例

			// 检查1：实例是否已被移除？
			if _, ok := s.instanceMap.Load(instance.id); !ok {
				// 实例已死 (可能在 RemoveInstance 中被关闭)
				// 丢弃此实例，循环重试，获取 *下一个* 实例
				continue
			}

			// 检查2：实例是否适用于这个 Key 的 region？
			// (假设 checkAvailableOnRegion 函数存在于别处)
			if !checkAvailableOnRegion(groupKey.AdamId, instance.region, false) {
				// 实例有效，但不适用。
				// 把它归还给池，以便其他 Key 可以使用。
				s.instances <- instance
				// 循环重试，获取 *下一个* 实例
				// 注意: 如果所有实例都不适用，这里可能会轻微空转
				// 但不会死锁，因为新实例可以随时加入
				continue
			}

			// 成功：实例有效且适用
			// 派发任务并退出 `trySchedule` 协程
			go s.process(instance, taskQueue, groupKey, counter)
			return

		} // end select
	} // end for
}

func (s *Scheduler) Shutdown() {
	s.cancel()
	s.instanceMap.Range(func(key, value interface{}) bool {
		instance := value.(*DecryptInstance)
		close(instance.stopCh) // 通知所有正在运行的 process 协程
		if instance.conn != nil {
			_ = instance.conn.Close()
		}
		return true
	})
	// 清空实例池
	close(s.instances)
}
