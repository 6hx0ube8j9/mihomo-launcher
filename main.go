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
	mTun                 *systray.MenuItem
	isSystemInitializing = true
)

// --- 基础工具函数 ---

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

// --- 配置管理核心 (INI 保底 + YAML 矫正) ---

func ensureDefaultConfig() {
	configMu.Lock()
	defer configMu.Unlock()

	// 1. 读取现有文件
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	// 2. 强行检查 7 个保底字段，缺失或为空则填入初始化标准值
	defaults := [][]string{
		{"mode", "rule"},
		{"tun_enabled", "false"},
		{"system_proxy", "false"},
		{"startup_enabled", "false"},
		{"proxy_address", "127.0.0.1:7890"},
		{"external-controller", "http://127.0.0.1:9090"},
		{"secret", ""},
	}

	changed := false
	for _, pair := range defaults {
		if val, exists := configData[pair[0]]; !exists || val == "" {
			configData[pair[0]] = pair[1]
			changed = true
		}
	}

	if changed {
		configMu.Unlock()
		saveIniConfig("", "") // 立即写入文件
		configMu.Lock()
	}
}

func sniffAndSolidifyConfig() {
	configPath := filepath.Join(baseDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 嗅探 API 地址
		if strings.HasPrefix(line, "external-controller:") {
			addr := strings.Trim(strings.TrimPrefix(line, "external-controller:"), " \"'")
			if strings.HasPrefix(addr, ":") {
				addr = "127.0.0.1" + addr
			}
			if addr != "" {
				saveIniConfig("external-controller", "http://"+addr)
			}
		}

		// 嗅探 Secret
		if strings.HasPrefix(line, "secret:") {
			sec := strings.Trim(strings.TrimPrefix(line, "secret:"), " \"'")
			saveIniConfig("secret", sec)
		}

		// 嗅探 代理端口 (mixed-port 优先级最高)
		if strings.HasPrefix(line, "mixed-port:") || (strings.HasPrefix(line, "port:") && getIniConfig("proxy_address") == "127.0.0.1:7890") {
			port := strings.Trim(strings.SplitN(line, ":", 2)[1], " \"'")
			if port != "" {
				saveIniConfig("proxy_address", "127.0.0.1:"+port)
			}
		}
	}
}

func getIniConfig(key string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configData[key]
}

func saveIniConfig(key, val string) {
	configMu.Lock()
	if key != "" {
		configData[key] = val
	}
	var buf bytes.Buffer
	for k, v := range configData {
		if k = strings.TrimSpace(k); k != "" {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	content := buf.Bytes()
	configMu.Unlock()

	configPath := filepath.Join(baseDir, CONFIG_FILE)
	_ = os.WriteFile(configPath, content, 0644)
}

// 统一请求包装，处理 Secret
func doAPIRequest(method, path string, payload interface{}) (*http.Response, error) {
	url := getIniConfig("external-controller") + path
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}

	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if secret := getIniConfig("secret"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	return httpClient.Do(req)
}

// --- 核心逻辑 ---

func syncConfigToKernel() {
	tun := getIniConfig("tun_enabled") == "true"
	mode := getIniConfig("mode")
	proxy := getIniConfig("system_proxy") == "true"

	payload := map[string]interface{}{
		"mode": mode,
		"tun":  map[string]bool{"enable": tun},
	}
	resp, err := doAPIRequest("PATCH", "/configs", payload)
	if err == nil {
		defer resp.Body.Close()
		if (resp.StatusCode == 204 || resp.StatusCode == 200) && proxy {
			setProxyRegistry(true)
		}
	}
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting {
			return
		}
		if !isProcessRunning("mihomo.exe") {
			onceSync = sync.Once{}
			killCmd := exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T")
			killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			_ = killCmd.Run()
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

func monitorIconState() {
	for {
		if isReallyExiting {
			return
		}
		var curr int
		if !isProcessRunning("mihomo.exe") {
			curr = StateStop
		} else {
			curr = checkSystemState()
		}
		if curr != lastState {
			updateIconByState(curr)
			lastState = curr
		}
		time.Sleep(1 * time.Second)
	}
}

func checkSystemState() int {
	resp, err := doAPIRequest("GET", "", nil)
	if err != nil {
		tunErrorCounter = 0
		return StateStop
	}
	defer resp.Body.Close()

	if isSystemInitializing {
		isSystemInitializing = false
	}

	onceSync.Do(func() {
		go syncConfigToKernel()
	})

	wantTun := getIniConfig("tun_enabled") == "true"
	wantProxy := getIniConfig("system_proxy") == "true"

	if wantTun {
		hasTunInterface := false
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				name := strings.ToLower(i.Name)
				if strings.Contains(name, "mihomo") || strings.Contains(name, "meta") ||
					strings.Contains(name, "clash") || strings.Contains(name, "sing-box") {
					hasTunInterface = true
					break
				}
			}
		}
		if hasTunInterface {
			tunErrorCounter = 0
			return StateTun
		} else {
			tunErrorCounter++
			if tunErrorCounter > 5 {
				return StateError
			}
			return StateTun
		}
	}
	if wantProxy {
		return StateProxy
	}
	return StateDefault
}

func watchTunState() {
	var (
		modiphlpapi          = syscall.NewLazyDLL("iphlpapi.dll")
		procNotifyAddrChange = modiphlpapi.NewProc("NotifyAddrChange")
		handle               syscall.Handle
		overlapped           syscall.Overlapped
	)
	for {
		procNotifyAddrChange.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&overlapped)))
		time.Sleep(500 * time.Millisecond)
		hasTun := false
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				name := strings.ToLower(i.Name)
				if strings.Contains(name, "mihomo") || strings.Contains(name, "meta") ||
					strings.Contains(name, "clash") || strings.Contains(name, "sing-box") {
					hasTun = true
					break
				}
			}
		}
		if mTun != nil && !isSystemInitializing {
			if hasTun {
				mTun.Check()
			} else {
				mTun.Uncheck()
			}
			saveIniConfig("tun_enabled", fmt.Sprint(hasTun))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	currPid := uint32(os.Getpid())
	if err := windows.Process32First(h, &pe); err != nil {
		return false
	}
	for {
		pname := windows.UTF16ToString(pe.ExeFile[:])
		if strings.EqualFold(pname, name) && pe.ProcessID != currPid {
			return true
		}
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := windows.Process32Next(h, &pe); err != nil {
			break
		}
	}
	return false
}

