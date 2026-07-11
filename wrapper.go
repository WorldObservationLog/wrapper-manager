package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/artdarek/go-unzip"
	"github.com/creack/pty"
	"github.com/gofrs/uuid/v5"
	log "github.com/sirupsen/logrus"
)

func parseStorefrontID(id string) (string, error) {
	sfID, err := strconv.Atoi(strings.Split(id, "-")[0])
	if err != nil {
		return "", fmt.Errorf("invalid storefront id %q: %w", id, err)
	}
	type StorefrontMapping struct {
		Name         string `json:"name"`
		Code         string `json:"code"`
		StorefrontId int    `json:"storefrontId"`
	}
	var mapping []StorefrontMapping
	file, err := os.ReadFile("data/storefront_ids.json")
	if err != nil {
		return "", fmt.Errorf("read storefront_ids.json: %w", err)
	}
	if err := json.Unmarshal(file, &mapping); err != nil {
		return "", fmt.Errorf("parse storefront_ids.json: %w", err)
	}
	for _, element := range mapping {
		if element.StorefrontId == sfID {
			return element.Code, nil
		}
	}
	return "", fmt.Errorf("storefront id %d not found in mapping", sfID)
}

func PrepareWrapper(mirror bool) {
	var wrapperZipPath string
	if runtime.GOARCH == "amd64" {
		wrapperZipPath = "data/wrapper-x86_64.zip"
	} else if runtime.GOARCH == "arm64" {
		wrapperZipPath = "data/wrapper-arm64.zip"
	}
	if _, err := os.Stat("data/wrapper/wrapper"); os.IsNotExist(err) {
		if _, err := os.Stat(wrapperZipPath); os.IsNotExist(err) {
			DownloadWrapperRelease(mirror)
		}
		err = unzip.New(wrapperZipPath, "data/wrapper").Extract()
		if err != nil {
			panic(err)
		}
		err = os.Chmod("data/wrapper/wrapper", 0755)
		if err != nil {
			panic(err)
		}
	}
}

func WrapperInitial(account string, password string) {
	id := uuid.NewV5(uuid.FromStringOrNil("77777777-7777-7777-7777-77777777"), account)
	err := os.MkdirAll("data/wrapper/rootfs/data/instances/"+id.String(), 0755)
	if err != nil {
		log.Errorf("failed to create instance dir for %s: %v", id.String(), err)
		go LoginFailedHandler(id.String())
		return
	}

	instance := WrapperInstance{
		Id:          id.String(),
		DecryptPort: GenerateUniquePort(),
		M3U8Port:    GenerateUniquePort(),
		M3U8Health:  100,
		NoRestart:   true,
	}

	args := []string{
		"-H0.0.0.0",
		fmt.Sprintf("-L%s:%s", account, password),
		fmt.Sprintf("-B%s", "/data/instances/"+instance.Id),
		fmt.Sprintf("-D%d", instance.DecryptPort),
		fmt.Sprintf("-M%d", instance.M3U8Port),
		fmt.Sprintf("-I%s", DeviceInfo),
		"-F",
	}

	if PROXY != "" {
		args = append(args, fmt.Sprintf("-P%s", PROXY))
	}

	cmd := exec.Command("./wrapper", args...)
	cmd.Dir = "data/wrapper/"

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Errorf("failed to start wrapper for %s: %v", instance.Id, err)
		ReleasePort(instance.DecryptPort)
		ReleasePort(instance.M3U8Port)
		go LoginFailedHandler(instance.Id)
		return
	}
	defer func() { _ = ptmx.Close() }()

	instance.Cmd = cmd
	go handleOutput(ptmx, &instance)

	err = cmd.Wait()
	if err != nil {
		log.Warnf("Wrapper exited with error: %v\n", err)
	}

	go wrapperDown(&instance)
}

func WrapperStart(id string) {
	instance := WrapperInstance{
		Id:          id,
		DecryptPort: GenerateUniquePort(),
		M3U8Port:    GenerateUniquePort(),
		M3U8Health:  100,
		NoRestart:   false,
	}

	args := []string{
		"-H0.0.0.0",
		fmt.Sprintf("-B%s", "/data/instances/"+id),
		fmt.Sprintf("-D%d", instance.DecryptPort),
		fmt.Sprintf("-M%d", instance.M3U8Port),
		fmt.Sprintf("-I%s", DeviceInfo),
	}

	if PROXY != "" {
		args = append(args, fmt.Sprintf("-P%s", PROXY))
	}

	cmd := exec.Command("./wrapper", args...)
	cmd.Dir = "data/wrapper/"

	ptmx, err := pty.Start(cmd)
	if err != nil {
		// 启动失败：进程未运行，直接走 wrapperDown 触发受崩溃环保护的重启逻辑。
		log.Errorf("failed to start wrapper for %s: %v", instance.Id, err)
		go wrapperDown(&instance)
		return
	}
	defer func() { _ = ptmx.Close() }()

	instance.Cmd = cmd
	go handleOutput(ptmx, &instance)

	_ = cmd.Wait()

	go wrapperDown(&instance)
}

