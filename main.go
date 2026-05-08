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
	syncMu               sync.Mutex // 用于同步配置时的互斥
	lastState            = -1
	tunErrorCounter      = 0
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

func isTunInterfaceMatch(ifaceName string) bool {
	name := strings.ToLower(ifaceName)
	target := strings.ToLower(getIniConfig("tun_device_name"))
	if target != "" && strings.Contains(name, target) {
		return true
	}
	keywords := []string{"mihomo", "meta", "clash", "sing-box", "wintun"}
	for _, kw := range keywords {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
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

// --- 配置管理核心 ---

func ensureDefaultConfig() {
	configMu.Lock()
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
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
	defaults := [][]string{
		{"mode", "rule"},
		{"tun_enabled", "false"},
		{"system_proxy_enabled", "false"},
		{"startup_enabled", "false"},
		{"proxy_address", "127.0.0.1:7890"},
		{"tun_device_name", "Mihomo"},
		{"external-controller", "http://127.0.0.1:9090"},
		{"secret", ""},
	}
	for _, pair := range defaults {
		if val, exists := configData[pair[0]]; !exists || val == "" {
			configData[pair[0]] = pair[1]
		}
	}
	configMu.Unlock()
	saveIniConfig("", "")
}

func sniffAndSolidifyConfig() {
	configPath := filepath.Join(baseDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	inTunSection := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "tun:") {
			inTunSection = true
			continue
		}
		if inTunSection && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inTunSection = false
		}
		if inTunSection && strings.Contains(trimmed, "device:") {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				devName := strings.Trim(parts[1], " \"'")
				if devName != "" {
					saveIniConfig("tun_device_name", devName)
				}
			}
		}
		if strings.HasPrefix(trimmed, "external-controller:") {
			addr := strings.Trim(strings.TrimPrefix(trimmed, "external-controller:"), " \"'")
			if strings.HasPrefix(addr, ":") {
				addr = "127.0.0.1" + addr
			}
			if addr != "" {
				saveIniConfig("external-controller", "http://"+addr)
			}
		}
		if strings.HasPrefix(trimmed, "secret:") {
			saveIniConfig("secret", strings.Trim(strings.TrimPrefix(trimmed, "secret:"), " \"'"))
		}
		if strings.HasPrefix(trimmed, "mixed-port:") || (strings.HasPrefix(trimmed, "port:") && getIniConfig("proxy_address") == "127.0.0.1:7890") {
			port := strings.Trim(strings.SplitN(trimmed, ":", 2)[1], " \"'")
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
	priorityKeys := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range priorityKeys {
		if v, ok := configData[k]; ok {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func doAPIRequest(method, path string, payload interface{}) (*http.Response, error) {
	base := strings.TrimSuffix(getIniConfig("external-controller"), "/")
	url := base + "/" + strings.TrimPrefix(path, "/")
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

// --- 系统操作 ---

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	payload := map[string]string{"mode": mode}
	_, _ = doAPIRequest("PATCH", "/configs", payload)
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
		_ = key.SetStringValue("ProxyServer", getIniConfig("proxy_address"))
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func reloadConfigFile() {
	isSystemInitializing = true
	configPath, _ := filepath.Abs(filepath.Join(baseDir, "config.yaml"))
	
	payload := map[string]interface{}{
		"path": configPath,
		"mode": getIniConfig("mode"), 
	}

	resp, err := doAPIRequest("PUT", "/configs?force=false", payload)
	if err != nil {
		fmt.Printf("重载失败: %v\n", err)
		isSystemInitializing = false
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		fmt.Println("内核已接收重载指令")
		go func() {
			time.Sleep(800 * time.Millisecond)
			isSystemInitializing = false
		}()
	} else {
		isSystemInitializing = false
	}
}

func toggleAutoStart(enable bool) {
	const taskName = "MihomoLauncherTask"
	if key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE); err == nil {
		_ = key.DeleteValue(APP_NAME)
		key.Close()
	}
	saveIniConfig("startup_enabled", fmt.Sprint(enable))

	if enable {
		createCmd := exec.Command("schtasks", "/Create", "/TN", taskName, "/TR", "\""+exePath+"\"", "/SC", "ONLOGON", "/RL", "HIGHEST", "/F")
		createCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := createCmd.Run(); err != nil {
			return
		}
		psScript := fmt.Sprintf(`$s = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); Set-ScheduledTask -TaskName '%s' -Settings $s`, taskName)
		modifyCmd := exec.Command("powershell", "-Command", psScript)
		modifyCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = modifyCmd.Run()
	} else {
		deleteCmd := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F")
		deleteCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = deleteCmd.Run()
	}
}

func checkAutoStartStatus() bool {
	const taskName = "MihomoLauncherTask"
	cmd := exec.Command("schtasks", "/Query", "/TN", taskName)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run() == nil
}

// --- 监控逻辑 ---

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		if !isProcessRunning("mihomo.exe") {
			// 清理残留进程
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
					hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
					if err == nil {
						_ = windows.AssignProcessToJobObject(hJob, hp)
						windows.CloseHandle(hp)
					}
				}
				_ = cmd.Wait()
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func monitorIconState() {
	for {
		if isReallyExiting { return }
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
		return StateStop
	}
	resp.Body.Close()

	if getIniConfig("tun_enabled") == "true" {
		hasTun := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			if isTunInterfaceMatch(i.Name) {
				hasTun = true
				break
			}
		}

		if hasTun {
			tunErrorCounter = 0
			return StateTun
		} else {
			tunErrorCounter++
			if tunErrorCounter > 8 {
				return StateError 
			}
			return StateStop
		}
	}

	if getIniConfig("system_proxy_enabled") == "true" {
		return StateProxy
	}

	return StateDefault
}

func watchTunState() {
	modiphlpapi := syscall.NewLazyDLL("iphlpapi.dll")
	procNotifyAddrChange := modiphlpapi.NewProc("NotifyAddrChange")
	var handle syscall.Handle
	var overlapped syscall.Overlapped

	for {
		procNotifyAddrChange.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&overlapped)))
		time.Sleep(800 * time.Millisecond)

		if isSystemInitializing { continue }

		resp, err := doAPIRequest("GET", "", nil)
		if err != nil { continue }
		resp.Body.Close()

		hasTun := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			if isTunInterfaceMatch(i.Name) {
				hasTun = true
				break
			}
		}

		if mTun != nil {
			currentIniConfig := getIniConfig("tun_enabled") == "true"
			if hasTun != currentIniConfig {
				fmt.Printf("[Monitor] 状态变更: TUN -> %v\n", hasTun)
				if hasTun { mTun.Check() } else { mTun.Uncheck() }
				saveIniConfig("tun_enabled", fmt.Sprint(hasTun))
			}
		}
	}
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil { return false }
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	currPid := uint32(os.Getpid())
	if err := windows.Process32First(h, &pe); err != nil { return false }
	for {
		pname := windows.UTF16ToString(pe.ExeFile[:])
		if strings.EqualFold(pname, name) && pe.ProcessID != currPid { return true }
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := windows.Process32Next(h, &pe); err != nil { break }
	}
	return false
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state < 0 || state >= len(files) { return }
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err == nil { systray.SetIcon(b) }
}

