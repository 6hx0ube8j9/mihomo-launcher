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
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/energye/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	APP_NAME    = "MihomoLauncher"
	CONFIG_FILE = "launcher_config.ini"
	REG_RUN     = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY   = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
)

const (
	StateStop = iota
	StateError
	StateTun
	StateProxy
	StateDefault
)

var (
	//go:embed icons/*
	iconFs embed.FS

	baseDir, _ = filepath.Abs(filepath.Dir(os.Args[0]))
	exePath, _ = os.Executable()

	configData = make(map[string]string)
	configMu   sync.RWMutex

	isSystemInitializing int32
	isSyncing            int32
	isKernelActive       int32
	isReallyExiting      int32
	hasFirstSynced       int32

	lastState        = -1
	globalLastHasTun bool

	mTun *systray.MenuItem
	hJob windows.Handle

	httpClient = &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   false,
			Proxy:               nil,
		},
	}
)

func main() {
	if !isAdmin() {
		runAsAdmin()
		return
	}

	initJobObject()
	sniffAndSolidifyConfig()
	ensureDefaultConfig()

	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetTitle(APP_NAME)
	systray.SetTooltip(APP_NAME)

	mDashboard := systray.AddMenuItem("控制面板", "打开 Web UI")
	systray.AddSeparator()

	mMode := systray.AddMenuItem("代理模式", "")
	mRule := mMode.AddSubMenuItemCheckbox("规则模式", "", getIniConfig("mode") == "rule")
	mGlobal := mMode.AddSubMenuItemCheckbox("全局模式", "", getIniConfig("mode") == "global")
	mDirect := mMode.AddSubMenuItemCheckbox("直连模式", "", getIniConfig("mode") == "direct")

	mTun = systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", getIniConfig("tun_enabled") == "true")
	mSystemProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
	systray.AddSeparator()

	mReload := systray.AddMenuItem("重载配置", "Reload config.yaml")
	mStartup := systray.AddMenuItemCheckbox("开机启动", "", checkAutoStartStatus())
	mQuit := systray.AddMenuItem("退出程序", "Exit")

	go monitorKernelDaemon()
	go monitorIconState()
	go watchTunState()

	for {
		select {
		case <-mDashboard.ClickedCh:
			launchWebUI()
		case <-mRule.ClickedCh:
			mRule.Check()
			mGlobal.Uncheck()
			mDirect.Uncheck()
			setMihomoMode("rule")
		case <-mGlobal.ClickedCh:
			mRule.Uncheck()
			mGlobal.Check()
			mDirect.Uncheck()
			setMihomoMode("global")
		case <-mDirect.ClickedCh:
			mRule.Uncheck()
			mGlobal.Uncheck()
			mDirect.Check()
			setMihomoMode("direct")
		case <-mTun.ClickedCh:
			next := !mTun.Checked()
			if next {
				mTun.Check()
			} else {
				mTun.Uncheck()
			}
			go setTunMode(next)
		case <-mSystemProxy.ClickedCh:
			next := !mSystemProxy.Checked()
			if next {
				mSystemProxy.Check()
			} else {
				mSystemProxy.Uncheck()
			}
			setProxyRegistry(next)
		case <-mReload.ClickedCh:
			reloadConfigFile()
		case <-mStartup.ClickedCh:
			next := !mStartup.Checked()
			if next {
				mStartup.Check()
			} else {
				mStartup.Uncheck()
			}
			toggleAutoStart(next)
		case <-mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func onExit() {
	atomic.StoreInt32(&isReallyExiting, 1)
	setProxyRegistry(false)

	if hJob != 0 {
		_ = windows.TerminateJobObject(hJob, 0)
		_ = windows.CloseHandle(hJob)
	}

	KillProcessByName("mihomo.exe")

	if apiAddr := getIniConfig("external-controller"); apiAddr != "" {
		url := strings.TrimSuffix(apiAddr, "/") + "/configs"
		req, _ := http.NewRequest("GET", url, nil)
		if secret := getIniConfig("secret"); secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			var result map[string]interface{}
			if json.NewDecoder(resp.Body).Decode(&result) == nil {
				if meta, ok := result["metadata"].(map[string]interface{}); ok {
					if cdp, ok := meta["cdp"].(string); ok {
						closeBrowserTab(cdp)
					}
				}
			}
			resp.Body.Close()
		}
	}
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	absBaseDir, _ := filepath.Abs(baseDir)

	for {
		if atomic.LoadInt32(&isReallyExiting) == 1 {
			return
		}

		if !isProcessRunning("mihomo.exe") {
			atomic.StoreInt32(&isSystemInitializing, 1)
			atomic.StoreInt32(&hasFirstSynced, 0)
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

				go func() {
					success := false
					for i := 0; i < 12; i++ {
						time.Sleep(500 * time.Millisecond)
						resp, err := doAPIRequest("GET", "/configs", nil)
						if err == nil && len(resp) > 200 {
							syncConfigToKernel()
							state := checkSystemState()
							globalLastHasTun = (state == StateTun)
							syncUIAppearance(state)
							success = true
							break
						}
					}
					atomic.StoreInt32(&isSystemInitializing, 0)
					if !success {
						syncUIAppearance(checkSystemState())
					}
				}()

				go func(c *exec.Cmd) {
					_ = c.Wait()
					atomic.StoreInt32(&isKernelActive, 0)
				}(cmd)
			} else {
				atomic.StoreInt32(&isSystemInitializing, 0)
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func monitorIconState() {
	var failCount int
	for {
		if atomic.LoadInt32(&isReallyExiting) == 1 {
			return
		}

		if !isProcessRunning("mihomo.exe") {
			failCount = 0
			if lastState != StateStop {
				updateIconByState(StateStop)
				lastState = StateStop
			}
		} else {
			curr := checkSystemState()
			isTunMode := (getIniConfig("tun_enabled") == "true")
			hasTun := false
			ifaces, _ := net.Interfaces()
			for _, i := range ifaces {
				if isTunInterfaceMatch(i.Name) {
					hasTun = true
					break
				}
			}

			if isTunMode && !hasTun {
				actualState := checkSystemState()
				if actualState != StateTun && actualState != StateStop {
					failCount = 0
					lastState = actualState
					updateIconByState(actualState)
					if mTun != nil {
						mTun.Uncheck()
					}
					time.Sleep(1 * time.Second)
					continue
				}
				if atomic.LoadInt32(&isSystemInitializing) == 1 {
					goto UseFailCountLogic
				} else {
					failCount = 0
					if lastState != StateError {
						updateIconByState(StateError)
						lastState = StateError
					}
					time.Sleep(1 * time.Second)
					continue
				}
			}

		UseFailCountLogic:
			if curr == StateStop {
				failCount++
				if failCount > 5 {
					if lastState != StateError {
						updateIconByState(StateError)
						lastState = StateError
					}
				}
			} else {
				failCount = 0
				if curr != lastState {
					updateIconByState(curr)
					lastState = curr
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
}

func watchTunState() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if atomic.LoadInt32(&isReallyExiting) == 1 {
				return
			}
			if atomic.LoadInt32(&isSystemInitializing) == 1 || atomic.LoadInt32(&isSyncing) == 1 {
				continue
			}

			currentHasTun := false
			ifaces, err := net.Interfaces()
			if err == nil {
				for _, i := range ifaces {
					if isTunInterfaceMatch(i.Name) {
						currentHasTun = true
						break
					}
				}
			}

			if currentHasTun != globalLastHasTun {
				if atomic.LoadInt32(&isKernelActive) == 1 {
					globalLastHasTun = currentHasTun
					atomic.StoreInt32(&hasFirstSynced, 1)
					saveIniConfig("tun_enabled", fmt.Sprint(currentHasTun))
					newState := checkSystemState()
					syncUIAppearance(newState)
				}
			}
		}
	}
}

func checkSystemState() int {
	if atomic.LoadInt32(&isSystemInitializing) == 1 {
		return StateStop
	}

	_, err := doAPIRequest("GET", "/", nil)
	if err != nil {
		return StateStop
	}

	hasTunOnSystem := false
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, i := range ifaces {
			if isTunInterfaceMatch(i.Name) {
				hasTunOnSystem = true
				break
			}
		}
	}

	globalLastHasTun = hasTunOnSystem

	if hasTunOnSystem {
		return StateTun
	}

	if getIniConfig("system_proxy_enabled") == "true" {
		return StateProxy
	}

	return StateDefault
}

func syncConfigToKernel() {
	if !atomic.CompareAndSwapInt32(&isSyncing, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&isSyncing, 0)

	atomic.StoreInt32(&isSystemInitializing, 1)
	timer := time.AfterFunc(10*time.Second, func() { atomic.StoreInt32(&isSystemInitializing, 0) })
	defer timer.Stop()

	tunEnabled := getIniConfig("tun_enabled") == "true"
	payload := map[string]interface{}{
		"mode": getIniConfig("mode"),
		"tun":  map[string]bool{"enable": tunEnabled},
	}

	success := false
	for i := 0; i < 3; i++ {
		_, err := doAPIRequest("PATCH", "/configs", payload)
		if err == nil {
			success = true
			break
		}
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}

	if success {
		if mTun != nil {
			if tunEnabled {
				mTun.Check()
			} else {
				mTun.Uncheck()
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	atomic.StoreInt32(&isSystemInitializing, 0)
}

func syncUIAppearance(state int) {
	updateIconByState(state)
	if mTun != nil {
		if state == StateTun {
			mTun.Check()
		} else {
			mTun.Uncheck()
		}
	}
}

func doAPIRequest(method, path string, payload interface{}) ([]byte, error) {
	apiAddr := strings.TrimSuffix(getIniConfig("external-controller"), "/")
	if apiAddr == "" {
		return nil, fmt.Errorf("api address is empty")
	}
	url := apiAddr + "/" + strings.TrimPrefix(path, "/")

	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal payload failed: %v", err)
		}
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
	defer resp.Body.Close()

	if method == "GET" && (path == "" || path == "/") {
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("API Heartbeat Error: %d", resp.StatusCode)
		}
		return nil, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body failed: %v", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("API Error: %d, Response: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

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
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	inTunSection := false
	foundMixed := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, "mixed-port:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				port := strings.Trim(parts[1], " \"'")
				if port != "" {
					saveIniConfig("proxy_address", "127.0.0.1:"+port)
					foundMixed = true
				}
			}
		} else if !foundMixed && strings.HasPrefix(trimmed, "port:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				port := strings.Trim(parts[1], " \"'")
				if port != "" {
					saveIniConfig("proxy_address", "127.0.0.1:"+port)
				}
			}
		}

		if strings.HasPrefix(trimmed, "tun:") {
			inTunSection = true
			continue
		}
		if inTunSection && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inTunSection = false
		}
		if inTunSection && strings.Contains(trimmed, "device:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
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
				if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
					addr = "http://" + addr
				}
				saveIniConfig("external-controller", addr)
			}
		}

		if strings.HasPrefix(trimmed, "secret:") {
			val := strings.Trim(strings.TrimPrefix(trimmed, "secret:"), " \"'")
			saveIniConfig("secret", val)
		}
	}
}

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	_, _ = doAPIRequest("PATCH", "/configs", map[string]string{"mode": mode})
}

