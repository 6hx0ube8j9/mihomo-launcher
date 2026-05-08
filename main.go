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
	"gopkg.in/yaml.v3"
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	APP_MUTEX   = "Global\\MihomoLauncher_Unique_Mutex"
	CONFIG_FILE = "mihomo-launcher.ini"
	REG_RUN     = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY   = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME    = "MihomoLauncher"

	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

var (
	isReallyExiting      bool
	hJob                 windows.Handle
	hMutex               windows.Handle
	httpClient           = &http.Client{Timeout: 1 * time.Second}
	exePath, _           = os.Executable()
	baseDir              = filepath.Dir(exePath)
	configData           = make(map[string]string)
	configMu             sync.RWMutex
	lastState            = -1
	tunErrorCounter      = 0
	onceSync             sync.Once
	isSystemInitializing = true

	// UI 全局引用
	mTun *systray.MenuItem
)

// --- 权限管理 ---

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
	windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_HIDE)
}

// --- 核心初始化：Job Object (确保内核不残留) ---

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

// --- 配置管理核心 (INI持久化 + YAML嗅探) ---

func sniffAndSolidifyConfig() {
	yamlPath := filepath.Join(baseDir, "config.yaml")
	data, err := os.ReadFile(yamlPath)

	tunName, apiAddr, proxyAddr := "utun", "127.0.0.1:9090", "127.0.0.1:7890"

	if err == nil {
		var cfg struct {
			Tun struct {
				Device string `yaml:"device"`
			} `yaml:"tun"`
			ExternalController string `yaml:"external-controller"`
			MixedPort          int    `yaml:"mixed-port"`
			Port               int    `yaml:"port"`
		}
		if err := yaml.Unmarshal(data, &cfg); err == nil {
			if cfg.Tun.Device != "" { tunName = cfg.Tun.Device }
			if cfg.ExternalController != "" { apiAddr = cfg.ExternalController }
			if cfg.MixedPort != 0 {
				proxyAddr = fmt.Sprintf("127.0.0.1:%d", cfg.MixedPort)
			} else if cfg.Port != 0 {
				proxyAddr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
			}
		}
	}

	configMu.Lock()
	configData["tun_device_name"] = tunName
	configData["api_url"] = "http://" + apiAddr
	configData["proxy_address"] = proxyAddr
	configMu.Unlock()
}

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	configMu.Lock()
	defer configMu.Unlock()
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		if parts := strings.SplitN(strings.TrimSpace(line), "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	// 补全默认字段
	defaults := map[string]string{"tun_enabled": "false", "system_proxy_enabled": "false", "mode": "rule", "auto_start": "false"}
	for k, v := range defaults {
		if _, ok := configData[k]; !ok { configData[k] = v }
	}
}

func getIniConfig(key string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configData[key]
}

func saveIniConfig(key, val string) {
	configMu.Lock()
	configData[key] = val
	var buf bytes.Buffer
	for k, v := range configData {
		if k != "" { buf.WriteString(fmt.Sprintf("%s = %s\n", k, v)) }
	}
	content := buf.Bytes()
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), content, 0644)
}

// --- 进程管理 (合入第一份代码的 /T 强力清理) ---

func killMihomo() {
	// 使用 /T 彻底杀灭 mihomo 及其产生的任何子进程树
	killCmd := exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe")
	killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = killCmd.Run()
	
	configMu.Lock()
	onceSync = sync.Once{} // 内核重启，允许重新同步配置
	configMu.Unlock()
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil { return false }
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil { return false }
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) && pe.ProcessID != uint32(os.Getpid()) {
			return true
		}
		if err := windows.Process32Next(h, &pe); err != nil { break }
	}
	return false
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		if !isProcessRunning("mihomo.exe") {
			killMihomo() 
			time.Sleep(300 * time.Millisecond)
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

// --- 状态监听 (NotifyAddrChange 底层监听) ---

func checkSystemState() int {
	configMu.RLock()
	apiURL := configData["api_url"]
	wantTun := configData["tun_enabled"] == "true"
	wantProxy := configData["system_proxy_enabled"] == "true"
	tunDevice := strings.ToLower(configData["tun_device_name"])
	configMu.RUnlock()

	resp, err := httpClient.Get(apiURL)
	if err != nil {
		tunErrorCounter = 0
		return StateStop
	}
	resp.Body.Close()

	onceSync.Do(func() { go syncConfigToKernel() })

	if wantTun {
		hasTunInterface := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			if strings.Contains(strings.ToLower(i.Name), tunDevice) {
				hasTunInterface = true
				break
			}
		}
		if hasTunInterface {
			tunErrorCounter = 0
			return StateTun
		} else {
			// 缓冲计数逻辑（第二份代码精华）
			tunErrorCounter++
			if tunErrorCounter > 5 { return StateError }
			return StateTun
		}
	}
	if wantProxy { return StateProxy }
	return StateDefault
}

