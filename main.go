package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	APP_MUTEX    = "Global\\MihomoLauncher_Unique_Mutex"
	CONFIG_FILE  = "mihomo-launcher.ini"
	REG_RUN      = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY    = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME     = "MihomoLauncher"
	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

var (
	hJob                 windows.Handle
	hMutex               windows.Handle
	httpClient           = &http.Client{Timeout: 1 * time.Second}
	exePath, _           = os.Executable()
	baseDir              = filepath.Dir(exePath)
	isSystemInitializing = true
	isSyncing            int32
	isReallyExiting      bool
	onceSync             sync.Once
	exitOnce             sync.Once
	configMu             sync.RWMutex
	configData           = make(map[string]string)
	lastState            = -1
	tunErrorCounter      = 0
	mTun                 *systray.MenuItem
	isKernelActive       int32
)

func main() {
	var err error
	exePath, err = os.Executable()
	if err != nil {
		return
	}
	baseDir = filepath.Dir(exePath)
	_ = os.Chdir(baseDir)

	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return
	}
	hMutex = h

	isAutostart := false
	for _, arg := range os.Args {
		if arg == "---autostart" {
			isAutostart = true
			break
		}
	}

	if !isAdmin() && !isAutostart {
		runAsAdmin()
		return
	}

	ensureDefaultConfig()
	setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		systray.Quit()
	}()

	KillProcessByName("mihomo.exe")
	time.Sleep(200 * time.Millisecond)

	initJobObject()
	sniffAndSolidifyConfig()

	go func() {
		time.Sleep(1 * time.Second)
		go monitorKernelDaemon()
		go monitorIconState()
		go watchTunState()
	}()

	systray.Run(onReady, onExit)
	onExit()
}

func onReady() {
	saveIniConfig("startup_enabled", fmt.Sprint(checkAutoStartStatus()))
	ensureDefaultConfig()
	sniffAndSolidifyConfig()
	setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
	updateIconByState(StateStop)

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()

	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
	mTun = systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", getIniConfig("tun_enabled") == "true")
	systray.AddSeparator()

	mModeRoot := systray.AddMenuItem("模式切换", "")
	curMode := getIniConfig("mode")
	modeMenus := make(map[string]*systray.MenuItem)
	modeMenus["rule"] = mModeRoot.AddSubMenuItemCheckbox("规则模式", "", curMode == "rule")
	modeMenus["global"] = mModeRoot.AddSubMenuItemCheckbox("全局模式", "", curMode == "global")
	modeMenus["direct"] = mModeRoot.AddSubMenuItemCheckbox("直连模式", "", curMode == "direct")
	systray.AddSeparator()

	mDir := systray.AddMenuItem("打开目录", "")
	mMoreRoot := systray.AddMenuItem("更多", "")
	mAuto := mMoreRoot.AddSubMenuItemCheckbox("开机自启动", "", checkAutoStartStatus())
	mRestart := mMoreRoot.AddSubMenuItem("重启内核", "")
	mReload := mMoreRoot.AddSubMenuItem("重载配置文件", "")
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
			if next { mTun.Check() } else { mTun.Uncheck() }
			go setTunMode(next)
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
			onceSync = sync.Once{}
			KillProcessByName("mihomo.exe")
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
			return
		}
	}
}

