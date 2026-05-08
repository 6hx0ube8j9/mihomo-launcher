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
	isSystemInitializing = true

	// 全量定义菜单指针，避免局部变量阴影
	mTun     *systray.MenuItem
	mProxy   *systray.MenuItem
	mStartup *systray.MenuItem
	iconData []byte
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

// --- 配置管理 ---

func ensureDefaultConfig() {
	configMu.Lock()
	defer configMu.Unlock()

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

	changed := false
	for _, pair := range defaults {
		if val, exists := configData[pair[0]]; !exists || val == "" {
			configData[pair[0]] = pair[1]
			changed = true
		}
	}

	if changed {
		var buf bytes.Buffer
		for k, v := range configData {
			if k != "" {
				buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
			}
		}
		_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
	}
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
	var buf bytes.Buffer
	for k, v := range configData {
		if k = strings.TrimSpace(k); k != "" {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	content := buf.Bytes()
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), content, 0644)
}

// --- 网络与内核通信 ---

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

func syncAll() {
	payload := map[string]interface{}{
		"mode": getIniConfig("mode"),
		"tun":  map[string]bool{"enable": getIniConfig("tun_enabled") == "true"},
	}
	resp, err := doAPIRequest("PATCH", "/configs", payload)
	if err == nil {
		resp.Body.Close()
	}
	if getIniConfig("system_proxy_enabled") == "true" {
		setProxyRegistry(true)
	}
}

// --- 监控守护协程 ---

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
		curr := StateStop
		if isProcessRunning("mihomo.exe") {
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
	resp, err := doAPIRequest("GET", "/configs", nil)
	if err != nil {
		tunErrorCounter = 0
		return StateStop
	}
	resp.Body.Close()

	onceSync.Do(func() { go syncAll() })

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
		if tunErrorCounter > 5 {
			return StateError
		}
		return StateTun
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
		time.Sleep(500 * time.Millisecond)

		hasTun := false
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				if isTunInterfaceMatch(i.Name) {
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
			currentConfig := getIniConfig("tun_enabled") == "true"
			if hasTun != currentConfig {
				saveIniConfig("tun_enabled", fmt.Sprint(hasTun))
			}
		}
		time.Sleep(200 * time.Millisecond)
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

// --- UI 与 系统操作 ---

func onReady() {
	systray.SetTemplateIcon(iconData, iconData)
	systray.SetTooltip("Mihomo Launcher")

	mStatus := systray.AddMenuItem("状态: 检查中...", "")
	mStatus.Disable()
	systray.AddSeparator()

	mMode := systray.AddMenuItem("代理模式", "")
	modeMenus := map[string]*systray.MenuItem{
		"rule":   mMode.AddSubMenuItem("规则模式", ""),
		"global": mMode.AddSubMenuItem("全局模式", ""),
		"direct": mMode.AddSubMenuItem("直连模式", ""),
	}

	mTun = systray.AddMenuItem("TUN 模式", "")
	mProxy = systray.AddMenuItem("系统代理", "")
	mStartup = systray.AddMenuItem("开机自启", "")

	systray.AddSeparator()
	mReload := systray.AddMenuItem("重载配置文件", "")
	mRestart := systray.AddMenuItem("重启内核", "")

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "")

	// 初始状态勾选
	currentMode := getIniConfig("mode")
	if item, ok := modeMenus[currentMode]; ok {
		item.Check()
	}
	if getIniConfig("tun_enabled") == "true" {
		mTun.Check()
	}
	if getIniConfig("system_proxy_enabled") == "true" {
		mProxy.Check()
	}
	if getIniConfig("startup_enabled") == "true" {
		mStartup.Check()
	}

	isSystemInitializing = false

	go func() {
		for {
			select {
			case <-modeMenus["rule"].ClickedCh:
				saveIniConfig("mode", "rule")
				syncAll()
				for k, v := range modeMenus {
					if k == "rule" {
						v.Check()
					} else {
						v.Uncheck()
					}
				}
			case <-modeMenus["global"].ClickedCh:
				saveIniConfig("mode", "global")
				syncAll()
				for k, v := range modeMenus {
					if k == "global" {
						v.Check()
					} else {
						v.Uncheck()
					}
				}
			case <-modeMenus["direct"].ClickedCh:
				saveIniConfig("mode", "direct")
				syncAll()
				for k, v := range modeMenus {
					if k == "direct" {
						v.Check()
					} else {
						v.Uncheck()
					}
				}
			case <-mTun.ClickedCh:
				isSystemInitializing = true
				newState := !mTun.Checked()
				saveIniConfig("tun_enabled", fmt.Sprint(newState))
				syncAll()
				if newState {
					mTun.Check()
				} else {
					mTun.Uncheck()
				}
				go func() { time.Sleep(2 * time.Second); isSystemInitializing = false }()
			case <-mProxy.ClickedCh:
				newState := !mProxy.Checked()
				setProxyRegistry(newState)
				if newState {
					mProxy.Check()
				} else {
					mProxy.Uncheck()
				}
			case <-mStartup.ClickedCh:
				newState := !mStartup.Checked()
				if setStartup(newState) {
					saveIniConfig("startup_enabled", fmt.Sprint(newState))
					if newState {
						mStartup.Check()
					} else {
						mStartup.Uncheck()
					}
				}
			case <-mReload.ClickedCh:
				body := map[string]string{"path": filepath.Join(baseDir, "config.yaml")}
				_, _ = doAPIRequest("PATCH", "/configs", body)
			case <-mRestart.ClickedCh:
				isSystemInitializing = true
				killCmd := exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T")
				killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
				_ = killCmd.Run()
				go func() { time.Sleep(5 * time.Second); isSystemInitializing = false }()
			case <-mQuit.ClickedCh:
				isReallyExiting = true
				systray.Quit()
			}
		}
	}()
}

func onExit() {
	if isReallyExiting {
		setProxyRegistry(false)
		if hJob != 0 {
			windows.CloseHandle(hJob)
		}
		if hMutex != 0 {
			windows.CloseHandle(hMutex)
		}
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
		_ = key.SetStringValue("ProxyServer", getIniConfig("proxy_address"))
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func setStartup(enable bool) bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	if enable {
		err = key.SetStringValue(APP_NAME, `"`+exePath+`"`)
	} else {
		err = key.DeleteValue(APP_NAME)
	}
	return err == nil || err == registry.ErrNotExist
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state >= 0 && state < len(files) {
		b, err := iconFs.ReadFile("icons/" + files[state])
		if err == nil {
			systray.SetIcon(b)
		}
	}
}

func init() {
	ensureDefaultConfig()
	sniffAndSolidifyConfig()
	b, err := iconFs.ReadFile("icons/default.ico")
	if err == nil {
		iconData = b
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
			windows.CloseHandle(hMutex); hMutex = 0
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
