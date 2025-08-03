package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/artdarek/go-unzip"
	"github.com/creack/pty"
	"github.com/gofrs/uuid/v5"
	log "github.com/sirupsen/logrus"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func parseStorefrontID(id string) string {
	sfID, err := strconv.Atoi(strings.Split(id, "-")[0])
	if err != nil {
		panic(err)
	}
	type StorefrontMapping struct {
		Name         string `json:"name"`
		Code         string `json:"code"`
		StorefrontId int    `json:"storefrontId"`
	}
	var mapping []StorefrontMapping
	file, err := os.ReadFile("data/storefront_ids.json")
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(file, &mapping)
	if err != nil {
		panic(err)
	}
	for _, element := range mapping {
		if element.StorefrontId == sfID {
			return element.Code
		}
	}
	return ""
}

func PrepareWrapper(mirror bool) {
	if _, err := os.Stat("data/wrapper/wrapper"); os.IsNotExist(err) {
		if _, err := os.Stat("data/wrapper-x86_64.zip"); os.IsNotExist(err) {
			DownloadWrapperRelease(mirror)
		}
		err = unzip.New("data/wrapper-x86_64.zip", "data/wrapper").Extract()
		if err != nil {
			panic(err)
		}
		err = os.Chmod("data/wrapper/wrapper", 0777)
		if err != nil {
			panic(err)
		}
	}
}

func WrapperInitial(account string, password string) {
	id := uuid.NewV5(uuid.FromStringOrNil("77777777-7777-7777-7777-77777777"), account)
	err := os.MkdirAll("data/wrapper/rootfs/data/instances/"+id.String(), 0777)
	if err != nil {
		panic(err)
	}

	instance := WrapperInstance{
		Id:          id.String(),
		DecryptPort: GenerateUniquePort(),
		M3U8Port:    GenerateUniquePort(),
		NoRestart:   true,
	}

	args := []string{
		"-H0.0.0.0",
		fmt.Sprintf("-L%s:%s", account, password),
		fmt.Sprintf("-B%s", "/data/instances/"+instance.Id),
		fmt.Sprintf("-D%d", instance.DecryptPort),
		fmt.Sprintf("-M%d", instance.M3U8Port),
		"-F",
	}

	if PROXY != "" {
		args = append(args, fmt.Sprintf("-P%s", PROXY))
	}

	cmd := exec.Command("./wrapper", args...)
	cmd.Dir = "data/wrapper/"

	ptmx, err := pty.Start(cmd)
	if err != nil {
		panic(err)
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
		NoRestart:   false,
	}

	args := []string{
		"-H0.0.0.0",
		fmt.Sprintf("-B%s", "/data/instances/"+id),
		fmt.Sprintf("-D%d", instance.DecryptPort),
		fmt.Sprintf("-M%d", instance.M3U8Port),
	}

	if PROXY != "" {
		args = append(args, fmt.Sprintf("-P%s", PROXY))
	}

	cmd := exec.Command("./wrapper", args...)
	cmd.Dir = "data/wrapper/"

	ptmx, err := pty.Start(cmd)
	if err != nil {
		panic(err)
	}
	defer func() { _ = ptmx.Close() }()

	instance.Cmd = cmd
	go handleOutput(ptmx, &instance)

	_ = cmd.Wait()

	go wrapperDown(&instance)
}

func handleOutput(reader io.Reader, instance *WrapperInstance) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "__") {
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
		panic(err)
	}
	region := parseStorefrontID(string(storefrontID))
	instance.Region = region
	InsertInstance(instance)
	err = SchedulerInstance.AddInstance(instance)
	if err != nil {
		return
	}
	instance.NoRestart = false
	go LoginDoneHandler(instance.Id)
	log.Info(fmt.Sprintf("[wrapper %s]", strings.Split(instance.Id, "-")[0]), " Wrapper ready")
}

func wrapperDown(instance *WrapperInstance) {
	log.Info(fmt.Sprintf("[wrapper %s]", strings.Split(instance.Id, "-")[0]), " Wrapper Down")
	RemoveInstance(instance)
	err := SchedulerInstance.RemoveInstance(instance.Id)
	if err != nil {
		return
	}
	if !instance.NoRestart {
		go WrapperStart(instance.Id)
	} else {
		SaveInstances()
	}
}

func provide2FACode(id string, code string) {
	err := os.WriteFile("data/wrapper/rootfs/data/instances/"+id+"/2fa.txt", []byte(code), 0777)
	if err != nil {
		panic(err)
	}
}

func RemoveWrapperData(id string) {
	err := os.RemoveAll("data/wrapper/rootfs/data/instances/" + id)
	if err != nil {
		panic(err)
	}
}

func DownloadWrapperRelease(mirror bool) {
	resp, err := GetHttpClient().R().Get("https://api.github.com/repos/WorldObservationLog/wrapper/releases/latest")
	if err != nil {
		panic(err)
	}
	buf := new(strings.Builder)
	_, err = io.Copy(buf, resp.Body)
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
	wrapperResp, err := GetHttpClient().R().Get(downloadUrl.(string))
	if err != nil {
		panic(err)
	}
	binary, err := io.ReadAll(wrapperResp.Body)
	err = os.WriteFile("data/wrapper-x86_64.zip", binary, 0777)
	if err != nil {
		panic(err)
	}
}

func DownloadStorefrontIds() {
	resp, err := GetHttpClient().R().Get("https://gist.githubusercontent.com/BrychanOdlum/2208578ba151d1d7c4edeeda15b4e9b1/raw/8f01e4a4cb02cf97a48aba4665286b0e8de14b8e/storefrontmappings.json")
	if err != nil {
		panic(err)
	}
	ids, err := io.ReadAll(resp.Body)
	err = os.WriteFile("data/storefront_ids.json", ids, 0777)
	if err != nil {
		panic(err)
	}
}

func NoSubscriptionHandler(instance *WrapperInstance) {
	if instance.NoRestart {
		go LoginFailedHandler(instance.Id)
	} else {
		RemoveInstance(instance)
		RemoveWrapperData(instance.Id)
		SaveInstances()
	}
}