func onReady() {
	// 1. 立即初始化保底配置
	ensureDefaultConfig()
	// 2. 尝试从 yaml 矫正
	sniffAndSolidifyConfig()

	setProxyRegistry(getIniConfig("system_proxy") == "true")
	updateIconByState(StateStop)

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy") == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun_enabled") == "true")
	systray.AddSeparator()

	curMode := getIniConfig("mode")
	modeMenus := make(map[string]*systray.MenuItem)
	modeMenus["rule"] = systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule")
	modeMenus["global"] = systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
	modeMenus["direct"] = systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
	systray.AddSeparator()

	mAuto := systray.AddMenuItemCheckbox("开机自动启动", "", getIniConfig("startup_enabled") == "true")
	mDir := systray.AddMenuItem("打开程序目录", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("退出程序", "")

	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(getIniConfig("external-controller")+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-modeMenus["rule"].ClickedCh:
			setMihomoMode("rule")
			modeMenus["rule"].Check()
			modeMenus["global"].Uncheck()
			modeMenus["direct"].Uncheck()
		case <-modeMenus["global"].ClickedCh:
			setMihomoMode("global")
			modeMenus["rule"].Uncheck()
			modeMenus["global"].Check()
			modeMenus["direct"].Uncheck()
		case <-modeMenus["direct"].ClickedCh:
			setMihomoMode("direct")
			modeMenus["rule"].Uncheck()
			modeMenus["global"].Uncheck()
			modeMenus["direct"].Check()
		case <-mTun.ClickedCh:
			next := !mTun.Checked()
			setTunMode(next)
			if next {
				mTun.Check()
			} else {
				mTun.Uncheck()
			}
		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			saveIniConfig("system_proxy", fmt.Sprint(next))
			setProxyRegistry(next)
			if next {
				mProxy.Check()
			} else {
				mProxy.Uncheck()
			}
		case <-mAuto.ClickedCh:
			next := !mAuto.Checked()
			toggleAutoStart(next)
			if next {
				mAuto.Check()
			} else {
				mAuto.Uncheck()
			}
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRestart.ClickedCh:
			go func() {
				killCmd := exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe")
				killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
				_ = killCmd.Run()
				onceSync = sync.Once{}
			}()
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
			return
		}
	}
}

func onExit() {
	if isReallyExiting {
		setProxyRegistry(false)
		time.Sleep(50 * time.Millisecond)
		if hJob != 0 {
			windows.CloseHandle(hJob)
		}
		if hMutex != 0 {
			windows.CloseHandle(hMutex)
		}
	}
}

// --- 系统操作 ---

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	payload := map[string]string{"mode": mode}
	_, _ = doAPIRequest("PATCH", "/configs", payload)
}

func setTunMode(enable bool) {
	isSystemInitializing = true
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	payload := map[string]interface{}{"tun": map[string]bool{"enable": enable}}
	_, _ = doAPIRequest("PATCH", "/configs", payload)
	go func() {
		time.Sleep(3 * time.Second)
		isSystemInitializing = false
	}()
}

func setProxyRegistry(enable bool) {
	if !isReallyExiting {
		saveIniConfig("system_proxy", fmt.Sprint(enable))
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer key.Close()
	if enable {
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", getIniConfig("proxy_address"))
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func toggleAutoStart(enable bool) {
	saveIniConfig("startup_enabled", fmt.Sprint(enable))
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE)
	defer key.Close()
	if enable {
		_ = key.SetStringValue(APP_NAME, exePath)
	} else {
		_ = key.DeleteValue(APP_NAME)
	}
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state < 0 || state >= len(files) {
		return
	}
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err == nil {
		systray.SetIcon(b)
	}
}

func main() {
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil {
		return
	}
	event, _ := windows.WaitForSingleObject(h, 0)
	if event == uint32(windows.WAIT_TIMEOUT) || event == uint32(windows.WAIT_FAILED) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return
	}
	hMutex = h

	if !isAdmin() {
		if hMutex != 0 {
			windows.CloseHandle(hMutex)
			hMutex = 0
		}
		runAsAdmin()
		os.Exit(0)
	}

	os.Chdir(baseDir)
	initJobObject()

	go monitorKernelDaemon()
	go monitorIconState()
	go watchTunState()

	systray.Run(onReady, onExit)
}