func handleOutput(reader io.Reader, instance *WrapperInstance) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("[wrapper %s] panic in handleOutput: %v", strings.Split(instance.Id, "-")[0], r)
		}
	}()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "__") || !strings.HasPrefix(line, "WARNING") {
			log.Debug(fmt.Sprintf("[wrapper %s]", strings.Split(instance.Id, "-")[0]), line)
		}

		if strings.Contains(line, "Waiting for input...") {
			go Login2FAHandler(instance.Id)
		}
		if strings.Contains(line, "[!] listening m3u8 request on") {
			go wrapperReady(instance)
		}
		if strings.Contains(line, "[!] login failed") {
			go LoginFailedHandler(instance.Id)
		}
		if strings.Contains(line, "No Active Subscription") {
			go NoSubscriptionHandler(instance)
		}
	}
}

func wrapperReady(instance *WrapperInstance) {
	storefrontID, err := os.ReadFile(fmt.Sprintf("data/wrapper/rootfs/data/instances/%s/STOREFRONT_ID", instance.Id))
	if err != nil {
		log.Errorf("[wrapper %s] failed to read STOREFRONT_ID, killing to trigger restart: %v",
			strings.Split(instance.Id, "-")[0], err)
		killProcess(instance)
		return
	}
	region, err := parseStorefrontID(string(storefrontID))
	if err != nil {
		log.Errorf("[wrapper %s] failed to parse STOREFRONT_ID, killing to trigger restart: %v",
			strings.Split(instance.Id, "-")[0], err)
		killProcess(instance)
		return
	}
	instance.Lock()
	instance.Region = region
	instance.Unlock()

	// 若进程已在我们初始化期间退出，放弃注册——wrapperDown 会负责重启。
	// 避免 wrapperReady/wrapperDown 竞态导致幽灵实例（Ready=true、端口已释放、进程已死）。
	if instance.Cmd == nil || instance.Cmd.ProcessState != nil {
		log.Warnf("[wrapper %s] process already exited before wrapperReady completed, aborting registration",
			strings.Split(instance.Id, "-")[0])
		return
	}

	// Initialize DecryptClient
	client, err := NewDecryptClient(instance.DecryptPort)
	if err != nil {
		log.Errorf("failed to create decrypt client for instance %s, killing to trigger restart: %v", instance.Id, err)
		killProcess(instance)
		return
	}
	instance.SetClient(client)
	instance.SetReady(true)
	// 成功就绪：清空该账号的崩溃历史与退避代数，下次崩溃从最短间隔重新计数。
	clearCrash(instance.Id)

	GlobalManager.Add(instance)

	instance.NoRestart = false
	go LoginDoneHandler(instance.Id)
	log.Info(fmt.Sprintf("[wrapper %s]", strings.Split(instance.Id, "-")[0]), " Wrapper ready")

	// Check readiness
	readyCount := 0
	list := GlobalManager.List()
	for _, inst := range list {
		if inst.IsReady() {
			readyCount++
		}
	}
	if readyCount == ShouldStartInstances {
		Ready = true
	}
}

func wrapperDown(instance *WrapperInstance) {
	log.Info(fmt.Sprintf("[wrapper %s]", strings.Split(instance.Id, "-")[0]), " Wrapper Down")

	if client := instance.GetClient(); client != nil {
		client.Close()
		instance.SetClient(nil)
	}
	instance.SetReady(false)

	// 进程已退出，其占用的端口可以归还。无论是否重启都先释放：
	// 重启路径（WrapperStart）会重新分配新端口，否则会造成端口只增不减的泄漏。
	ReleasePort(instance.DecryptPort)
	ReleasePort(instance.M3U8Port)

	// Only remove from GlobalManager if we are genuinely not restarting (like on Logout or initial Failure).
	// If it's going to restart, we keep it in the manager so it isn't lost from instances.json dumps.
	// But it is no longer marked as Ready, so it won't receive traffic.
	if instance.NoRestart {
		GlobalManager.Remove(instance.Id)
	}

	if !instance.NoRestart {
		// 崩溃决策集中在 crashRecords（按账号 id 持久于内存），跨进程重建不丢失。
		delay, giveUp := recordCrash(instance.Id)
		if giveUp {
			// 退避代数已达上限：判定该账号无法恢复（如凭据失效、机器无法访问 Apple），
			// 停止自动重启，保留数据等待人工介入。不从 GlobalManager 移除，保留在
			// instances.json 中，重启进程后仍会尝试拉起。
			log.Errorf("Wrapper %s exceeded max restart backoff, giving up auto-restart. Manual intervention required.", instance.Id)
			GlobalManager.Save()
			return
		}
		if delay > 0 {
			log.Errorf("Wrapper %s is crash-looping, retrying in %v", instance.Id, delay)
			GlobalManager.Save()
			go func() {
				time.Sleep(delay)
				WrapperStart(instance.Id)
			}()
			return
		}
		go WrapperStart(instance.Id)
	} else {
		GlobalManager.Save()
	}
}

