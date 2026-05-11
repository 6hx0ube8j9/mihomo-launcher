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

	"github.com/energye/systray"
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
	HWND_TOPMOST = ^uintptr(0)
	HWND_NOTOPMOST = ^uintptr(1)
	SW_RESTORE   = 9
	SWP_SILKY    = 0x0043
	debugPort    = "52719"
)

var (
	hJob, hMutex         windows.Handle
	httpClient           = &http.Client{Timeout: 1 * time.Second}
	exePath, _           = os.Executable()
	baseDir              = filepath.Dir(exePath)
	isSystemInitializing int32 = 1
	isSyncing, isReallyExiting, hasFirstSynced, isKernelActive, isFocusing int32
	exitOnce             sync.Once
	configMu             sync.RWMutex
	configData           = make(map[string]string)
	lastState            = -1
	mTun                 *systray.MenuItem
	globalLastHasTun     bool
	u32                  = windows.NewLazySystemDLL("user32.dll")
	k32                  = windows.NewLazySystemDLL("kernel32.dll")
	procEnumWindows      = u32.NewProc("EnumWindows")
	procGetClassName     = u32.NewProc("GetClassNameW")
	procIsWindowVisible  = u32.NewProc("IsWindowVisible")
	procGetWindowThread  = u32.NewProc("GetWindowThreadProcessId")
	procSetWindowPos     = u32.NewProc("SetWindowPos")
	procShowWindow       = u32.NewProc("ShowWindow")
	procSetForeground    = u32.NewProc("SetForegroundWindow")
	procBringToTop       = u32.NewProc("BringWindowToTop")
	procGetForeground    = u32.NewProc("GetForegroundWindow")
	procAttachThread     = u32.NewProc("AttachThreadInput")
	procKeybdEvent       = u32.NewProc("keybd_event")
	procGetCurrentThread = k32.NewProc("GetCurrentThreadId")
)

func main() {
	exePath, _ = os.Executable()
	baseDir = filepath.Dir(exePath)
	_ = os.Chdir(baseDir)
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 { windows.CloseHandle(h) }
		return
	}
	hMutex = h
	isAutostart := false
	for _, arg := range os.Args {
		if arg == "---autostart" { isAutostart = true; break }
	}
	if !isAdmin() && !isAutostart { runAsAdmin(); return }
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
}

func onReady() {
	saveIniConfig("startup_enabled", fmt.Sprint(checkAutoStartStatus()))
	ensureDefaultConfig()
	sniffAndSolidifyConfig()
	setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
	updateIconByState(StateStop)
	systray.SetOnClick(func(menu systray.IMenu) { go launchWebUI() })
	systray.AddMenuItem("进入 Web 面板", "").Click(func() { go launchWebUI() })
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
	mProxy.Click(func() {
		next := !mProxy.Checked()
		saveIniConfig("system_proxy_enabled", fmt.Sprint(next))
		setProxyRegistry(next)
		if next { mProxy.Check() } else { mProxy.Uncheck() }
	})
	mTun = systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", getIniConfig("tun_enabled") == "true")
	mTun.Click(func() {
		next := getIniConfig("tun_enabled") != "true"
		saveIniConfig("tun_enabled", fmt.Sprint(next))
		if next { mTun.Check() } else { mTun.Uncheck() }
		go func() {
			setTunMode(next)
			syncUIAppearance(checkSystemState())
		}()
	})
	systray.AddSeparator()
	mModeRoot := systray.AddMenuItem("模式切换", "")
	curMode := getIniConfig("mode")
	modeMenus := make(map[string]*systray.MenuItem)
	setupMode := func(key, label string) {
		modeMenus[key] = mModeRoot.AddSubMenuItemCheckbox(label, "", curMode == key)
		modeMenus[key].Click(func() {
			saveIniConfig("mode", key)
			for k, menu := range modeMenus {
				if k == key { menu.Check() } else { menu.Uncheck() }
			}
			go setMihomoMode(key)
		})
	}
	setupMode("rule", "规则模式"); setupMode("global", "全局模式"); setupMode("direct", "直连模式")
	systray.AddSeparator()
	systray.AddMenuItem("打开目录", "").Click(func() {
		windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
	})
	mMoreRoot := systray.AddMenuItem("更多", "")
	mAuto := mMoreRoot.AddSubMenuItemCheckbox("开机自启动", "", checkAutoStartStatus())
	mAuto.Click(func() {
		next := !mAuto.Checked()
		toggleAutoStart(next)
		saveIniConfig("startup_enabled", fmt.Sprint(next))
		if next { mAuto.Check() } else { mAuto.Uncheck() }
	})
	mMoreRoot.AddSubMenuItem("重启内核", "").Click(func() {
		atomic.StoreInt32(&isSystemInitializing, 1)
		atomic.StoreInt32(&hasFirstSynced, 0)
		KillProcessByName("mihomo.exe")
	})
	mMoreRoot.AddSubMenuItem("重载配置文件", "").Click(func() {
		sniffAndSolidifyConfig()
		reloadConfigFile()
		syncUIAppearance(checkSystemState())
	})
	systray.AddSeparator()
	systray.AddMenuItem("关闭程序", "").Click(func() {
		atomic.StoreInt32(&isReallyExiting, 1)
		systray.Quit()
	})
}