func watchTunState() {
	modiphlpapi := syscall.NewLazyDLL("iphlpapi.dll")
	procNotifyAddrChange := modiphlpapi.NewProc("NotifyAddrChange")
	var handle syscall.Handle
	var overlapped syscall.Overlapped

	for {
		// 阻塞等待系统网卡事件
		procNotifyAddrChange.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&overlapped)))
		time.Sleep(800 * time.Millisecond) 

		hasTun := false
		configMu.RLock()
		tunDevice := strings.ToLower(configData["tun_device_name"])
		configMu.RUnlock()

		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			if strings.Contains(strings.ToLower(i.Name), tunDevice) {
				hasTun = true
				break
			}
		}

		if mTun != nil && !isSystemInitializing {
			if hasTun { mTun.Check() } else { mTun.Uncheck() }
			currentConfig := getIniConfig("tun_enabled") == "true"
			if hasTun != currentConfig {
				saveIniConfig("tun_enabled", fmt.Sprint(hasTun))
			}
		}
	}
}

// --- UI 与 事件 (合入瞬间勾选反馈优点) ---

func onReady() {
	sniffAndSolidifyConfig()
	loadIniConfigAll()
	setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun_enabled") == "true")
	systray.AddSeparator()

	curMode := getIniConfig("mode")
	mRule := systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule" || curMode == "")
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
	systray.AddSeparator()

	mAuto := systray.AddMenuItemCheckbox("开机启动", "", getIniConfig("auto_start") == "true")
	mDir := systray.AddMenuItem("程序目录", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("退出", "")

	go func() {
		time.Sleep(15 * time.Second)
		isSystemInitializing = false
	}()

	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(getIniConfig("api_url")+"/ui"), nil, nil, windows.SW_SHOWNORMAL)

		case <-mTun.ClickedCh:
			next := !mTun.Checked()
			// 移植优点：瞬间切换UI勾选状态，不等待API响应
			if next { mTun.Check() } else { mTun.Uncheck() }
			setTunMode(next)

		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			if next { mProxy.Check() } else { mProxy.Uncheck() }
			setProxyRegistry(next)

		case <-mRule.ClickedCh:
			setMihomoMode("rule")
			mRule.Check(); mGlobal.Uncheck(); mDirect.Uncheck()
		case <-mGlobal.ClickedCh:
			setMihomoMode("global")
			mRule.Uncheck(); mGlobal.Check(); mDirect.Uncheck()
		case <-mDirect.ClickedCh:
			setMihomoMode("direct")
			mRule.Uncheck(); mGlobal.Uncheck(); mDirect.Check()

		case <-mAuto.ClickedCh:
			next := !mAuto.Checked()
			if next { mAuto.Check() } else { mAuto.Uncheck() }
			toggleAutoStart(next)

		case <-mRestart.ClickedCh:
			go killMihomo()
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
			return
		}
	}
}

// --- 系统与API操作 ---

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	configMu.RLock()
	url := configData["api_url"] + "/configs"
	configMu.RUnlock()
	body, _ := json.Marshal(map[string]string{"mode": mode})
	req, _ := http.NewRequest("PATCH", url, bytes.NewBuffer(body))
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func setTunMode(enable bool) {
	isSystemInitializing = true
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	configMu.RLock()
	url := configData["api_url"] + "/configs"
	configMu.RUnlock()
	body, _ := json.Marshal(map[string]interface{}{"tun": map[string]bool{"enable": enable}})
	req, _ := http.NewRequest("PATCH", url, bytes.NewBuffer(body))
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
	go func() { time.Sleep(3 * time.Second); isSystemInitializing = false }()
}

func setProxyRegistry(enable bool) {
	if !isSystemInitializing && !isReallyExiting {
		saveIniConfig("system_proxy_enabled", fmt.Sprint(enable))
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	if err != nil { return }
	defer key.Close()
	if enable {
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", getIniConfig("proxy_address"))
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func syncConfigToKernel() {
	configMu.RLock()
	apiURL := configData["api_url"]
	payload := map[string]interface{}{
		"mode": configData["mode"],
		"tun":  map[string]bool{"enable": configData["tun_enabled"] == "true"},
	}
	configMu.RUnlock()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PATCH", apiURL+"/configs", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func toggleAutoStart(enable bool) {
	saveIniConfig("auto_start", fmt.Sprint(enable))
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE)
	defer key.Close()
	if enable { _ = key.SetStringValue(APP_NAME, exePath) } else { _ = key.DeleteValue(APP_NAME) }
}

func monitorIconState() {
	for {
		if isReallyExiting { return }
		curr := StateStop
		if isProcessRunning("mihomo.exe") { curr = checkSystemState() }
		if curr != lastState {
			files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
			if b, err := iconFs.ReadFile("icons/" + files[curr]); err == nil {
				systray.SetIcon(b)
			}
			lastState = curr
		}
		time.Sleep(1 * time.Second)
	}
}

func onExit() {
	if isReallyExiting {
		setProxyRegistry(false)
		if hJob != 0 { windows.CloseHandle(hJob) }
		if hMutex != 0 { windows.CloseHandle(hMutex) }
	}
}

func main() {
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.WaitForSingleObject(h, 0) == windows.WAIT_TIMEOUT { return }
	hMutex = h

	if !isAdmin() {
		windows.CloseHandle(hMutex)
		runAsAdmin()
		return
	}

	os.Chdir(baseDir)
	initJobObject()
	go monitorKernelDaemon()
	go monitorIconState()
	go watchTunState()
	systray.Run(onReady, onExit)
}