func KillWrapper(id string) error {
	instance := GlobalManager.Get(id)
	if instance == nil {
		return fmt.Errorf("instance %s not found", id)
	}
	return killProcess(instance)
}

// killProcess 直接杀掉实例进程，无需先在 GlobalManager 中注册。
// 用于实例尚未 Add 到管理器（如 wrapperReady 早期失败）时触发 wrapperDown 重启流程。
func killProcess(instance *WrapperInstance) error {
	if instance.Cmd == nil {
		return fmt.Errorf("instance %s cmd is nil", instance.Id)
	}
	if instance.Cmd.Process == nil {
		return fmt.Errorf("instance %s process is nil", instance.Id)
	}
	return instance.Cmd.Process.Kill()
}

func provide2FACode(id string, code string) error {
	if err := os.WriteFile("data/wrapper/rootfs/data/instances/"+id+"/2fa.txt", []byte(code), 0644); err != nil {
		return fmt.Errorf("write 2fa code for %s: %w", id, err)
	}
	return nil
}

func RemoveWrapperData(id string) {
	if err := os.RemoveAll("data/wrapper/rootfs/data/instances/" + id); err != nil {
		log.Errorf("failed to remove wrapper data for %s: %v", id, err)
	}
}

func DownloadWrapperRelease(mirror bool) {
	var resp *http.Response
	if runtime.GOARCH == "amd64" {
		var err error
		resp, err = GetHttpClient().Get("https://api.github.com/repos/WorldObservationLog/wrapper/releases/latest")
		if err != nil {
			panic(err)
		}
	} else if runtime.GOARCH == "arm64" {
		var err error
		resp, err = GetHttpClient().Get("https://api.github.com/repos/WorldObservationLog/wrapper/releases/tags/Wrapper.arm64.latest")
		if err != nil {
			panic(err)
		}
	} else {
		panic("unsupported arch")
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	_, err := io.Copy(buf, resp.Body)
	var info struct {
		Assets []map[string]interface{} `json:"assets"`
	}
	err = json.Unmarshal([]byte(buf.String()), &info)
	if err != nil {
		panic(err)
	}
	downloadUrl := info.Assets[0]["browser_download_url"]
	if mirror {
		downloadUrl = strings.Replace(downloadUrl.(string), "github.com", "gh-proxy.com/github.com", -1)
	}
	wrapperResp, err := GetHttpClient().Get(downloadUrl.(string))
	if err != nil {
		panic(err)
	}
	defer wrapperResp.Body.Close()
	binary, err := io.ReadAll(wrapperResp.Body)
	if runtime.GOARCH == "amd64" {
		err = os.WriteFile("data/wrapper-x86_64.zip", binary, 0644)
	} else if runtime.GOARCH == "arm64" {
		err = os.WriteFile("data/wrapper-arm64.zip", binary, 0644)
	} else {
		panic("unsupported arch")
	}

	if err != nil {
		panic(err)
	}
}

func DownloadStorefrontIds() {
	resp, err := GetHttpClient().Get("https://gist.githubusercontent.com/BrychanOdlum/2208578ba151d1d7c4edeeda15b4e9b1/raw/8f01e4a4cb02cf97a48aba4665286b0e8de14b8e/storefrontmappings.json")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	ids, err := io.ReadAll(resp.Body)
	err = os.WriteFile("data/storefront_ids.json", ids, 0644)
	if err != nil {
		panic(err)
	}
}

func NoSubscriptionHandler(instance *WrapperInstance) {
	if instance.NoRestart {
		go LoginFailedHandler(instance.Id)
	} else {
		// Just stop restarting until user manually checks. Removing it wipes the credentials which might be a temporary error.
		log.Warnf("Instance %s reported No Active Subscription. Stopping auto-restart but keeping data.", instance.Id)
		instance.NoRestart = true
		GlobalManager.Save()
	}
}
