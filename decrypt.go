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
	defer func() {
		// 归还实例到池
		if _, exists := s.instanceMap.Load(instance.id); exists {
			s.instances <- instance
		}

		// 注意：不在这里Dec计数器
		// 因为这个实例可能会继续处理同样的Key

		if len(taskQueue) > 0 {
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
			// 切换失败，恢复计数
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
		if !checkSongAvailableOnRegion(groupKey.AdamId, instance.region) {
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
