package main

import (
	"container/list"
	"context"
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

type Scheduler struct {
	taskQueue *list.List
	queueLock sync.Mutex
	queueCond *sync.Cond

	instanceMap sync.Map // map[string]*DecryptInstance
	activeKeys  sync.Map // map[TaskGroupKey]bool

	ctx    context.Context
	cancel context.CancelFunc
}

func NewScheduler(maxConcurrent int32) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		taskQueue:   list.New(),
		instanceMap: sync.Map{},
		activeKeys:  sync.Map{},
		ctx:         ctx,
		cancel:      cancel,
	}
	s.queueCond = sync.NewCond(&s.queueLock)
	return s
}

// Submit 极其简单
func (s *Scheduler) Submit(task *Task) {
	s.queueLock.Lock()
	s.taskQueue.PushBack(task)
	s.queueLock.Unlock()
	s.queueCond.Signal() // 唤醒一个正在等待的 worker
}

// process 是每个 instance 的常驻协程
func (s *Scheduler) process(instance *DecryptInstance) {
	var isBroken bool
	defer func() {
		if isBroken {
			_ = s.RemoveInstance(instance.id) // 实例损坏，移除
		}
	}()

	// 外部循环：不断寻找 *新* 的 Key
	for {
		// 1. 等待并获取一个 *任何* 任务
		task := s.findWork(instance)
		if task == nil {
			return // 调度器或实例关闭
		}

		groupKey := TaskGroupKey{AdamId: task.AdamId, Key: task.Key}

		// 2. 尝试锁定 Key
		if _, loaded := s.activeKeys.LoadOrStore(groupKey, true); loaded {
			// 锁定失败：此 Key 已被其他 Instance 处理
			s.requeueTask(task)  // 立即放回队列
			s.queueCond.Signal() // 唤醒别人（可能包括自己）
			continue             // 立即寻找下一个任务
		}

		// --- 锁定成功 ---
		// 我们现在独占 groupKey，必须在处理完后释放
		// 使用一个匿名函数块来管理 defer
		func() {
			defer s.activeKeys.Delete(groupKey) // 确保锁被释放

			// 3. 检查上下文切换
			if instance.currentKey == nil || *instance.currentKey != groupKey {
				if err := instance.switchContext(groupKey); err != nil {
					isBroken = true     // 实例损坏
					s.requeueTask(task) // 放回任务
					task.Result <- &Result{Success: false, Error: err}
					return // 退出匿名函数, 释放锁
				}
			}

			// 4. 处理第一个任务
			result, err := instance.decrypt(task.Payload)
			if err != nil {
				isBroken = true
				s.requeueTask(task)
				task.Result <- &Result{Success: false, Error: err}
				return // 退出匿名函数, 释放锁
			}
			task.Result <- &Result{Success: true, Data: result}

			// 5. 内部“贪婪循环”：处理此 Key 的所有剩余任务
			for {
				// 非阻塞地寻找下一个 *相同 Key* 的任务
				nextTask := s.findSpecificWork(groupKey)
				if nextTask == nil {
					// 队列中已没有此 Key 的任务，工作完成
					return // 退出匿名函数, 释放锁
				}

				// 处理下一个任务
				result, err := instance.decrypt(nextTask.Payload)
				if err != nil {
					isBroken = true
					s.requeueTask(nextTask)
					nextTask.Result <- &Result{Success: false, Error: err}
					return // 实例损坏，退出匿名函数, 释放锁
				}
				nextTask.Result <- &Result{Success: true, Data: result}
			}
		}() // 匿名函数结束

		if isBroken {
			return // 实例已损坏，退出 worker 协程
		}

		// 检查 stopCh (非阻塞)
		select {
		case <-instance.stopCh:
			return // 实例被移除
		default:
			// 继续外部循环，寻找新 Key
		}
	}
}

// findSpecificWork (非阻塞)
// 在队列中 *搜索* 一个匹配 Key 的任务
func (s *Scheduler) findSpecificWork(groupKey TaskGroupKey) *Task {
	s.queueLock.Lock()
	defer s.queueLock.Unlock()

	// 遍历 list
	for e := s.taskQueue.Front(); e != nil; e = e.Next() {
		task := e.Value.(*Task)
		if task.AdamId == groupKey.AdamId && task.Key == groupKey.Key {
			s.taskQueue.Remove(e) // 找到并移除
			return task
		}
	}
	return nil // 未找到
}

// findWork (阻塞)
// 等待并拉取队列中的 *第一个* 任务
func (s *Scheduler) findWork(instance *DecryptInstance) *Task {
	s.queueLock.Lock()
	defer s.queueLock.Unlock()

	for {
		// 获取队列中的第一个任务
		if e := s.taskQueue.Front(); e != nil {
			task := s.taskQueue.Remove(e).(*Task)
			return task
		}

		// 等待：队列为空
		select {
		case <-s.ctx.Done():
			return nil // 调度器关闭
		case <-instance.stopCh:
			return nil // 实例被移除
		default:
			s.queueCond.Wait() // 等待 Submit 的信号
		}
	}
}

// requeueTask 将任务放回队列头部
func (s *Scheduler) requeueTask(task *Task) {
	s.queueLock.Lock()
	s.taskQueue.PushFront(task)
	s.queueLock.Unlock()
	s.queueCond.Signal()
}

func (s *Scheduler) Shutdown() {
	s.cancel()
	s.queueCond.Broadcast() // 唤醒所有等待的 worker 让他们退出
	s.instanceMap.Range(func(key, value interface{}) bool {
		instance := value.(*DecryptInstance)
		close(instance.stopCh)
		if instance.conn != nil {
			_ = instance.conn.Close()
		}
		return true
	})
}
