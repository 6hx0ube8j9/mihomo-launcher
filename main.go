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
	APP_MUTEX  = "Global\\MihomoLauncher_Unique_Mutex"
	CONFIG_FILE = "mihomo-launcher.ini"
	REG_RUN    = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY  = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME   = "MihomoLauncher"

	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

var (
	// 成品变量
	ExternalController string
	Secret             string
	MixedPort          string

	isReallyExiting      bool
	isSystemInitializing bool
	hJob                 windows.Handle
	hMutex               windows.Handle
	httpClient           = &http.Client{Timeout: 1 * time.Second}
	exePath, _           = os.Executable()
	baseDir              = filepath.Dir(exePath)
	configData           = make(map[string]string)
	configMu             sync.RWMutex
	lastState            = -1
	mTun                 *systray.MenuItem
	onceSync             sync.Once
)

// --- 基础工具 ---

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

// --- 配置管理核心 (手术重点) ---

func loadIniConfigAll() {
	b, err := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	if err != nil {
		return
	}
	configMu.Lock()
	defer configMu.Unlock()

	configData = make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
}

func saveIniConfig(key, val string) {
	configMu.Lock()
	if key != "" {
		configData[key] = val
	}
	
	// 强制对齐 Key 名，防止碎片化
	order := []string{"mode", "tun", "system_proxy", "startup_enabled", "proxy_address", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range order {
		if v, ok := configData[k]; ok {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	content := buf.Bytes()
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), content, 0644)
}

func sniffAndSolidifyConfig() {
	configPath := filepath.Join(baseDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	
	// 预设死守值
	yamlController := "127.0.0.1:9090"
	yamlMixed := "7890"
	yamlSecret := ""

	if err == nil {
		content := string(data)
		// 增强版正则：排除注释行，匹配冒号后的值
		reController := regexp.MustCompile(`(?m)^external-controller:\s*['"]?([^'"]+?)['"]?\s*$`)
		reSecret     := regexp.MustCompile(`(?m)^secret:\s*['"]?([^'"]+?)['"]?\s*$`)
		reMixed      := regexp.MustCompile(`(?m)^mixed-port:\s*(\d+)`)
		rePort       := regexp.MustCompile(`(?m)^port:\s*(\d+)`)

		if m := reController.FindStringSubmatch(content); len(m) > 1 {
			yamlController = strings.TrimSpace(m[1])
		}
		if m := reSecret.FindStringSubmatch(content); len(m) > 1 {
			yamlSecret = strings.TrimSpace(m[1])
		}
		if m := reMixed.FindStringSubmatch(content); len(m) > 1 {
			yamlMixed = m[1]
		} else if m := rePort.FindStringSubmatch(content); len(m) > 1 {
			yamlMixed = m[1]
		}
	}

	// 格式化 Controller：必须是 http://ip:port
	if !strings.HasPrefix(yamlController, "http") {
		yamlController = "http://" + yamlController
	}
	// 防止出现 http://:9090 这种情况
	yamlController = strings.Replace(yamlController, "http://:", "http://127.0.0.1:", 1)
	ExternalController = strings.TrimSuffix(yamlController, "/")
	
	MixedPort = "127.0.0.1:" + yamlMixed
	Secret = yamlSecret

	// 固化回内存和文件
	saveIniConfig("proxy_address", MixedPort)
	saveIniConfig("external-controller", ExternalController)
	saveIniConfig("secret", Secret)
}

// --- 通讯模块 ---

func sendAPIRequest(method, path string, payload interface{}) {
	jsonBody, _ := json.Marshal(payload)
	url := ExternalController + path
	req, err := http.NewRequest(method, url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return
	}

	if Secret != "" {
		req.Header.Set("Authorization", "Bearer "+Secret)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// --- 状态监控 ---

func checkSystemState() int {
	// 使用 /version 探测存活
	req, _ := http.NewRequest("GET", ExternalController+"/version", nil)
	if Secret != "" {
		req.Header.Set("Authorization", "Bearer "+Secret)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return StateStop
	}
	resp.Body.Close()

	if isSystemInitializing {
		isSystemInitializing = false
	}

	onceSync.Do(func() {
		configMu.RLock()
		tun := configData["tun"] == "true"
		mode := configData["mode"]
		configMu.RUnlock()
		
		sendAPIRequest("PATCH", "/configs", map[string]interface{}{
			"mode": mode,
			"tun":  map[string]bool{"enable": tun},
		})
	})

	configMu.RLock()
	wantTun := configData["tun"] == "true"
	wantProxy := configData["system_proxy"] == "true"
	configMu.RUnlock()

	if wantTun {
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			n := strings.ToLower(i.Name)
			if strings.Contains(n, "mihomo") || strings.Contains(n, "meta") || strings.Contains(n, "clash") {
				return StateTun
			}
		}
		return StateError
	}
	if wantProxy {
		return StateProxy
	}
	return StateDefault
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		if !isProcessRunning("mihomo.exe") {
			onceSync = sync.Once{}
			cmd := exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T")
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			_ = cmd.Run()

			time.Sleep(300 * time.Millisecond)
			runCmd := exec.Command(target, "-d", baseDir)
			runCmd.SysProcAttr = &windows.SysProcAttr{
				CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
			}
			if err := runCmd.Start(); err == nil {
				if hJob != 0 {
					hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(runCmd.Process.Pid))
					_ = windows.AssignProcessToJobObject(hJob, hp)
					windows.CloseHandle(hp)
				}
				_ = runCmd.Wait()
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func monitorIconState() {
	for {
		if isReallyExiting { return }
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

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil { return false }
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil { return false }
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			return true
		}
		if err := windows.Process32Next(h, &pe); err != nil { break }
	}
	return false
}

// --- UI 事件 ---

func onReady() {
	loadIniConfigAll()
	updateIconByState(StateStop)

	mProxy := systray.AddMenuItemCheckbox("系统代理", "", configData["system_proxy"] == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", configData["tun"] == "true")
	systray.AddSeparator()

	mode := configData["mode"]
	mRule := systray.AddMenuItemCheckbox("规则模式", "", mode == "rule" || mode == "")
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", mode == "global")
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", mode == "direct")
	systray.AddSeparator()

	mAuto := systray.AddMenuItemCheckbox("开机启动", "", configData["startup_enabled"] == "true")
	mExit := systray.AddMenuItem("退出", "")

	for {
		select {
		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			saveIniConfig("system_proxy", fmt.Sprint(next))
			setProxyRegistry(next)
			if next { mProxy.Check() } else { mProxy.Uncheck() }

		case <-mTun.ClickedCh:
			next := !mTun.Checked()
			isSystemInitializing = true
			saveIniConfig("tun", fmt.Sprint(next))
			sendAPIRequest("PATCH", "/configs", map[string]interface{}{"tun": map[string]bool{"enable": next}})
			if next { mTun.Check() } else { mTun.Uncheck() }
			go func() { time.Sleep(3 * time.Second); isSystemInitializing = false }()

		case <-mRule.ClickedCh:
			saveIniConfig("mode", "rule")
			sendAPIRequest("PATCH", "/configs", map[string]string{"mode": "rule"})
			mRule.Check(); mGlobal.Uncheck(); mDirect.Uncheck()
		case <-mGlobal.ClickedCh:
			saveIniConfig("mode", "global")
			sendAPIRequest("PATCH", "/configs", map[string]string{"mode": "global"})
			mRule.Uncheck(); mGlobal.Check(); mDirect.Uncheck()
		case <-mDirect.ClickedCh:
			saveIniConfig("mode", "direct")
			sendAPIRequest("PATCH", "/configs", map[string]string{"mode": "direct"})
			mRule.Uncheck(); mGlobal.Uncheck(); mDirect.Check()

		case <-mAuto.ClickedCh:
			next := !mAuto.Checked()
			saveIniConfig("startup_enabled", fmt.Sprint(next))
			key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE)
			if next { _ = key.SetStringValue(APP_NAME, exePath) } else { _ = key.DeleteValue(APP_NAME) }
			key.Close()
			if next { mAuto.Check() } else { mAuto.Uncheck() }

		case <-mExit.ClickedCh:
			isReallyExiting = true
			setProxyRegistry(false)
			systray.Quit()
			return
		}
	}
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

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if b, err := iconFs.ReadFile("icons/" + files[state]); err == nil {
		systray.SetIcon(b)
	}
}

func main() {
	if !isAdmin() { runAsAdmin(); return }
	
	h, _ := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(APP_MUTEX))
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS { return }
	hMutex = h

	os.Chdir(baseDir)
	initJobObject()

	// 核心初始化顺序
	sniffAndSolidifyConfig() // 1. 先解析 YAML/生成 INI
	loadIniConfigAll()       // 2. 加载到内存

	go monitorKernelDaemon()
	go monitorIconState()

	systray.Run(onReady, func() {
		if hJob != 0 { windows.CloseHandle(hJob) }
		if hMutex != 0 { windows.CloseHandle(hMutex) }
	})
}
