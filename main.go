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
	APP_NAME    = "MihomoLauncher"
	TASK_NAME   = "MihomoLauncherTask"

	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

var (
	// 控制状态
	isReallyExiting bool
	silenceUntil    time.Time // 静默期截止时间：防止操作期间监控干扰
	onceSync        sync.Once
	
	// 句柄
	hJob   windows.Handle
	hMutex windows.Handle

	// 全局变量
	httpClient = &http.Client{Timeout: 1 * time.Second}
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	configData = make(map[string]string)
	configMu   sync.RWMutex
	lastState  = -1
	
	// 托盘菜单引用
	mTun *systray.MenuItem
)

// --- 权限与进程管理 ---

func isAdmin() bool {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

func runAsAdmin() {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := syscall.UTF16PtrFromString(exePath)
	cwd, _ := syscall.UTF16PtrFromString(baseDir)
	windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_HIDE)
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

// --- 配置管理系统 (核心健壮性) ---

func loadAndCleanConfig() {
	configMu.Lock()
	defer configMu.Unlock()

	// 1. 读取并清理非法字符
	path := filepath.Join(baseDir, CONFIG_FILE)
	b, _ := os.ReadFile(path)
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	// 2. 核心保底值
	defaults := map[string]string{
		"mode":                 "rule",
		"tun_enabled":          "false",
		"system_proxy_enabled": "false",
		"proxy_address":        "127.0.0.1:7890",
		"tun_device_name":      "Mihomo",
		"external-controller":  "http://127.0.0.1:9090",
	}
	for k, v := range defaults {
		if configData[k] == "" {
			configData[k] = v
		}
	}
}

func syncIniToDisk() {
	configMu.RLock()
	// 定义严格的写入顺序
	order := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range order {
		if v, ok := configData[k]; ok {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	configMu.RUnlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func setConfig(key, val string) {
	configMu.Lock()
	configData[key] = val
	configMu.Unlock()
	syncIniToDisk()
}

func getConfig(key string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configData[key]
}

// --- 网络状态感知 ---

func isTunActive() bool {
	target := strings.ToLower(getConfig("tun_device_name"))
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, i := range ifaces {
		name := strings.ToLower(i.Name)
		// 动态匹配：包含配置名 或 包含常见内核关键词
		if (target != "" && strings.Contains(name, target)) || 
		   strings.Contains(name, "wintun") || 
		   strings.Contains(name, "mihomo") {
			return true
		}
	}
	return false
}

// --- 后台守候协程 ---

func daemonLoop() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }

		if !isProcessRunning("mihomo.exe") {
			onceSync = sync.Once{} // 重置同步标记
			cmd := exec.Command(target, "-d", baseDir)
			cmd.SysProcAttr = &windows.SysProcAttr{
				CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
			}
			if err := cmd.Start(); err == nil {
				if hJob != 0 {
					hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
					_ = windows.AssignProcessToJobObject(hJob, hp)
					windows.CloseHandle(hp)
				}
				_ = cmd.Wait()
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func monitorLoop() {
	for {
		if isReallyExiting { return }

		// 如果在静默期，跳过监控，防止 UI 闪烁
		if time.Now().Before(silenceUntil) {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		var curr int
		if !isProcessRunning("mihomo.exe") {
			curr = StateStop
		} else {
			// API 检测
			resp, err := doAPIRequest("GET", "/configs", nil)
			if err != nil {
				curr = StateStop
			} else {
				resp.Body.Close()
				// 内核刚启动，同步一次配置
				onceSync.Do(func() { go syncParamsToKernel() })
				
				// 判定图标状态
				if getConfig("tun_enabled") == "true" {
					if isTunActive() { curr = StateTun } else { curr = StateError }
				} else if getConfig("system_proxy_enabled") == "true" {
					curr = StateProxy
				} else {
					curr = StateDefault
				}
			}
		}

		if curr != lastState {
			updateIcon(curr)
			lastState = curr
		}
		time.Sleep(1 * time.Second)
	}
}

func watchNetworkLoop() {
	// 监听网卡变动同步 INI
	for {
		time.Sleep(3 * time.Second)
		if isReallyExiting || time.Now().Before(silenceUntil) { continue }
		if !isProcessRunning("mihomo.exe") { continue }

		active := isTunActive()
		recorded := getConfig("tun_enabled") == "true"
		
		// 只有当两者不一致，且内核存活时，同步状态（处理外部 Web 端的改动）
		if active != recorded {
			setConfig("tun_enabled", fmt.Sprint(active))
			if mTun != nil {
				if active { mTun.Check() } else { mTun.Uncheck() }
			}
		}
	}
}

// --- 动作执行 ---

func doAPIRequest(method, path string, payload interface{}) (*http.Response, error) {
	url := getConfig("external-controller") + path
	var body []byte
	if payload != nil { body, _ = json.Marshal(payload) }

	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil { return nil, err }

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

	// 菜单构建
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()
	
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getConfig("system_proxy_enabled") == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getConfig("tun_enabled") == "true")
	systray.AddSeparator()

	modes := []string{"rule", "global", "direct"}
	modeItems := make(map[string]*systray.MenuItem)
	for _, m := range modes {
		label := map[string]string{"rule":"规则模式","global":"全局模式","direct":"直连模式"}[m]
		modeItems[m] = systray.AddMenuItemCheckbox(label, "", getConfig("mode") == m)
	}
	systray.AddSeparator()

	mAuto := systray.AddMenuItemCheckbox("开机自启动", "", checkAutoStart())
	mDir := systray.AddMenuItem("打开程序目录", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("关闭程序", "")

	// 事件循环
	for {
		select {
		case <-mWeb.ClickedCh:
			u := getConfig("external-controller")
			host := "127.0.0.1"
			port := "9090"
			clean := strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
			if p := strings.Split(clean, ":"); len(p) == 2 { host, port = p[0], p[1] }
			final := fmt.Sprintf("%s/ui/?hostname=%s&port=%s&secret=%s#/proxies", u, host, port, getConfig("secret"))
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(final), nil, nil, windows.SW_SHOWNORMAL)

		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			setConfig("system_proxy_enabled", fmt.Sprint(next))
			setProxyRegistry(next)
			if next { mProxy.Check() } else { mProxy.Uncheck() }

		case <-mTun.ClickedCh:
			next := !mTun.Checked()
			silenceUntil = time.Now().Add(3 * time.Second) // 开启 3 秒静默期
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
			exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()

		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)

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
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func toggleAutoStart(enable bool) {
	if enable {
		exec.Command("schtasks", "/Create", "/TN", TASK_NAME, "/TR", "\""+exePath+"\"", "/SC", "ONLOGON", "/RL", "HIGHEST", "/F").Run()
	} else {
		exec.Command("schtasks", "/Delete", "/TN", TASK_NAME, "/F").Run()
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
	windows.Process32First(h, &pe)
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) && pe.ProcessID != uint32(os.Getpid()) {
			return true
		}
		if windows.Process32Next(h, &pe) != nil { break }
	}
	return false
}

func main() {
	os.Chdir(baseDir)
	
	// 互斥锁逻辑
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	for i := 0; i < 3; i++ {
		h, _ := windows.CreateMutex(nil, false, mName)
		if h != 0 {
			if event, _ := windows.WaitForSingleObject(h, 0); event == uint32(windows.WAIT_OBJECT_0) {
				hMutex = h
				break
			}
			windows.CloseHandle(h)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if hMutex == 0 { return }

	// 权限逻辑
	if !isAdmin() {
		windows.CloseHandle(hMutex)
		runAsAdmin()
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	}

	initJobObject()
	go daemonLoop()
	go monitorLoop()
	go watchNetworkLoop()

	systray.Run(onReady, nil)
}