func checkSystemState() int {
	hasTunOnSystem := false
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if isTunInterfaceMatch(i.Name) { hasTunOnSystem = true; break }
	}
	resp, err := doAPIRequest("GET", "/configs", nil)
	if err != nil { return StateStop }
	isBusy := atomic.LoadInt32(&isSystemInitializing) == 1 || atomic.LoadInt32(&isSyncing) == 1
	if !isBusy {
		respStr := string(resp)
		kernelTunEnabled := strings.Contains(respStr, `"tun":`) && strings.Contains(respStr, `"enable":true`)
		if getIniConfig("tun_enabled") == "true" && !kernelTunEnabled {
			if !hasTunOnSystem { go syncConfigToKernel(); return StateError }
		}
	}
	globalLastHasTun = hasTunOnSystem
	if getIniConfig("tun_enabled") == "true" {
		if hasTunOnSystem { return StateTun }
		return StateError
	}
	if getIniConfig("system_proxy_enabled") == "true" { return StateProxy }
	return StateDefault
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	absBaseDir, _ := filepath.Abs(baseDir)
	for {
		if atomic.LoadInt32(&isReallyExiting) == 1 { return }
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
					if !success { syncUIAppearance(checkSystemState()) }
				}()
				go func(c *exec.Cmd) { _ = c.Wait(); atomic.StoreInt32(&isKernelActive, 0) }(cmd)
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
		if atomic.LoadInt32(&isReallyExiting) == 1 { return }
		if !isProcessRunning("mihomo.exe") {
			failCount = 0
			if lastState != StateStop { updateIconByState(StateStop); lastState = StateStop }
		} else {
			curr := checkSystemState()
			isTunMode := (getIniConfig("tun_enabled") == "true")
			hasTun := false
			ifaces, _ := net.Interfaces()
			for _, i := range ifaces {
				if isTunInterfaceMatch(i.Name) { hasTun = true; break }
			}
			if isTunMode && !hasTun {
				actualState := checkSystemState()
				if actualState != StateTun && actualState != StateStop {
					failCount = 0; lastState = actualState; updateIconByState(actualState)
					if mTun != nil { mTun.Uncheck() }
					time.Sleep(1 * time.Second); continue
				}
				if atomic.LoadInt32(&isSystemInitializing) == 0 {
					failCount = 0
					if lastState != StateError { updateIconByState(StateError); lastState = StateError }
					time.Sleep(1 * time.Second); continue
				}
			}
			if curr == StateStop {
				failCount++
				if failCount > 5 && lastState != StateError { updateIconByState(StateError); lastState = StateError }
			} else {
				failCount = 0
				if curr != lastState { updateIconByState(curr); lastState = curr }
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
			if atomic.LoadInt32(&isReallyExiting) == 1 { return }
			if atomic.LoadInt32(&isSystemInitializing) == 1 || atomic.LoadInt32(&isSyncing) == 1 { continue }
			currentHasTun := false
			ifaces, err := net.Interfaces()
			if err == nil {
				for _, i := range ifaces {
					if isTunInterfaceMatch(i.Name) { currentHasTun = true; break }
				}
			}
			if currentHasTun != globalLastHasTun && atomic.LoadInt32(&isKernelActive) == 1 {
				globalLastHasTun = currentHasTun
				atomic.StoreInt32(&hasFirstSynced, 1)
				saveIniConfig("tun_enabled", fmt.Sprint(currentHasTun))
				syncUIAppearance(checkSystemState())
			}
		}
	}
}

