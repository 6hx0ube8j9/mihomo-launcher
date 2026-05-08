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
	// 动态嗅探结果
	ExternalController string
	Secret             string
	MixedPort          string

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
	isSystemInitializing = true // 蓝本锁：防止启动网络抖动
)

// --- 基础工具 (保留蓝本) ---

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

// --- 配置管理 (整合蓝本原子保存 + 动态解析) ---

func sniffYAML() {
	configPath := filepath.Join(baseDir, "config.yaml")
	rawCtrl := "127.0.0.1:9090"
	rawSecret := ""
	rawPort := "7890"

	if file, err := os.Open(configPath); err == nil {
		defer file.Close()
		reCtrl := regexp.MustCompile(`(?m)^\s*external-controller:\s*['"]?([^'"]+?)['"]?`)
		reSec := regexp.MustCompile(`(?m)^\s*secret:\s*['"]?([^'"]+?)['"]?`)
		reMix := regexp.MustCompile(`(?m)^\s*mixed-port:\s*(\d+)`)
		rePort := regexp.MustCompile(`(?m)^\s*port:\s*(\d+)`)

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()
			if m := reCtrl.FindStringSubmatch(line); len(m) > 1 { rawCtrl = m[1] }
			if m := reSec.FindStringSubmatch(line); len(m) > 1 { rawSecret = m[1] }
			if m := reMix.FindStringSubmatch(line); len(m) > 1 { rawPort = m[1] }
			if m := rePort.FindStringSubmatch(line); len(m) > 1 && rawPort == "7890" { rawPort = m[1] }
		}
	}

	if !strings.HasPrefix(strings.ToLower(rawCtrl), "http") {
		rawCtrl = "http://" + rawCtrl
	}
	ExternalController = strings.TrimRight(rawCtrl, "/")
	Secret = rawSecret
	MixedPort = "127.0.0.1:" + rawPort

	configMu.Lock()
	configData["proxy_address"] = MixedPort
	configData["external-controller"] = ExternalController
	configData["secret"] = Secret
	configMu.Unlock()
	saveIniConfig("", "")
}

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	configMu.Lock()
	configData = make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") { continue }
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	configMu.Unlock()

	// 强制设定默认字段名
	defaults := map[string]string{
		"mode": "rule", "tun": "false", "system_proxy": "false", "startup_enabled": "false",
	}
	needsSave := false
	for k, v := range defaults {
		if _, exists := configData[k]; !exists {
			configData[k] = v
			needsSave = true
		}
	}
	if needsSave { saveIniConfig("", "") }
}

func getIniConfig(key string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configData[key]
}

// 蓝本原装原子保存逻辑
func saveIniConfig(key, val string) {
	configMu.Lock()
	if key != "" { configData[key] = val }
	var buf bytes.Buffer
	// 按顺序排列更美观
	order := []string{"mode", "tun", "system_proxy", "startup_enabled", "proxy_address", "external-controller", "secret"}
	for _, k := range order {
		if v, ok := configData[k]; ok {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	content := buf.Bytes()
	configMu.Unlock()

	path := filepath.Join(baseDir, CONFIG_FILE)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0644); err == nil {
		os.Remove(path)
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Rename(tmp, path)
		}
	}
}

// --- 核心通讯 (注入 Secret) ---

func apiPATCH(path string, payload interface{}) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PATCH", ExternalController+path, bytes.NewBuffer(body))
	if Secret != "" { req.Header.Set("Authorization", "Bearer "+Secret) }
	req.Header.Set("Content-Type", "application/json")
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func syncToKernel() {
	configMu.RLock()
	tun := configData["tun"] == "true"
	mode := configData["mode"]
	proxy := configData["system_proxy"] == "true"
	configMu.RUnlock()

	apiPATCH("/configs", map[string]interface{}{
		"mode": mode,
		"tun":  map[string]bool{"enable": tun},
	})
	if proxy { setProxyRegistry(true) }
}

