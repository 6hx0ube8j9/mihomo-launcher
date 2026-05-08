package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	APP_MUTEX   = "Global\\MihomoLauncher_Unique_Mutex"
	CONFIG_FILE = "mihomo-launcher.ini"
	TASK_NAME   = "MihomoLauncherTask"

	StateStop    = 0 // 红色
	StateError   = 1 // 黄色
	StateTun     = 2 // 绿色
	StateProxy   = 3 // 蓝色
	StateDefault = 4 // 灰色
)

var (
	isReallyExiting bool
	silenceUntil    time.Time
	errorStartTime  time.Time
	onceSync        sync.Once
	hJob, hMutex    windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	configData      = make(map[string]string)
	configMu        sync.RWMutex
	lastState       = -1
	mTun, mProxy    *systray.MenuItem
)

// --- 权限与进程管理 ---

func isAdmin() bool {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil { return false }
	defer token.Close()
	return token.IsElevated()
}

func runAsAdmin() {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := syscall.UTF16PtrFromString(exePath)
	cwd, _ := syscall.UTF16PtrFromString(baseDir)
	_ = windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_HIDE)
}

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(h), 9, uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))),
		)
		hJob = h
	}
}

// --- 配置管理系统 ---

func loadAndCleanConfig() {
	configMu.Lock()
	defer configMu.Unlock()
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	defaults := map[string]string{
		"mode": "rule", "tun_enabled": "false", "system_proxy_enabled": "false",
		"proxy_address": "127.0.0.1:7890", "tun_device_name": "Mihomo",
		"external-controller": "http://127.0.0.1:9090",
	}
	for k, v := range defaults {
		if configData[k] == "" { configData[k] = v }
	}
}