func onExit() {
	exitOnce.Do(func() {
		isReallyExiting = true
		setProxyRegistry(false)
		time.Sleep(100 * time.Millisecond)
		if hJob != 0 {
			windows.CloseHandle(hJob)
		}
		if hMutex != 0 {
			windows.CloseHandle(hMutex)
		}
	})
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	absBaseDir, _ := filepath.Abs(baseDir)
	for {
		if isReallyExiting {
			return
		}
		if !isProcessRunning("mihomo.exe") {
			onceSync = sync.Once{}
			isSystemInitializing = true
			atomic.StoreInt32(&isKernelActive, 0)
			KillProcessByName("mihomo.exe")
			time.Sleep(500 * time.Millisecond)
			cmd := exec.Command(target, "-d", ".")
			cmd.Dir = absBaseDir
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			if err := cmd.Start(); err == nil {
				atomic.StoreInt32(&isKernelActive, 1)
				if hJob != 0 {
					hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
					if err == nil {
						_ = windows.AssignProcessToJobObject(hJob, hp)
						_ = windows.CloseHandle(hp)
					}
				}
				go func(c *exec.Cmd) {
					_ = c.Wait()
					atomic.StoreInt32(&isKernelActive, 0)
				}(cmd)
				time.Sleep(1500 * time.Millisecond)
				isSystemInitializing = false
			} else {
				isSystemInitializing = false
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

func watchTunState() {
	modiphlpapi := syscall.NewLazyDLL("iphlpapi.dll")
	procNotifyAddrChange := modiphlpapi.NewProc("NotifyAddrChange")
	var lastHasTun bool
	for {
		if isReallyExiting { return }
		var handle windows.Handle
		procNotifyAddrChange.Call(uintptr(unsafe.Pointer(&handle)), 0)
		time.Sleep(2 * time.Second)
		if isSystemInitializing || atomic.LoadInt32(&isSyncing) == 1 {
			continue
		}
		currentHasTun := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			if isTunInterfaceMatch(i.Name) {
				currentHasTun = true
				break
			}
		}
		if currentHasTun == lastHasTun {
			continue
		}
		if atomic.LoadInt32(&isKernelActive) == 0 {
			continue
		}
		resp, err := doAPIRequest("GET", "/configs", nil)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		configEnabled := getIniConfig("tun_enabled") == "true"
		if currentHasTun != configEnabled {
			if mTun != nil {
				if currentHasTun { mTun.Check() } else { mTun.Uncheck() }
			}
			saveIniConfig("tun_enabled", fmt.Sprint(currentHasTun))
		}
		lastHasTun = currentHasTun
	}
}

func syncConfigToKernel() {
	if !atomic.CompareAndSwapInt32(&isSyncing, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&isSyncing, 0)
	isSystemInitializing = true
	timer := time.AfterFunc(10*time.Second, func() { isSystemInitializing = false })
	defer timer.Stop()
	tunEnabled := getIniConfig("tun_enabled") == "true"
	payload := map[string]interface{}{
		"mode": getIniConfig("mode"),
		"tun":  map[string]bool{"enable": tunEnabled},
	}
	success := false
	for i := 0; i < 3; i++ {
		resp, err := doAPIRequest("PATCH", "/configs", payload)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			success = true
			break
		}
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	if success && mTun != nil {
		if tunEnabled { mTun.Check() } else { mTun.Uncheck() }
	}
	time.Sleep(1 * time.Second)
	isSystemInitializing = false
}

func doAPIRequest(method, path string, payload interface{}) (*http.Response, error) {
	apiAddr := strings.TrimSuffix(getIniConfig("external-controller"), "/")
	url := apiAddr + "/" + strings.TrimPrefix(path, "/")
	var bodyReader io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		bodyReader = bytes.NewBuffer(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := getIniConfig("secret"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API Error: %d", resp.StatusCode)
	}
	return resp, nil
}

func ensureDefaultConfig() {
	configMu.Lock()
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") { continue }
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	defaults := [][]string{
		{"mode", "rule"}, {"tun_enabled", "false"}, {"system_proxy_enabled", "false"},
		{"startup_enabled", "false"}, {"proxy_address", "127.0.0.1:7890"},
		{"tun_device_name", "Mihomo"}, {"external-controller", "http://127.0.0.1:9090"}, {"secret", ""},
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
	data, err := os.ReadFile(filepath.Join(baseDir, "config.yaml"))
	if err != nil { return }
	lines := strings.Split(string(data), "\n")
	inTunSection := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") { continue }
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
				if devName := strings.Trim(parts[1], " \"'"); devName != "" {
					saveIniConfig("tun_device_name", devName)
				}
			}
		}
		if strings.HasPrefix(trimmed, "external-controller:") {
			addr := strings.Trim(strings.TrimPrefix(trimmed, "external-controller:"), " \"'")
			if strings.HasPrefix(addr, ":") { addr = "127.0.0.1" + addr }
			if addr != "" { saveIniConfig("external-controller", "http://"+addr) }
		}
		if strings.HasPrefix(trimmed, "secret:") {
			saveIniConfig("secret", strings.Trim(strings.TrimPrefix(trimmed, "secret:"), " \"'"))
		}
		if strings.HasPrefix(trimmed, "mixed-port:") || (strings.HasPrefix(trimmed, "port:") && getIniConfig("proxy_address") == "127.0.0.1:7890") {
			port := strings.Trim(strings.SplitN(trimmed, ":", 2)[1], " \"'")
			if port != "" { saveIniConfig("proxy_address", "127.0.0.1:"+port) }
		}
	}
}

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	_, _ = doAPIRequest("PATCH", "/configs", map[string]string{"mode": mode})
}

func setTunMode(enable bool) {
	isSystemInitializing = true
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	_, _ = doAPIRequest("PATCH", "/configs", map[string]interface{}{"tun": map[string]bool{"enable": enable}})
	time.Sleep(3 * time.Second)
	isSystemInitializing = false
}

func setProxyRegistry(enable bool) {
	if !isReallyExiting {
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

func toggleAutoStart(enable bool) {
	const taskName = "MihomoLauncherTask"
	if key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE); err == nil {
		_ = key.DeleteValue(APP_NAME)
		key.Close()
	}
	success := false
	if enable {
		cleanExe := strings.ReplaceAll(exePath, "'", "''")
		cleanDir := strings.ReplaceAll(baseDir, "'", "''")
		psScript := fmt.Sprintf(
			"$trigger = New-ScheduledTaskTrigger -AtLogOn; $trigger.Delay = 'PT8S'; "+
				"$action = New-ScheduledTaskAction -Execute '%s' -Argument '---autostart' -WorkingDirectory '%s'; "+
				"$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); "+
				"Register-ScheduledTask -TaskName '%s' -Trigger $trigger -Action $action -Settings $settings -User $env:USERNAME -RunLevel Highest -Force",
			cleanExe, cleanDir, taskName,
		)
		cmd := exec.Command("powershell", "-Command", psScript)
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil { success = true }
	} else {
		cmd := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F")
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil || !checkAutoStartStatus() { success = true }
	}
	if success { saveIniConfig("startup_enabled", fmt.Sprint(enable)) }
}

func reloadConfigFile() {
	isSystemInitializing = true
	resp, err := doAPIRequest("PUT", "/configs?force=false", map[string]string{"path": filepath.Join(baseDir, "config.yaml")})
	if err != nil {
		isSystemInitializing = false
		return
	}
	resp.Body.Close()
	go syncConfigToKernel()
}

func checkSystemState() int {
	resp, err := doAPIRequest("GET", "", nil)
	if err != nil {
		tunErrorCounter = 0
		return StateStop
	}
	resp.Body.Close()
	if isSystemInitializing { isSystemInitializing = false }
	onceSync.Do(func() { go syncConfigToKernel() })
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
		}
		tunErrorCounter++
		if tunErrorCounter > 8 { return StateError }
		return StateStop
	}
	if getIniConfig("system_proxy_enabled") == "true" { return StateProxy }
	return StateDefault
}

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
	_ = windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_SHOWNORMAL)
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil { return false }
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil { return false }
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			if pe.ProcessID != uint32(os.Getpid()) { return true }
		}
		if err := windows.Process32Next(h, &pe); err != nil { break }
	}
	return false
}

