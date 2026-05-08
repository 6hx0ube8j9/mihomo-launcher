package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// --- 动态固化变量 ---
	ExternalController string
	Secret             string
	MixedPort          string

	// --- 状态控制 ---
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
	isSystemInitializing = true // 蓝本核心：启动锁
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

// --- 配置管理（手术区：严格遵循蓝本原子写入，仅修改字段名） ---

func sniffAndSolidifyConfig() {
	configPath := filepath.Join(baseDir, "config.yaml")
	file, err := os.Open(configPath)
	
	yamlController := "http://127.0.0.1:9090"
	yamlSecret := ""
	yamlPort := "7890"

	if err == nil {
		defer file.Close()
		reController := regexp.MustCompile(`(?m)^\s*external-controller:\s*['"]?([^'"]+?)['"]?`)
		reSecret := regexp.MustCompile(`(?m)^\s*secret:\s*['"]?([^'"]+?)['"]?`)
		reMixed := regexp.MustCompile(`(?m)^\s*mixed-port:\s*(\d+)`)
		rePort := regexp.MustCompile(`(?m)^\s*port:\s*(\d+)`)

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()
			if m := reController.FindStringSubmatch(line); len(m) > 1 {
				yamlController = m[1]
			} else if m := reSecret.FindStringSubmatch(line); len(m) > 1 {
				yamlSecret = m[1]
			} else if m := reMixed.FindStringSubmatch(line); len(m) > 1 {
				yamlPort = m[1]
			} else if m := rePort.FindStringSubmatch(line); len(m) > 1 && yamlPort == "7890" {
				yamlPort = m[1]
			}
		}
	}

	if !strings.HasPrefix(yamlController, "http") {
		yamlController = "http://" + yamlController
	}
	ExternalController = strings.TrimSuffix(yamlController, "/")
	Secret = yamlSecret
	MixedPort = "127.0.0.1:" + yamlPort

	configMu.Lock()
	configData["proxy_address"] = MixedPort
	configData["external-controller"] = ExternalController
	configData["secret"] = Secret
	configMu.Unlock()
	
	// 初始化完成后立即保存一次全量 INI
	saveIniConfig("", "") 
}

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	configMu.Lock()
	defer configMu.Unlock()

	configData = make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") { continue }
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	// 使用你要求的 INI 字段名
	defaults := map[string]string{
		"mode":            "rule",
		"tun":             "false",
		"system_proxy":    "false",
		"startup_enabled": "false",
	}

	needsSave := false
	for k, v := range defaults {
		if _, exists := configData[k]; !exists {
			configData[k] = v
			needsSave = true
		}
	}

	if needsSave {
		configMu.Unlock() // 解锁去执行保存，保存函数内部有自己的锁
		saveIniConfig("", "")
		configMu.Lock() 
	}
}

func getIniConfig(key string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configData[key]
}

// 蓝本原装：原子性保存逻辑 (无损移植)
func saveIniConfig(key, val string) {
	configMu.Lock()
	if key != "" { configData[key] = val }
	
	// 按照美观顺序排列
	order := []string{"mode", "tun", "system_proxy", "startup_enabled", "proxy_address", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range order {
		if v, ok := configData[k]; ok {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	content := buf.Bytes()
	configMu.Unlock()

	configPath := filepath.Join(baseDir, CONFIG_FILE)
	tmpPath := configPath + ".tmp"

	if err := os.WriteFile(tmpPath, content, 0644); err != nil { return }
	os.Remove(configPath)
	if err := os.Rename(tmpPath, configPath); err != nil {
		_ = os.Rename(tmpPath, configPath)
	}
}

// --- 核心逻辑 (基于蓝本，注入 Secret 和动态 API) ---

func sendAPIRequest(method, path string, payload interface{}) {
	jsonBody, _ := json.Marshal(payload)
	req, err := http.NewRequest(method, ExternalController+path, bytes.NewBuffer(jsonBody))
	if err != nil { return }

	if Secret != "" {
		req.Header.Set("Authorization", "Bearer "+Secret)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func syncConfigToKernel() {
	configMu.RLock()
	tun := configData["tun"] == "true"
	mode := configData["mode"]
	if mode == "" { mode = "rule" }
	proxy := configData["system_proxy"] == "true"
	configMu.RUnlock()

	payload := map[string]interface{}{
		"mode": mode,
		"tun":  map[string]bool{"enable": tun},
	}
	sendAPIRequest("PATCH", "/configs", payload)
	
	if proxy { setProxyRegistry(true) }
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
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
	// 1. 探测 API 状态（注入 Secret）
	req, _ := http.NewRequest("GET", ExternalController+"/version", nil)
	if Secret != "" { req.Header.Set("Authorization", "Bearer "+Secret) }
	
	resp, err := httpClient.Do(req)
	if err != nil {
		tunErrorCounter = 0
		return StateStop
	}
	resp.Body.Close()

	// 2. 蓝本原装解锁逻辑
	if isSystemInitializing { isSystemInitializing = false }

	// 3. 确保启动后只执行一次配置对齐
	onceSync.Do(func() { go syncConfigToKernel() })

	configMu.RLock()
	wantTun := configData["tun"] == "true"
	wantProxy := configData["system_proxy"] == "true"
	configMu.RUnlock()

	if wantTun {
		hasTunInterface := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			name := strings.ToLower(i.Name)
			if strings.Contains(name, "mihomo") || strings.Contains(name, "meta") || strings.Contains(name, "clash") {
				hasTunInterface = true
				break
			}
		}
		if hasTunInterface {
			tunErrorCounter = 0
			return StateTun
		} else {
			tunErrorCounter++
			if tunErrorCounter > 5 { return StateError }
			return StateTun
		}
	}
	if wantProxy { return StateProxy }
	return StateDefault
}

// 蓝本原装：系统信号级网卡监听 (无损移植)
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
				if strings.Contains(name, "mihomo") || strings.Contains(name, "meta") || strings.Contains(name, "clash") {
					hasTun = true
					break
				}
			}
		}

		if mTun != nil && !isSystemInitializing {
			if hasTun { mTun.Check() } else { mTun.Uncheck() }
			saveIniConfig("tun", fmt.Sprint(hasTun))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil { return false }
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil { return false }
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) { return true }
		if err := windows.Process32Next(h, &pe); err != nil { break }
	}
	return false
}