func setTunMode(enable bool) {
	atomic.StoreInt32(&isSystemInitializing, 1)
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	_, _ = doAPIRequest("PATCH", "/configs", map[string]interface{}{"tun": map[string]bool{"enable": enable}})
	time.Sleep(3 * time.Second)
	atomic.StoreInt32(&isSystemInitializing, 0)
}

func setProxyRegistry(enable bool) {
	if atomic.LoadInt32(&isReallyExiting) == 0 {
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
		if err := cmd.Run(); err == nil {
			success = true
		}
	} else {
		cmd := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F")
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil || !checkAutoStartStatus() {
			success = true
		}
	}
	if success {
		saveIniConfig("startup_enabled", fmt.Sprint(enable))
	}
}

func reloadConfigFile() {
	atomic.StoreInt32(&isSystemInitializing, 1)
	_, err := doAPIRequest("PUT", "/configs?force=false", map[string]string{"path": filepath.Join(baseDir, "config.yaml")})
	if err != nil {
		atomic.StoreInt32(&isSystemInitializing, 0)
		return
	}
	go syncConfigToKernel()
}

func launchWebUI() {
	targetURL := "https://metacubex.github.io/metacubexd"
	apiAddr := getIniConfig("external-controller")
	secret := getIniConfig("secret")
	if apiAddr != "" {
		targetURL += fmt.Sprintf("/?hostname=%s&port=%s&secret=%s",
			strings.TrimPrefix(strings.Split(apiAddr, ":")[1], "//"),
			strings.Split(apiAddr, ":")[2],
			secret)
	}

	resp, err := httpClient.Get("http://127.0.0.1:9222/json")
	if err == nil {
		defer resp.Body.Close()
		var targets []map[string]interface{}
		if json.NewDecoder(resp.Body).Decode(&targets) == nil {
			for _, t := range targets {
				if strings.Contains(t["url"].(string), "metacubexd") {
					_ = exec.Command("cmd", "/c", "start", "chrome", "--reuse-tab", t["url"].(string)).Run()
					focusWindowSilky("Metacubexd")
					return
				}
			}
		}
	}
	_ = exec.Command("cmd", "/c", "start", targetURL).Run()
}

