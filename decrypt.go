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

type AtomicCounter struct {
	value int32
	mutex sync.Mutex
}

func (c *AtomicCounter) Inc() int32 {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.value++
	return c.value
}
func (c *AtomicCounter) Dec() int32 {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.value--
	return c.value
}
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

func (s *Scheduler) Submit(task *Task) {
	groupKey := TaskGroupKey{AdamId: task.AdamId, Key: task.Key}

	queueVal, loaded := s.taskQueues.LoadOrStore(groupKey, make(chan *Task, 1000))
	if !loaded {
		s.processingCount.Store(groupKey, &AtomicCounter{})
	}
	taskQueue := queueVal.(chan *Task)

	taskQueue <- task
	go s.trySchedule(groupKey)

}

func (s *Scheduler) process(instance *DecryptInstance, taskQueue chan *Task, groupKey TaskGroupKey) {
	// [修改点1] 引入状态标志，用于控制 defer 中的行为，避免活锁
	var (
		limitReached bool // 标记是否触达并发限制
		isBroken     bool // 标记实例是否已损坏（如网络断开）
	)

	defer func() {
		// [修改点2] 如果实例已损坏，直接销毁，不再归还到池中
		if isBroken {
			_ = s.RemoveInstance(instance.id)
		} else {
			// 正常情况下归还实例到池
			if _, exists := s.instanceMap.Load(instance.id); exists {
				s.instances <- instance
			}
		}

		// 注意：不在这里Dec计数器
		// 因为这个实例可能会继续处理同样的Key

		// [修改点3] 仅在未触发并发限制时才尝试重新调度。
		// 如果是因为 limitReached 而退出，说明当前 Key 已满，
		// 应由正在处理该 Key 的其他实例在完成时触发调度，而不是由本实例立即触发死循环。
		if !limitReached && len(taskQueue) > 0 {
			go s.trySchedule(groupKey)
		}
	}()

	// Key变更时才涉及计数器操作
	if instance.currentKey == nil || *instance.currentKey != groupKey {
		counterVal, _ := s.processingCount.Load(groupKey)
		counter := counterVal.(*AtomicCounter)

		// 如果是切换Key，需要对旧Key的计数减一
		if instance.currentKey != nil {
			oldCounterVal, _ := s.processingCount.Load(*instance.currentKey)
			oldCounter := oldCounterVal.(*AtomicCounter)
			oldCounter.Dec()
		}

		// 新Key计数加一
		if counter.Get() >= s.maxConcurrent {
			// 超过限制，拒绝切换
			limitReached = true // [标记] 触达限制
			if instance.currentKey != nil {
				// 如果是切换Key失败，恢复旧Key的计数
				oldCounterVal, _ := s.processingCount.Load(*instance.currentKey)
				oldCounter := oldCounterVal.(*AtomicCounter)
				oldCounter.Inc()
			}
			return
		}

		// 切换上下文
		if err := instance.switchContext(groupKey); err != nil {
			// 切换失败
			isBroken = true // [标记] 上下文切换失败通常意味着连接已损坏
			// 恢复新Key计数（虽然还没成功切换，但上面已经Inc了吗？并没有，上面只是Get检查。
			// 等等，原始代码逻辑是先Get检查，再switch，成功后再Inc。
			// 让我们仔细看原始代码：
			// 原始代码：先 check >= maxConcurrent, 然后 switchContext, 然后 counter.Inc()
			// 我的修改保持了这个顺序，所以这里只需要恢复旧Key计数。

			if instance.currentKey != nil {
				oldCounterVal, _ := s.processingCount.Load(*instance.currentKey)
				oldCounter := oldCounterVal.(*AtomicCounter)
				oldCounter.Inc()
			}
			return
		}

		// 切换成功，新Key计数加一
		counter.Inc()
		instance.currentKey = &groupKey
	}

	// 处理任务循环
	for {
		select {
		case <-s.ctx.Done():
			return
		case task := <-taskQueue:
			if _, ok := s.instanceMap.Load(instance.id); !ok {
				task.Result <- &Result{Success: false, Error: fmt.Errorf("instance removed")}
				return
			}
			result, err := instance.decrypt(task.Payload)
			if err != nil {
				// 可选：如果 decrypt 也返回严重网络错误，也可以设置 isBroken = true 并 return
				// 这里为了最小化修改，暂且保留原样，仅返回错误给 Task
				task.Result <- &Result{Success: false, Error: err}
				return
			}
			task.Result <- &Result{Success: true, Data: result}
		default:
			return
		}
	}
}

// trySchedule不再负责计数
func (s *Scheduler) trySchedule(groupKey TaskGroupKey) {
	queueVal, exists := s.taskQueues.Load(groupKey)
	if !exists {
		return
	}
	taskQueue := queueVal.(chan *Task)

	select {
	case instance := <-s.instances:
		if !checkAvailableOnRegion(groupKey.AdamId, instance.region, false) {
			go s.trySchedule(groupKey)
			return
		}
		if _, ok := s.instanceMap.Load(instance.id); !ok {
			go s.trySchedule(groupKey)
			return
		}
		go s.process(instance, taskQueue, groupKey)
	default:
		return
	}
}

func (s *Scheduler) Shutdown() {
	s.cancel()
	s.instanceMap.Range(func(key, value interface{}) bool {
		instance := value.(*DecryptInstance)
		close(instance.stopCh)
		if instance.conn != nil {
			_ = instance.conn.Close()
		}
		return true
	})
}