func syncIniToDisk() {
	configMu.RLock()
	defer configMu.RUnlock()
	order := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range order {
		if v, ok := configData[k]; ok {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func setConfig(k, v string) {
	configMu.Lock()
	if configData[k] == v { configMu.Unlock(); return }
	configData[k] = v
	configMu.Unlock()
	syncIniToDisk()
}

func getConfig(k string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configData[k]
}

// --- 状态感知核心 (含 Error 不插队逻辑) ---

func isTunActive() bool {
	target := strings.ToLower(getConfig("tun_device_name"))
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		name := strings.ToLower(i.Name)
		if (strings.Contains(name, target) || strings.Contains(name, "wintun")) && (i.Flags&net.FlagUp) != 0 {
			return true
		}
	}
	return false
}

func monitorLoop() {
	for {
		if isReallyExiting { return }
		if time.Now().Before(silenceUntil) {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		curr := StateStop
		if isProcessRunning("mihomo.exe") {
			resp, err := doAPIRequest("GET", "/configs", nil)
			if err == nil {
				resp.Body.Close()
				onceSync.Do(func() { go syncParamsToKernel() })

				wantTun := getConfig("tun_enabled") == "true"
				wantProxy := getConfig("system_proxy_enabled") == "true"
				tunActive := isTunActive()

				if wantTun {
					if tunActive {
						curr = StateTun
						errorStartTime = time.Time{}
					} else {
						// Error 不插队逻辑：网卡缺失时进入观察期
						if errorStartTime.IsZero() {
							errorStartTime = time.Now()
							curr = lastState // 保持上一次状态，不跳黄
						} else if time.Since(errorStartTime) > 5*time.Second {
							curr = StateError // 5秒后还没网卡，才报黄
						} else {
							curr = lastState // 观察期内，不准黄色插队
						}
					}
				} else if wantProxy {
					curr = StateProxy
					errorStartTime = time.Time{}
				} else {
					curr = StateDefault
					errorStartTime = time.Time{}
				}
			}
		}

		if curr != lastState && curr != -1 {
			updateIcon(curr)
			lastState = curr
		}
		time.Sleep(1 * time.Second)
	}
}

// --- 动作执行 ---

func doAPIRequest(method, path string, payload interface{}) (*http.Response, error) {
	url := getConfig("external-controller") + path
	var body []byte
	if payload != nil { body, _ = json.Marshal(payload) }
	req, _ := http.NewRequest(method, url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	if s := getConfig("secret"); s != "" { req.Header.Set("Authorization", "Bearer "+s) }
	return httpClient.Do(req)
}

func syncParamsToKernel() {
	payload := map[string]interface{}{
		"mode": getConfig("mode"),
		"tun":  map[string]bool{"enable": getConfig("tun_enabled") == "true"},
	}
	_, _ = doAPIRequest("PATCH", "/configs", payload)
}

func updateIcon(state int) {
	icons := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state >= 0 && state < len(icons) {
		b, _ := iconFs.ReadFile("icons/" + icons[state])
		systray.SetIcon(b)
	}
}

// --- 托盘主程序 ---

func onReady() {
	loadAndCleanConfig()
	updateIcon(StateStop)

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()
	mProxy = systray.AddMenuItemCheckbox("系统代理", "", getConfig("system_proxy_enabled") == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getConfig("tun_enabled") == "true")
	systray.AddSeparator()

	modeItems := make(map[string]*systray.MenuItem)
	for _, m := range []string{"rule", "global", "direct"} {
		label := map[string]string{"rule": "规则模式", "global": "全局模式", "direct": "直连模式"}[m]
		modeItems[m] = systray.AddMenuItemCheckbox(label, "", getConfig("mode") == m)
	}
	systray.AddSeparator()

	mAuto := systray.AddMenuItemCheckbox("开机自启动", "", checkAutoStart())
	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("关闭程序", "")

	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(getConfig("external-controller")+"/ui/"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			setConfig("system_proxy_enabled", fmt.Sprint(next))
			setProxyRegistry(next)
			if next { mProxy.Check() } else { mProxy.Uncheck() }
		case <-mTun.ClickedCh:
			next := !mTun.Checked()
			silenceUntil = time.Now().Add(3 * time.Second)
			errorStartTime = time.Time{} // 重置错误计时，给内核启动时间
			setConfig("tun_enabled", fmt.Sprint(next))
			syncParamsToKernel()
			if next { mTun.Check() } else { mTun.Uncheck() }
		case <-modeItems["rule"].ClickedCh:
			for k, v := range modeItems { if k == "rule" { v.Check() } else { v.Uncheck() } }
			setConfig("mode", "rule"); syncParamsToKernel()
		case <-modeItems["global"].ClickedCh:
			for k, v := range modeItems { if k == "global" { v.Check() } else { v.Uncheck() } }
			setConfig("mode", "global"); syncParamsToKernel()
		case <-modeItems["direct"].ClickedCh:
			for k, v := range modeItems { if k == "direct" { v.Check() } else { v.Uncheck() } }
			setConfig("mode", "direct"); syncParamsToKernel()
		case <-mAuto.ClickedCh:
			next := !mAuto.Checked()
			toggleAutoStart(next)
			if next { mAuto.Check() } else { mAuto.Uncheck() }
		case <-mRestart.ClickedCh:
			silenceUntil = time.Now().Add(4 * time.Second)
			errorStartTime = time.Time{}
			_ = exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
		case <-mExit.ClickedCh:
			isReallyExiting = true
			setProxyRegistry(false)
			systray.Quit()
			return
		}
	}
}

// --- 系统工具 ---

func setProxyRegistry(enable bool) {
	key, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.SET_VALUE)
	defer key.Close()
	if enable {
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", getConfig("proxy_address"))
	} else { _ = key.SetDWordValue("ProxyEnable", 0) }
}

func toggleAutoStart(enable bool) {
	if enable {
		_ = exec.Command("schtasks", "/Create", "/TN", TASK_NAME, "/TR", "\""+exePath+"\"", "/SC", "ONLOGON", "/RL", "HIGHEST", "/F").Run()
	} else {
		_ = exec.Command("schtasks", "/Delete", "/TN", TASK_NAME, "/F").Run()
	}
}

func checkAutoStart() bool {
	return exec.Command("schtasks", "/Query", "/TN", TASK_NAME).Run() == nil
}

func isProcessRunning(name string) bool {
	h, _ := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := windows.Process32First(h, &pe); err == nil; err = windows.Process32Next(h, &pe) {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) { return true }
	}
	return false
}

func main() {
	os.Chdir(baseDir)
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	for i := 0; i < 3; i++ {
		h, _ := windows.CreateMutex(nil, false, mName)
		if h != 0 {
			if st, _ := windows.WaitForSingleObject(h, 0); st == uint32(windows.WAIT_OBJECT_0) {
				hMutex = h
				break
			}
			windows.CloseHandle(h)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if hMutex == 0 { return }

	if !isAdmin() {
		windows.CloseHandle(hMutex)
		runAsAdmin()
		os.Exit(0)
	}

	initJobObject()
	go func() {
		for {
			if isReallyExiting { return }
			if !isProcessRunning("mihomo.exe") {
				onceSync = sync.Once{}
				cmd := exec.Command(filepath.Join(baseDir, "mihomo.exe"), "-d", baseDir)
				cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
				if err := cmd.Start(); err == nil {
					hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
					_ = windows.AssignProcessToJobObject(hJob, hp)
					_ = cmd.Wait()
				}
			}
			time.Sleep(2 * time.Second)
		}
	}()
	go monitorLoop()
	systray.Run(onReady, nil)
}