func launchWebUI() {
	apiAddr := getIniConfig("external-controller")
	secret := getIniConfig("secret")
	proxyAddr := getIniConfig("proxy_address")
	baseUI := strings.TrimRight(apiAddr, "/")
	if !strings.HasPrefix(baseUI, "http") { baseUI = "http://" + baseUI }
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(baseUI, "http://"), "https://"))
	if port == "" { host, port = "127.0.0.1", "9090" }
	finalURL := fmt.Sprintf("%s/ui/?hostname=%s&port=%s&secret=%s#/proxies", baseUI, host, port, secret)
	client := &http.Client{Timeout: 300 * time.Millisecond}
	if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json", debugPort)); err == nil {
		defer resp.Body.Close()
		var targets []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&targets); err == nil {
			for _, t := range targets {
				pURL, _ := t["url"].(string)
				if strings.Contains(pURL, "/ui/") || strings.Contains(pURL, "setup") {
					id, _ := t["id"].(string)
					client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/activate/%s", debugPort, id))
					go func() {
						time.Sleep(100 * time.Millisecond)
						var targetHwnd uintptr
						procEnumWindows.Call(windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
							var buf [256]uint16
							procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
							if windows.UTF16ToString(buf[:]) == "Chrome_WidgetWin_1" {
								if vis, _, _ := procIsWindowVisible.Call(hwnd); vis != 0 {
									targetHwnd = hwnd; return 0
								}
							}
							return 1
						}), 0)
						if targetHwnd != 0 { focusWindowSilky(targetHwnd) }
					}()
					return
				}
			}
		}
	}
	var browserPath string
	potentialPaths := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
	}
	for _, p := range potentialPaths {
		if _, err := os.Stat(p); err == nil { browserPath = p; break }
	}
	if browserPath != "" {
		userDataDir := filepath.Join(baseDir, "WebCache")
		_ = os.MkdirAll(userDataDir, 0755)
		args := []string{"--app=" + finalURL, "--remote-debugging-port=" + debugPort, "--user-data-dir=" + userDataDir, "--window-size=1280,768", "--proxy-server=" + proxyAddr, "--no-first-run"}
		cmd := exec.Command(browserPath, args...)
		if err := cmd.Start(); err == nil && hJob != 0 {
			hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			if err == nil { _ = windows.AssignProcessToJobObject(hJob, hp); _ = windows.CloseHandle(hp) }
		}
	} else {
		_ = exec.Command("cmd", "/c", "start", "", finalURL).Start()
	}
}