func syncConfigToKernel() {
	syncMu.Lock()
	defer syncMu.Unlock()

	isSystemInitializing = true
	tun := getIniConfig("tun_enabled") == "true"
	mode := getIniConfig("mode")

	payload := map[string]interface{}{
		"mode": mode,
		"tun":  map[string]bool{"enable": tun},
	}

	var err error
	var resp *http.Response
	for i := 0; i < 3; i++ {
		resp, err = doAPIRequest("PATCH", "/configs", payload)
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if err != nil {
		isSystemInitializing = false
		return
	}

	go func() {
		if tun { time.Sleep(2 * time.Second) } else { time.Sleep(500 * time.Millisecond) }
		isSystemInitializing = false
		lastState = -1 
	}()

	if mTun != nil {
		if tun { mTun.Check() } else { mTun.Uncheck() }
	}
}

func onReady() {
	ensureDefaultConfig()
	sniffAndSolidifyConfig()

	setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
	updateIconByState(StateStop)

	go func() {
		success := false
		for i := 0; i < 20; i++ {
			resp, err := doAPIRequest("GET", "", nil)
			if err == nil {
				resp.Body.Close()
				success = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if success {
			syncConfigToKernel()
		} else {
			isSystemInitializing = false
		}
	}()

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()

	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun_enabled") == "true")
	systray.AddSeparator()

	curMode := getIniConfig("mode")
	modeMenus := make(map[string]*systray.MenuItem)
	modeMenus["rule"] = systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule")
	modeMenus["global"] = systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
	modeMenus["direct"] = systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
	systray.AddSeparator()

	isAuto := checkAutoStartStatus()
	mAuto := systray.AddMenuItemCheckbox("开机自启动", "", isAuto)
	mReload := systray.AddMenuItem("重载配置文件", "")
	mDir := systray.AddMenuItem("打开程序目录", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("关闭程序", "")

	for {
		select {
		case <-mWeb.ClickedCh:
			apiAddr := getIniConfig("external-controller")
			secret := getIniConfig("secret")
			host, port := "127.0.0.1", "9090"
			cleanAddr := strings.TrimPrefix(strings.TrimPrefix(apiAddr, "http://"), "https://")
			if parts := strings.Split(cleanAddr, ":"); len(parts) == 2 {
				host, port = parts[0], parts[1]
			}
			finalURL := fmt.Sprintf("%s/ui/?hostname=%s&port=%s&secret=%s#/proxies", apiAddr, host, port, secret)
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(finalURL), nil, nil, windows.SW_SHOWNORMAL)
		case <-mReload.ClickedCh:
			sniffAndSolidifyConfig()
			reloadConfigFile()
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
			saveIniConfig("tun_enabled", fmt.Sprint(next))
			go syncConfigToKernel()
		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			saveIniConfig("system_proxy_enabled", fmt.Sprint(next))
			setProxyRegistry(next)
			if next { mProxy.Check() } else { mProxy.Uncheck() }
		case <-mAuto.ClickedCh:
			next := !mAuto.Checked()
			toggleAutoStart(next)
			if next { mAuto.Check() } else { mAuto.Uncheck() }
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRestart.ClickedCh:
			isSystemInitializing = true
			go func() {
				exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
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
		if hJob != 0 { windows.CloseHandle(hJob) }
		if hMutex != 0 { windows.CloseHandle(hMutex) }
	}
}

func main() {
	if !isAdmin() {
		runAsAdmin()
		os.Exit(0)
	}

	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil { return }
	
	event, _ := windows.WaitForSingleObject(h, 0)
	if event == uint32(windows.WAIT_TIMEOUT) || event == uint32(windows.WAIT_FAILED) {
		windows.CloseHandle(h)
		return
	}
	hMutex = h

	os.Chdir(baseDir)
	initJobObject()

	go monitorKernelDaemon()
	go monitorIconState()
	go watchTunState()

	systray.Run(onReady, onExit)
}
