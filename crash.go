package main

import (
	"sync"
	"time"
)

// 崩溃循环 / 退避策略参数。
const (
	// crashWindow：判定"是否陷入崩溃循环"的滑动窗口。窗口内崩溃达到 crashLoopThreshold 次即进入退避链。
	crashWindow = 5 * time.Minute
	// crashLoopThreshold：crashWindow 内触发退避所需的崩溃次数。
	crashLoopThreshold = 3
	// maxBackoffGen：退避代数上限。达到后彻底放弃重启，等待人工介入。
	// 退避序列：1,2,4,8,15,15 分钟（累计约 45 分钟），第 maxBackoffGen 次崩溃时放弃。
	maxBackoffGen = 6
	// healthWindow：负载均衡打分用——最近该窗口内崩溃过的实例会被降权。
	healthWindow = 15 * time.Minute
)

// crashRecord 以账号 id 为单位记录崩溃历史与退避代数。
// 关键点：状态挂在 id 上而非短命的 WrapperInstance 对象上，因此跨越任意次
// 进程重建都不会丢失；backoffGen 只增不减，唯一的清零路径是 clearCrash（成功就绪）。
type crashRecord struct {
	times      []time.Time
	backoffGen int
	givenUp    bool
}

var (
	crashRecords = make(map[string]*crashRecord)
	crashMu      sync.Mutex
)

func backoffDelay(gen int) time.Duration {
	d := time.Duration(1<<gen) * time.Minute
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

// recordCrash 记录 id 的一次崩溃，返回重启决策：
//   - giveUp=true：退避代数已达上限，彻底放弃，调用方应停止重启。
//   - delay>0    ：处于崩溃循环，等待 delay 后再重启。
//   - delay==0   ：偶发崩溃，可立即重启。
//
// backoffGen 只在成功就绪（clearCrash）时清零，因此持续启动失败的账号会稳步
// 走完退避链并最终放弃，不会因退避时长超过统计窗口而把计数刷掉重来。
func recordCrash(id string) (delay time.Duration, giveUp bool) {
	crashMu.Lock()
	defer crashMu.Unlock()

	now := time.Now()
	rec := crashRecords[id]
	if rec == nil {
		rec = &crashRecord{}
		crashRecords[id] = rec
	}

	if rec.givenUp {
		return 0, true
	}

	// 仅保留 healthWindow 内的崩溃时间（供打分复用），再追加本次。
	kept := rec.times[:0]
	for _, t := range rec.times {
		if now.Sub(t) < healthWindow {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	rec.times = kept

	// 已进入退避链：持续退避直到成功就绪(clearCrash)或达上限放弃。
	if rec.backoffGen > 0 {
		if rec.backoffGen >= maxBackoffGen {
			rec.givenUp = true
			return 0, true
		}
		delay = backoffDelay(rec.backoffGen)
		rec.backoffGen++
		return delay, false
	}

	// 尚未进入退避：统计 crashWindow 内崩溃次数。
	recent := 0
	for _, t := range kept {
		if now.Sub(t) < crashWindow {
			recent++
		}
	}
	if recent < crashLoopThreshold {
		return 0, false // 偶发崩溃，立即重启
	}
	// 判定为崩溃循环，进入退避链。
	delay = backoffDelay(0)
	rec.backoffGen = 1
	return delay, false
}

// clearCrash 在实例成功就绪后清空其崩溃历史与退避代数。
func clearCrash(id string) {
	crashMu.Lock()
	defer crashMu.Unlock()
	delete(crashRecords, id)
}

// crashPenalty 负载均衡打分用：最近崩溃越近、次数越多，惩罚分越高。
func crashPenalty(id string) int {
	crashMu.Lock()
	defer crashMu.Unlock()
	rec := crashRecords[id]
	if rec == nil {
		return 0
	}
	now := time.Now()
	penalty := 0
	for _, t := range rec.times {
		age := now.Sub(t)
		if age <= healthWindow {
			decay := 1.0 - (age.Seconds() / (15 * 60))
			if decay < 0 {
				decay = 0
			}
			penalty += int(200 * decay)
		}
	}
	return penalty
}

// isCrashUnhealthy 负载均衡打分用：healthWindow 内崩溃 >= 3 次视为不健康，选择时过滤掉。
func isCrashUnhealthy(id string) bool {
	crashMu.Lock()
	defer crashMu.Unlock()
	rec := crashRecords[id]
	if rec == nil {
		return false
	}
	now := time.Now()
	recent := 0
	for _, t := range rec.times {
		if age := now.Sub(t); age >= 0 && age <= healthWindow {
			recent++
		}
	}
	return recent >= 3
}
