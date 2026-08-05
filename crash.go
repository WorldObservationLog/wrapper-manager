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
	// maxBackoffGen：退避代数上限。达到后固定以最长间隔继续重试（永不放弃），
	// 等待底层故障（如网络/凭据临时失效）恢复后自动拉起。
	maxBackoffGen = 8
	// backoffBase：退避起始间隔。序列：20s, 40s, 80s, 2m40s, 5m20s, 10m40s, ... 封顶 maxBackoffDelay。
	backoffBase = 20 * time.Second
	// maxBackoffDelay：退避间隔封顶。
	maxBackoffDelay = 10 * time.Minute
	// healthWindow：负载均衡打分用——最近该窗口内崩溃过的实例会被降权。
	healthWindow = 15 * time.Minute
)

// crashRecord 以账号 id 为单位记录崩溃历史与退避代数。
// 关键点：状态挂在 id 上而非短命的 WrapperInstance 对象上，因此跨越任意次
// 进程重建都不会丢失；backoffGen 在成功就绪（clearCrash）时清零。
// 本记录永不"永久放弃"：即使达到退避代数上限，也固定以 maxBackoffDelay 间隔
// 继续尝试重启，直到实例真正 ready 成功（clearCrash）为止。
type crashRecord struct {
	times      []time.Time
	backoffGen int
}

var (
	crashRecords = make(map[string]*crashRecord)
	crashMu      sync.Mutex
)

func backoffDelay(gen int) time.Duration {
	d := time.Duration(1<<gen) * backoffBase
	if d > maxBackoffDelay {
		d = maxBackoffDelay
	}
	return d
}

// recordCrash 记录 id 的一次崩溃，返回重启等待间隔：
//   - delay==0：偶发崩溃，可立即重启。
//   - delay>0 ：处于崩溃循环，等待 delay 后再重启。
//
// backoffGen 只在成功就绪（clearCrash）时清零，因此持续启动失败的账号会稳步
// 走完退避链并按最长间隔持续重试，不会因退避时长超过统计窗口而把计数刷掉重来，
// 也不会永久停止——底层故障恢复后一旦 ready 成功即清空历史回到快速路径。
func recordCrash(id string) time.Duration {
	crashMu.Lock()
	defer crashMu.Unlock()

	now := time.Now()
	rec := crashRecords[id]
	if rec == nil {
		rec = &crashRecord{}
		crashRecords[id] = rec
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

	// 已进入退避链：持续退避直到成功就绪(clearCrash)，达上限后固定最长间隔。
	if rec.backoffGen > 0 {
		if rec.backoffGen >= maxBackoffGen {
			return maxBackoffDelay
		}
		delay := backoffDelay(rec.backoffGen)
		rec.backoffGen++
		return delay
	}

	// 尚未进入退避：统计 crashWindow 内崩溃次数。
	recent := 0
	for _, t := range kept {
		if now.Sub(t) < crashWindow {
			recent++
		}
	}
	if recent < crashLoopThreshold {
		return 0 // 偶发崩溃，立即重启
	}
	// 判定为崩溃循环，进入退避链。
	rec.backoffGen = 1
	return backoffDelay(0)
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