func focusWindowSilky(titlePart string) {
	cb := syscall.NewCallback(func(hwnd windows.HWND, lparam uintptr) uintptr {
		b := make([]uint16, 255)
		_, err := windows.GetWindowText(hwnd, &b[0], int32(len(b)))
		if err == nil && strings.Contains(windows.UTF16ToString(b), titlePart) {
			windows.ShowWindow(hwnd, windows.SW_RESTORE)
			windows.SetForegroundWindow(hwnd)
			return 0
		}
		return 1
	})
	_ = windows.EnumWindows(cb, 0)
}

func closeBrowserTab(cdpURL string) {
	if cdpURL == "" {
		return
	}
	id := cdpURL[strings.LastIndex(cdpURL, "/")+1:]
	req, _ := http.NewRequest("GET", "http://127.0.0.1:9222/json/close/"+id, nil)
	_, _ = httpClient.Do(req)
}

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
	_ = windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_SHOWNORMAL)
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil {
		return false
	}
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			if pe.ProcessID != uint32(os.Getpid()) {
				return true
			}
		}
		if err := windows.Process32Next(h, &pe); err != nil {
			break
		}
	}
	return false
}

func KillProcessByName(name string) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(snapshot)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snapshot, &pe); err != nil {
		return
	}
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			pid := pe.ProcessID
			if pid != uint32(os.Getpid()) {
				h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_TERMINATE, false, pid)
				if err == nil {
					_ = windows.TerminateProcess(h, 9)
					windows.CloseHandle(h)
				}
			}
		}
		if err := windows.Process32Next(snapshot, &pe); err != nil {
			break
		}
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
	if key != "" {
		old, ok := configData[key]
		if ok && old == val {
			configMu.Unlock()
			return
		}
		configData[key] = val
	}
	keys := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range keys {
		if v, ok := configData[k]; ok {
			buf.WriteString(k + " = " + v + "\n")
		}
	}
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func isTunInterfaceMatch(ifaceName string) bool {
	name := strings.ToLower(ifaceName)
	target := strings.ToLower(getIniConfig("tun_device_name"))
	if target != "" && strings.Contains(name, target) {
		return true
	}
	for _, kw := range []string{"mihomo", "meta", "clash", "sing-box", "wintun"} {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

func initJobObject() {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		windows.CloseHandle(h)
		return
	}
	hJob = h
}
