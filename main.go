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
	APP_MUTEX          = "Global\\MihomoLauncher_Unique_Mutex"
	API_URL            = "http://127.0.0.1:9090"
	CONFIG_FILE        = "mihomo-launcher.ini"
	REG_RUN            = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY          = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME           = "MihomoLauncher"
	DEFAULT_PROXY_ADDR = "127.0.0.1:7890"

	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

var (
	isReallyExiting bool
	hJob            windows.Handle
	hMutex          windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	configData      = make(map[string]string)
	configMu        sync.RWMutex
	lastState       = -1
	tunErrorCounter = 0
	onceSync        sync.Once

	// --- 提升菜单变量作用域，方便状态同步 ---
	mProxy *systray.MenuItem
	mTun   *systray.MenuItem
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

// --- 配置管理 ---

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	configMu.Lock()
	defer configMu.Unlock()

	configData = make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	defaults := map[string]string{
		"tun_enabled":          "false",
		"system_proxy_enabled": "false",
		"mode":                 "rule",
		"auto_start":           "false",
	}

	needsSave := false
	for k, v := range defaults {
		if _, exists := configData[k]; !exists {
			configData[k] = v
			needsSave = true
		}
	}

	if needsSave {
		saveConfigInternal()
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
	configMu.Unlock()
	saveConfigInternal()
}

func saveConfigInternal() {
	configMu.RLock()
	defer configMu.RUnlock()
	var buf bytes.Buffer
	for k, v := range configData {
		if k = strings.TrimSpace(k); k != "" {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

// --- 核心逻辑 ---

func syncConfigToKernel() {
	configMu.RLock()
	tun := configData["tun_enabled"] == "true"
	mode := configData["mode"]
	if mode == "" {
		mode = "rule"
	}
	proxy := configData["system_proxy_enabled"] == "true"
	configMu.RUnlock()

	payload := map[string]interface{}{
		"mode": mode,
		"tun":  map[string]bool{"enable": tun},
	}
	jsonPayload, _ := json.Marshal(payload)

	req, err := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
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
	resp, err := httpClient.Get(API_URL)
	if err != nil {
		tunErrorCounter = 0
		return StateStop
	}
	resp.Body.Close()

	onceSync.Do(func() {
		go syncConfigToKernel()
	})

	configMu.RLock()
	wantTun := configData["tun_enabled"] == "true"
	wantProxy := configData["system_proxy_enabled"] == "true"
	configMu.RUnlock()

	if wantTun {
		hasTunInterface := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			name := strings.ToLower(i.Name)
			// 修正：包含任意内核虚拟网卡关键字
			if strings.Contains(name, "mihomo") || strings.Contains(name, "meta") || strings.Contains(name, "clash") || strings.Contains(name, "utun") {
				hasTunInterface = true
				break
			}
		}

		if hasTunInterface {
			tunErrorCounter = 0
			return StateTun
		} else {
			tunErrorCounter++
			if tunErrorCounter > 5 {
				return StateError // 5秒还没出网卡，显示报错图标
			}
			return StateTun // 暂时维持 TUN 图标，等待网卡启动
		}
	}

	if wantProxy {
		return StateProxy
	}

	return StateDefault
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
	loadIniConfigAll()
	setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
	updateIconByState(StateStop)

	// --- 菜单初始化 ---
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	mProxy = systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun_enabled") == "true")
	systray.AddSeparator()

	curMode := getIniConfig("mode")
	modeMenus := make(map[string]*systray.MenuItem)
	modeMenus["rule"] = systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule" || curMode == "")
	modeMenus["global"] = systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
	modeMenus["direct"] = systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
	systray.AddSeparator()

	mAuto := systray.AddMenuItemCheckbox("开机自动启动", "", getIniConfig("auto_start") == "true")
	mDir := systray.AddMenuItem("打开程序目录", "")
	mReloadYAML := systray.AddMenuItem("重载配置文件", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	systray.AddSeparator()

	mExit := systray.AddMenuItem("退出程序", "")

	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)

		case <-modeMenus["rule"].ClickedCh:
			setMihomoMode("rule")
			modeMenus["rule"].Check(); modeMenus["global"].Uncheck(); modeMenus["direct"].Uncheck()
		case <-modeMenus["global"].ClickedCh:
			setMihomoMode("global")
			modeMenus["rule"].Uncheck(); modeMenus["global"].Check(); modeMenus["direct"].Uncheck()
		case <-modeMenus["direct"].ClickedCh:
			setMihomoMode("direct")
			modeMenus["rule"].Uncheck(); modeMenus["global"].Uncheck(); modeMenus["direct"].Check()

		case <-mTun.ClickedCh:
			next := !mTun.Checked()
			setTunMode(next)
			if next {
				mTun.Check()
				// 开启 TUN 时，为了防止图标逻辑冲突，可以考虑自动取消代理勾选（可选）
			} else {
				mTun.Uncheck()
			}

		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			saveIniConfig("system_proxy_enabled", fmt.Sprint(next))
			setProxyRegistry(next)
			if next {
				mProxy.Check()
			} else {
				mProxy.Uncheck()
			}

		case <-mReloadYAML.ClickedCh:
			go reloadConfigFile()
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
				configMu.Lock()
				onceSync = sync.Once{}
				configMu.Unlock()
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

// --- 系统操作实现 ---

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	payload := map[string]string{"mode": mode}
	jsonBody, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBody))
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

func setTunMode(enable bool) {
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	payload := map[string]interface{}{"tun": map[string]bool{"enable": enable}}
	jsonBody, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBody))
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

func setProxyRegistry(enable bool) {
	if !isReallyExiting {
		saveIniConfig("system_proxy_enabled", fmt.Sprint(enable))
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer key.Close()

	if enable {
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", DEFAULT_PROXY_ADDR)
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func toggleAutoStart(enable bool) {
	saveIniConfig("auto_start", fmt.Sprint(enable))
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE)
	defer key.Close()
	if enable {
		_ = key.SetStringValue(APP_NAME, exePath)
	} else {
		_ = key.DeleteValue(APP_NAME)
	}
}

func reloadConfigFile() {
	configPath := filepath.Join(baseDir, "config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return
	}
	body := map[string]string{"path": configPath}
	jsonPayload, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
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

	systray.Run(onReady, onExit)
}