// --- 状态监测 (核心图标逻辑修复) ---

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
	// 使用 /version 探测，并携带 Secret，这是解决 Stop 红色图标的关键
	req, _ := http.NewRequest("GET", ExternalController+"/version", nil)
	if Secret != "" { req.Header.Set("Authorization", "Bearer "+Secret) }
	
	resp, err := httpClient.Do(req)
	if err != nil {
		tunErrorCounter = 0
		return StateStop // API不通 -> 红色
	}
	resp.Body.Close()

	if isSystemInitializing { isSystemInitializing = false }
	onceSync.Do(func() { go syncToKernel() })

	configMu.RLock()
	wantTun := configData["tun"] == "true"
	wantProxy := configData["system_proxy"] == "true"
	configMu.RUnlock()

	if wantTun {
		hasTun := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			n := strings.ToLower(i.Name)
			if strings.Contains(n, "mihomo") || strings.Contains(n, "meta") || strings.Contains(n, "clash") {
				hasTun = true; break
			}
		}
		if hasTun {
			tunErrorCounter = 0
			return StateTun // 绿色
		}
		tunErrorCounter++
		if tunErrorCounter > 5 { return StateError } // 持续5秒无网卡 -> 黄色
		return StateTun
	}

	if wantProxy { return StateProxy } // 蓝色
	return StateDefault // 白色
}

// --- 蓝本原装：NotifyAddrChange 网卡监听 ---

func watchTunState() {
	var (
		modiphlpapi = syscall.NewLazyDLL("iphlpapi.dll")
		procNotify  = modiphlpapi.NewProc("NotifyAddrChange")
		handle      syscall.Handle
		overlapped  syscall.Overlapped
	)
	for {
		procNotify.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&overlapped)))
		time.Sleep(500 * time.Millisecond)

		hasTun := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			n := strings.ToLower(i.Name)
			if strings.Contains(n, "mihomo") || strings.Contains(n, "meta") || strings.Contains(n, "clash") {
				hasTun = true; break
			}
		}

		if mTun != nil && !isSystemInitializing {
			if hasTun { mTun.Check() } else { mTun.Uncheck() }
			saveIniConfig("tun", fmt.Sprint(hasTun))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --- 其他后台逻辑 ---

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		if !isProcessRunning("mihomo.exe") {
			onceSync = sync.Once{}
			exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T").Run()
			time.Sleep(300 * time.Millisecond)
			cmd := exec.Command(target, "-d", baseDir)
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB}
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

func isProcessRunning(name string) bool {
	h, _ := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
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

// --- UI 事件 ---

func onReady() {
	loadIniConfigAll()
	sniffYAML() // 启动即更新一次 API 和 Secret

	setProxyRegistry(getIniConfig("system_proxy") == "true")
	updateIconByState(StateStop)

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy") == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun") == "true")
	systray.AddSeparator()

	curMode := getIniConfig("mode")
	mRule := systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule")
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
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
		case <-mRule.ClickedCh:
			apiPATCH("/configs", map[string]string{"mode": "rule"}); saveIniConfig("mode", "rule")
			mRule.Check(); mGlobal.Uncheck(); mDirect.Uncheck()
		case <-mGlobal.ClickedCh:
			apiPATCH("/configs", map[string]string{"mode": "global"}); saveIniConfig("mode", "global")
			mRule.Uncheck(); mGlobal.Check(); mDirect.Uncheck()
		case <-mDirect.ClickedCh:
			apiPATCH("/configs", map[string]string{"mode": "direct"}); saveIniConfig("mode", "direct")
			mRule.Uncheck(); mGlobal.Uncheck(); mDirect.Check()
		case <-mTun.ClickedCh:
			isSystemInitializing = true
			next := !mTun.Checked()
			apiPATCH("/configs", map[string]interface{}{"tun": map[string]bool{"enable": next}})
			saveIniConfig("tun", fmt.Sprint(next))
			if next { mTun.Check() } else { mTun.Uncheck() }
			go func() { time.Sleep(3 * time.Second); isSystemInitializing = false }()
		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			setProxyRegistry(next)
			saveIniConfig("system_proxy", fmt.Sprint(next))
			if next { mProxy.Check() } else { mProxy.Uncheck() }
		case <-mAuto.ClickedCh:
			next := !mAuto.Checked()
			toggleAutoStart(next)
			if next { mAuto.Check() } else { mAuto.Uncheck() }
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRestart.ClickedCh:
			exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
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

// --- 系统接口 ---

func setProxyRegistry(enable bool) {
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
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