func onReady() {
	loadIniConfigAll()
	// 如果配置中还没有动态变量，则嗅探一次
	if getIniConfig("external-controller") == "" {
		sniffAndSolidifyConfig()
	} else {
		ExternalController = getIniConfig("external-controller")
		Secret = getIniConfig("secret")
		MixedPort = getIniConfig("proxy_address")
	}

	setProxyRegistry(getIniConfig("system_proxy") == "true")
	updateIconByState(StateStop)

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy") == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun") == "true")
	systray.AddSeparator()

	curMode := getIniConfig("mode")
	modeMenus := make(map[string]*systray.MenuItem)
	modeMenus["rule"] = systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule" || curMode == "")
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
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(ExternalController+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
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
			if next { mTun.Check() } else { mTun.Uncheck() }
		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			saveIniConfig("system_proxy", fmt.Sprint(next))
			setProxyRegistry(next)
			if next { mProxy.Check() } else { mProxy.Uncheck() }
		case <-mAuto.ClickedCh:
			next := !mAuto.Checked()
			toggleAutoStart(next)
			if next { mAuto.Check() } else { mAuto.Uncheck() }
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRestart.ClickedCh:
			exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
			configMu.Lock(); onceSync = sync.Once{}; configMu.Unlock()
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
		if hJob != 0 { windows.CloseHandle(hJob) }
		if hMutex != 0 { windows.CloseHandle(hMutex) }
	}
}

// --- 系统操作 ---

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	sendAPIRequest("PATCH", "/configs", map[string]string{"mode": mode})
}

func setTunMode(enable bool) {
	isSystemInitializing = true
	saveIniConfig("tun", fmt.Sprint(enable))
	sendAPIRequest("PATCH", "/configs", map[string]interface{}{"tun": map[string]bool{"enable": enable}})
	go func() { 
		time.Sleep(3 * time.Second) 
		isSystemInitializing = false 
	}()
}

func setProxyRegistry(enable bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	if err != nil { return }
	defer key.Close()
	if enable {
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", MixedPort)
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func toggleAutoStart(enable bool) {
	saveIniConfig("startup_enabled", fmt.Sprint(enable))
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE)
	defer key.Close()
	if enable { _ = key.SetStringValue(APP_NAME, exePath) } else { _ = key.DeleteValue(APP_NAME) }
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state >= 0 && state < len(files) {
		b, err := iconFs.ReadFile("icons/" + files[state])
		if err == nil { systray.SetIcon(b) }
	}
}

func main() {
	if !isAdmin() { runAsAdmin(); return }
	
	h, _ := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(APP_MUTEX))
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS { os.Exit(0) }
	hMutex = h

	os.Chdir(baseDir)
	initJobObject()

	go monitorKernelDaemon()
	go monitorIconState()
	go watchTunState()

	systray.Run(onReady, onExit)
}