func KillProcessByName(name string) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil { return }
	defer windows.CloseHandle(snapshot)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snapshot, &pe); err != nil { return }
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			if pe.ProcessID != uint32(os.Getpid()) {
				h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pe.ProcessID)
				if err == nil {
					_ = windows.TerminateProcess(h, 9)
					windows.CloseHandle(h)
				}
			}
		}
		if err := windows.Process32Next(snapshot, &pe); err != nil { break }
	}
}

func checkAutoStartStatus() bool {
	cmd := exec.Command("schtasks", "/Query", "/TN", "MihomoLauncherTask")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run() == nil
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state >= 0 && state < len(files) {
		if b, err := iconFs.ReadFile("icons/" + files[state]); err == nil {
			systray.SetIcon(b)
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
	if key != "" { configData[key] = val }
	keys := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range keys {
		if v, ok := configData[k]; ok { buf.WriteString(fmt.Sprintf("%s = %s\n", k, v)) }
	}
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func isTunInterfaceMatch(ifaceName string) bool {
	name := strings.ToLower(ifaceName)
	target := strings.ToLower(getIniConfig("tun_device_name"))
	if target != "" && strings.Contains(name, target) { return true }
	for _, kw := range []string{"mihomo", "meta", "clash", "sing-box", "wintun"} {
		if strings.Contains(name, kw) { return true }
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