func focusWindowSilky(targetHwnd uintptr) {
	if !atomic.CompareAndSwapInt32(&isFocusing, 0, 1) { return }
	defer atomic.StoreInt32(&isFocusing, 0)
	currT, _, _ := procGetCurrentThread.Call()
	foreH, _, _ := procGetForeground.Call()
	foreT, _, _ := procGetWindowThread.Call(foreH, 0)
	targT, _, _ := procGetWindowThread.Call(targetHwnd, 0)
	if foreT != currT { procAttachThread.Call(foreT, currT, 1) }
	procAttachThread.Call(currT, targT, 1)
	procShowWindow.Call(targetHwnd, SW_RESTORE)
	procSetForeground.Call(targetHwnd)
	procBringToTop.Call(targetHwnd)
	procSetWindowPos.Call(targetHwnd, HWND_TOPMOST, 0, 0, 0, 0, SWP_SILKY)
	procAttachThread.Call(currT, targT, 0)
	if foreT != currT { procAttachThread.Call(foreT, currT, 0) }
	procKeybdEvent.Call(0x12, 0, 0, 0); procKeybdEvent.Call(0x12, 0, 2, 0)
	time.AfterFunc(400*time.Millisecond, func() { procSetWindowPos.Call(targetHwnd, HWND_NOTOPMOST, 0, 0, 0, 0, SWP_SILKY) })
}

func syncUIAppearance(state int) {
	updateIconByState(state)
	if mTun != nil {
		if state == StateTun { mTun.Check() } else { mTun.Uncheck() }
	}
}

func syncConfigToKernel() {
	if !atomic.CompareAndSwapInt32(&isSyncing, 0, 1) { return }
	defer atomic.StoreInt32(&isSyncing, 0)
	atomic.StoreInt32(&isSystemInitializing, 1)
	time.AfterFunc(10*time.Second, func() { atomic.StoreInt32(&isSystemInitializing, 0) })
	tunEnabled := getIniConfig("tun_enabled") == "true"
	payload := map[string]interface{}{"mode": getIniConfig("mode"), "tun": map[string]bool{"enable": tunEnabled}}
	success := false
	for i := 0; i < 3; i++ {
		_, err := doAPIRequest("PATCH", "/configs", payload)
		if err == nil { success = true; break }
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	if success && mTun != nil {
		if tunEnabled { mTun.Check() } else { mTun.Uncheck() }
		time.Sleep(500 * time.Millisecond)
	}
	atomic.StoreInt32(&isSystemInitializing, 0)
}

func doAPIRequest(method, path string, payload interface{}) ([]byte, error) {
	apiAddr := strings.TrimSuffix(getIniConfig("external-controller"), "/")
	if apiAddr == "" { return nil, fmt.Errorf("empty api") }
	url := apiAddr + "/" + strings.TrimPrefix(path, "/")
	var bodyReader io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		bodyReader = bytes.NewBuffer(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil { return nil, err }
	req.Header.Set("Content-Type", "application/json")
	if secret := getIniConfig("secret"); secret != "" { req.Header.Set("Authorization", "Bearer "+secret) }
	resp, err := httpClient.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	if method == "GET" && (path == "" || path == "/") {
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 { return nil, fmt.Errorf("err %d", resp.StatusCode) }
		return nil, nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 { return body, fmt.Errorf("err %d", resp.StatusCode) }
	return body, nil
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
	defaults := [][]string{{"mode", "rule"}, {"tun_enabled", "false"}, {"system_proxy_enabled", "false"}, {"startup_enabled", "false"}, {"proxy_address", "127.0.0.1:7890"}, {"tun_device_name", "Mihomo"}, {"external-controller", "http://127.0.0.1:9090"}, {"secret", ""}}
	for _, pair := range defaults {
		if val, exists := configData[pair[0]]; !exists || val == "" { configData[pair[0]] = pair[1] }
	}
	configMu.Unlock(); saveIniConfig("", "")
}

func sniffAndSolidifyConfig() {
	data, err := os.ReadFile(filepath.Join(baseDir, "config.yaml"))
	if err != nil { return }
	lines := strings.Split(string(data), "\n")
	inTun, foundMixed := false, false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") { continue }
		if strings.HasPrefix(trimmed, "mixed-port:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				p := strings.Trim(parts[1], " \"'")
				if p != "" { saveIniConfig("proxy_address", "127.0.0.1:"+p); foundMixed = true }
			}
		} else if !foundMixed && strings.HasPrefix(trimmed, "port:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				p := strings.Trim(parts[1], " \"'")
				if p != "" { saveIniConfig("proxy_address", "127.0.0.1:"+p) }
			}
		}
		if strings.HasPrefix(trimmed, "tun:") { inTun = true; continue }
		if inTun && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") { inTun = false }
		if inTun && strings.Contains(trimmed, "device:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				dev := strings.Trim(parts[1], " \"'")
				if dev != "" { saveIniConfig("tun_device_name", dev) }
			}
		}
		if strings.HasPrefix(trimmed, "external-controller:") {
			addr := strings.Trim(strings.TrimPrefix(trimmed, "external-controller:"), " \"'")
			if strings.HasPrefix(addr, ":") { addr = "127.0.0.1" + addr }
			if addr != "" {
				if !strings.HasPrefix(addr, "http") { addr = "http://" + addr }
				saveIniConfig("external-controller", addr)
			}
		}
		if strings.HasPrefix(trimmed, "secret:") { saveIniConfig("secret", strings.Trim(strings.TrimPrefix(trimmed, "secret:"), " \"'")) }
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
	if atomic.LoadInt32(&isReallyExiting) == 0 { saveIniConfig("system_proxy_enabled", fmt.Sprint(enable)) }
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
		_ = key.DeleteValue(APP_NAME); key.Close()
	}
	success := false
	if enable {
		cleanExe := strings.ReplaceAll(exePath, "'", "''")
		cleanDir := strings.ReplaceAll(baseDir, "'", "''")
		psScript := fmt.Sprintf("$trigger = New-ScheduledTaskTrigger -AtLogOn; $trigger.Delay = 'PT8S'; $action = New-ScheduledTaskAction -Execute '%s' -Argument '---autostart' -WorkingDirectory '%s'; $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); Register-ScheduledTask -TaskName '%s' -Trigger $trigger -Action $action -Settings $settings -User $env:USERNAME -RunLevel Highest -Force", cleanExe, cleanDir, taskName)
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
	atomic.StoreInt32(&isSystemInitializing, 1)
	_, err := doAPIRequest("PUT", "/configs?force=false", map[string]string{"path": filepath.Join(baseDir, "config.yaml")})
	if err == nil { go syncConfigToKernel() } else { atomic.StoreInt32(&isSystemInitializing, 0) }
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
				h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_TERMINATE, false, pe.ProcessID)
				if err == nil { _ = windows.TerminateProcess(h, 9); windows.CloseHandle(h) }
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
		if b, err := iconFs.ReadFile("icons/" + files[state]); err == nil { systray.SetIcon(b) }
	}
}

