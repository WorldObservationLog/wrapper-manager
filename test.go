package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	log "github.com/sirupsen/logrus"
)

// testStatus 单个实例的测试判定。
type testStatus int

const (
	testOK      testStatus = iota // 成功监听 m3u8，视为正常
	testFailed                    // 登录失败 / pty 启动失败 / 进程未就绪就退出
	testTimeout                   // 超时内未出现任何决定性信号
	test2FA                       // 需要 2FA，无法在无人值守下完成，视为异常
	testNoSub                     // 无有效订阅，视为异常
)

func (s testStatus) String() string {
	switch s {
	case testOK:
		return "OK"
	case testFailed:
		return "FAILED"
	case testTimeout:
		return "TIMEOUT"
	case test2FA:
		return "NEED_2FA"
	case testNoSub:
		return "NO_SUBSCRIPTION"
	default:
		return "UNKNOWN"
	}
}

// instanceTestResult 单个实例的测试结果。
type instanceTestResult struct {
	Id     string
	Region string
	Status testStatus
	Detail string
}

// testInstanceIds 收集待测实例 id。
//
//	source == "dir" : 扫描 data/wrapper/rootfs/data/instances/ 下的子目录
//	source == "json": 读取 data/instances.json（默认）
func testInstanceIds(source string) ([]string, error) {
	if source == "dir" {
		base := "data/wrapper/rootfs/data/instances"
		entries, err := os.ReadDir(base)
		if err != nil {
			return nil, fmt.Errorf("read instances dir %q: %w", base, err)
		}
		var ids []string
		for _, e := range entries {
			if e.IsDir() {
				ids = append(ids, e.Name())
			}
		}
		return ids, nil
	}

	content, err := os.ReadFile("data/instances.json")
	if err != nil {
		return nil, fmt.Errorf("read instances.json: %w", err)
	}
	var instances []*WrapperInstance
	if err := json.Unmarshal(content, &instances); err != nil {
		return nil, fmt.Errorf("parse instances.json: %w", err)
	}
	var ids []string
	for _, inst := range instances {
		ids = append(ids, inst.Id)
	}
	return ids, nil
}

// testOneInstance 启动单个 wrapper 进程，观察其 stdout 判定能否正常启动。
// 与 WrapperStart 使用相同的启动参数，但完全独立——不接入 GlobalManager /
// 自动重启 / 解密客户端。无论结果如何，返回前都会杀掉该进程并回收端口。
func testOneInstance(id string, timeout time.Duration) instanceTestResult {
	res := instanceTestResult{Id: id}

	decryptPort := GenerateUniquePort()
	m3u8Port := GenerateUniquePort()
	defer ReleasePort(decryptPort)
	defer ReleasePort(m3u8Port)

	args := []string{
		"-H0.0.0.0",
		fmt.Sprintf("-B%s", "/data/instances/"+id),
		fmt.Sprintf("-D%d", decryptPort),
		fmt.Sprintf("-M%d", m3u8Port),
		fmt.Sprintf("-I%s", DeviceInfo),
	}
	if PROXY != "" {
		args = append(args, fmt.Sprintf("-P%s", PROXY))
	}

	cmd := exec.Command("./wrapper", args...)
	cmd.Dir = "data/wrapper/"

	ptmx, err := pty.Start(cmd)
	if err != nil {
		res.Status = testFailed
		res.Detail = fmt.Sprintf("pty start: %v", err)
		return res
	}
	// 收尾：关闭 pty、杀进程、回收僵尸。
	defer func() {
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	// 扫描输出，报告首个决定性信号（与 handleOutput 的状态机同源）。
	resultCh := make(chan instanceTestResult, 1)
	go func() {
		defer func() {
			// 扫描 goroutine 兜底：panic 也报一次失败，避免 select 永久阻塞到超时。
			if r := recover(); r != nil {
				select {
				case resultCh <- instanceTestResult{Id: id, Status: testFailed, Detail: fmt.Sprintf("scan panic: %v", r)}:
				default:
				}
			}
		}()
		scanner := bufio.NewScanner(ptmx)
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.Contains(line, "[!] listening m3u8 request on"):
				resultCh <- instanceTestResult{Id: id, Status: testOK, Region: readRegion(id)}
				return
			case strings.Contains(line, "[!] login failed"):
				resultCh <- instanceTestResult{Id: id, Status: testFailed, Detail: "login failed"}
				return
			case strings.Contains(line, "No Active Subscription"):
				resultCh <- instanceTestResult{Id: id, Status: testNoSub, Detail: "no active subscription"}
				return
			case strings.Contains(line, "Waiting for input..."):
				resultCh <- instanceTestResult{Id: id, Status: test2FA, Detail: "2FA required"}
				return
			}
		}
		// scanner 结束意味着进程已退出，却没出现任何就绪 / 失败信号。
		resultCh <- instanceTestResult{Id: id, Status: testFailed, Detail: "process exited without ready signal"}
	}()

	select {
	case r := <-resultCh:
		return r
	case <-time.After(timeout):
		res.Status = testTimeout
		res.Detail = fmt.Sprintf("no signal within %v", timeout)
		return res
	}
}