func getIniConfig(key string) string {
	configMu.RLock(); defer configMu.RUnlock()
	return configData[key]
}

func saveIniConfig(key, val string) {
	configMu.Lock()
	if key != "" {
		if old, ok := configData[key]; ok && old == val { configMu.Unlock(); return }
		configData[key] = val
	}
	keys := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range keys {
		if v, ok := configData[k]; ok { buf.WriteString(k + " = " + v + "\n") }
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
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil { return }
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE}}
	_, err = windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil { windows.CloseHandle(h); return }
	hJob = h
}

func onExit() {
	exitOnce.Do(func() {
		atomic.StoreInt32(&isReallyExiting, 1)
		client := &http.Client{Timeout: 200 * time.Millisecond}
		if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json", debugPort)); err == nil {
			var targets []map[string]interface{}
			if json.NewDecoder(resp.Body).Decode(&targets) == nil {
				for _, t := range targets {
					if id, ok := t["id"].(string); ok { client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/close/%s", debugPort, id)) }
				}
			}
			resp.Body.Close()
		}
		setProxyRegistry(false)
		systray.Quit()
		time.Sleep(100 * time.Millisecond)
		if hJob != 0 { windows.CloseHandle(hJob) }
		if hMutex != 0 { windows.CloseHandle(hMutex) }
		os.Exit(0)
	})
}