// readRegion 就绪后读取 wrapper 写出的 STOREFRONT_ID 并解析为区域码，失败返回空串。
func readRegion(id string) string {
	sf, err := os.ReadFile(fmt.Sprintf("data/wrapper/rootfs/data/instances/%s/STOREFRONT_ID", id))
	if err != nil {
		return ""
	}
	region, err := parseStorefrontID(string(sf))
	if err != nil {
		return ""
	}
	return region
}

// testConfig 测试模式参数。
type testConfig struct {
	source      string        // "json"(默认) 或 "dir"
	apply       bool          // 结束后按结果重写 instances.json
	timeout     time.Duration // 单实例启动超时
	concurrency int           // 并行测试的实例数
}

// RunInstanceTest 测试实例能否正常启动，结束后输出汇总。
// 返回 true 表示所有被测实例均正常；有任一异常返回 false（供调用方决定退出码）。
func RunInstanceTest(cfg testConfig) bool {
	ids, err := testInstanceIds(cfg.source)
	if err != nil {
		log.Fatalf("failed to collect instance ids: %v", err)
	}
	if len(ids) == 0 {
		log.Warn("no instances to test")
		return true
	}

	concurrency := cfg.concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	log.Infof("Testing %d instance(s) from %q, timeout %v each, concurrency %d ...",
		len(ids), cfg.source, cfg.timeout, concurrency)

	results := make([]instanceTestResult, len(ids))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var done atomic.Int32

	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			r := testOneInstance(id, cfg.timeout)
			results[i] = r
			detail := r.Detail
			if detail != "" {
				detail = " - " + detail
			}
			log.Infof("[%d/%d] %s => %s%s", done.Add(1), len(ids), id, r.Status, detail)
		}(i, id)
	}
	wg.Wait()

	printTestSummary(results)

	if cfg.apply {
		applyTestResults(results)
	}

	for _, r := range results {
		if r.Status != testOK {
			return false
		}
	}
	return true
}

func printTestSummary(results []instanceTestResult) {
	var okCount, badCount int
	fmt.Println()
	fmt.Println("==================== TEST RESULT ====================")
	for _, r := range results {
		mark := "✓"
		if r.Status != testOK {
			mark = "✗"
			badCount++
		} else {
			okCount++
		}
		region := r.Region
		if region == "" {
			region = "-"
		}
		detail := r.Detail
		if detail != "" {
			detail = "  (" + detail + ")"
		}
		fmt.Printf("  %s  %-38s %-16s region=%s%s\n", mark, r.Id, r.Status, region, detail)
	}
	fmt.Println("-----------------------------------------------------")
	fmt.Printf("  Total: %d   OK: %d   Abnormal: %d\n", len(results), okCount, badCount)
	fmt.Println("=====================================================")
}

// applyTestResults 按测试结果重写 instances.json：仅保留状态为 OK 的实例。
// 其余状态（FAILED / TIMEOUT / NEED_2FA / NO_SUBSCRIPTION）均视为异常并移除。
// 序列化格式与运行期 Save/StopAll 保持一致（[]*WrapperInstance，仅 id/region 落盘）。
func applyTestResults(results []instanceTestResult) {
	keep := make([]*WrapperInstance, 0, len(results))
	for _, r := range results {
		if r.Status == testOK {
			keep = append(keep, &WrapperInstance{Id: r.Id, Region: r.Region})
		}
	}
	data, err := json.Marshal(keep)
	if err != nil {
		log.Errorf("apply: marshal instances failed: %v", err)
		return
	}
	if err := AtomicWriteFile("data/instances.json", data); err != nil {
		log.Errorf("apply: write instances.json failed: %v", err)
		return
	}
	log.Infof("apply: instances.json rewritten — kept %d healthy, removed %d abnormal",
		len(keep), len(results)-len(keep))
}
