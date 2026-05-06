package main

import (
	"bytes"
	"embed"
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

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	WAKEUP_PORT = "18579"
	APP_MUTEX   = "Global\\MihomoUltimateManager_V25_Official"
	API_URL     = "http://127.0.0.1:9090"
	CONFIG_FILE = "mihomo-launcher.ini"
	PROXY_ADDR  = "127.0.0.1:7890"

	// 优先级状态
	StateStop    = 0 // 1. 内核异常
	StateError   = 1 // 2. TUN 异常
	StateTun     = 2 // 3. TUN 正常
	StateProxy   = 3 // 4. 系统代理开启
	StateDefault = 4 // 5. 默认
)

// Windows API 常量
const (
	NIM_ADD         = 0x00000000
	NIM_MODIFY      = 0x00000001
	NIM_DELETE      = 0x00000002
	NIF_MESSAGE     = 0x00000001
	NIF_ICON        = 0x00000002
	NIF_TIP         = 0x00000004
	WM_USER_TRAY    = 0x0400 + 1001
	IMAGE_ICON      = 1
	LR_LOADFROMFILE = 0x00000010
)

var (
	isReallyExiting bool
	isHidden        bool
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	
	configData = make(map[string]string)
	configMu   sync.RWMutex
	
	globalNid   NOTIFYICONDATA
	iconHandles [5]windows.Handle
	lastState   = -1
)

type NOTIFYICONDATA struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
}

// --- 1. 核心判断逻辑 (优先级闭环) ---

func checkSystemState() int {
	// 优先级 1: Stop (内核没跑)
	_, err := httpClient.Get(API_URL)
	if err != nil && !isProcessRunning("mihomo.exe") {
		return StateStop
	}

	// 优先级 2 & 3: TUN 判定
	// 此处保留原版逻辑：API 连通后检查网卡
	if isInterfaceExisted("Mihomo") {
		return StateTun
	}
	// 如果配置中开启了 TUN 但没网卡，则为 Error (此处可根据你的 API 返回值细化)

	// 优先级 4: 系统代理
	if isProxyEnabledInRegistry() {
		return StateProxy
	}

	// 优先级 5: Default
	return StateDefault
}

// --- 2. 守护层 (Guardian) ---

func runGuardian() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }

		curr := checkSystemState()
		
		// 自动拉起逻辑
		if curr == StateStop {
			cmd := exec.Command(target, "-d", baseDir)
			cmd.Dir = baseDir
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			if err := cmd.Start(); err == nil && hJob != 0 {
				hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				_ = windows.AssignProcessToJobObject(hJob, hp)
				windows.CloseHandle(hp)
			}
		}

		// 图标刷新逻辑 (NIM_MODIFY)
		if !isHidden && curr != lastState {
			updateTrayIcon(curr)
			lastState = curr
		}

		time.Sleep(2 * time.Second)
	}
}

// --- 3. 通信层 (Messenger) ---

func startIpcServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
		if isHidden {
			isHidden = false
			saveIniConfig("tray_hidden", "false")
			addTrayIcon()
		}
		fmt.Fprint(w, "OK")
	})
	http.ListenAndServe("127.0.0.1:"+WAKEUP_PORT, mux)
}

func wakeupExisting() bool {
	resp, err := httpClient.Get("http://127.0.0.1:" + WAKEUP_PORT + "/wakeup")
	if err == nil {
		resp.Body.Close()
		return true
	}
	return false
}

// --- 4. UI 底层控制 (Windows Native) ---

func addTrayIcon() {
	shell32 := windows.NewLazySystemDLL("shell32.dll")
	shell32.NewProc("Shell_NotifyIconW").Call(NIM_ADD, uintptr(unsafe.Pointer(&globalNid)))
}

func updateTrayIcon(state int) {
	globalNid.HIcon = iconHandles[state]
	shell32 := windows.NewLazySystemDLL("shell32.dll")
	shell32.NewProc("Shell_NotifyIconW").Call(NIM_MODIFY, uintptr(unsafe.Pointer(&globalNid)))
}

func removeTrayIcon() {
	shell32 := windows.NewLazySystemDLL("shell32.dll")
	shell32.NewProc("Shell_NotifyIconW").Call(NIM_DELETE, uintptr(unsafe.Pointer(&globalNid)))
}

// --- 5. 主入口 (稳健启动) ---

func main() {
	// 权限与单实例
	if !isAdmin() { runAsAdmin(); os.Exit(0) }
	if wakeupExisting() { os.Exit(0) }

	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hMutex, _ := windows.CreateMutex(nil, false, mName)
	defer windows.CloseHandle(hMutex)

	os.Chdir(baseDir)
	initJobObject()

	// 预加载
	loadIniConfigAll()
	preloadIcons()
	
	// 初始化 NID (使用隐藏窗口句柄，此处简化为 0)
	globalNid.CbSize = uint32(unsafe.Sizeof(globalNid))
	globalNid.UID = 1001
	globalNid.UFlags = NIF_ICON | NIF_MESSAGE | NIF_TIP
	copy(globalNid.SzTip[:], windows.StringToUTF16("Mihomo Launcher"))

	// 启动分层
	go startIpcServer()
	go runGuardian()

	// 初始显示状态
	if getIniConfig("tray_hidden") != "true" {
		addTrayIcon()
	} else {
		isHidden = true
	}

	// 保持主进程不退出 (此处可后续添加 WndProc 消息循环)
	select {} 
}

// --- 原版核心逻辑保留 ---

func isProxyEnabledInRegistry() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil { return false }
	defer key.Close()
	val, _, err := key.GetDWordValue("ProxyEnable")
	return err == nil && val == 1
}

func setSystemProxy(enable bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.SET_VALUE)
	if err != nil { return }
	defer key.Close()
	if enable {
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", PROXY_ADDR)
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
	wininet := windows.NewLazySystemDLL("wininet.dll")
	wininet.NewProc("InternetSetOptionW").Call(0, 39, 0, 0)
	wininet.NewProc("InternetSetOptionW").Call(0, 37, 0, 0)
}

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(h), 9, uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))),
		)
		hJob = h
	}
}

func loadIniConfigAll() {
	configMu.Lock()
	defer configMu.Unlock()
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) == 2 { configData[parts[0]] = parts[1] }
	}
}

func saveIniConfig(key, val string) {
	configMu.Lock()
	defer configMu.Unlock()
	configData[key] = val
	var buf bytes.Buffer
	for k, v := range configData { buf.WriteString(fmt.Sprintf("%s=%s\n", k, v)) }
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func getIniConfig(key string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configData[key]
}

func preloadIcons() {
	// 顺序必须对应前面的 State 常量
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	user32 := windows.NewLazySystemDLL("user32.dll")
	loadIcon := user32.NewProc("LoadImageW")

	for i, f := range files {
		path, _ := windows.UTF16PtrFromString(filepath.Join(baseDir, "icons", f))
		h, _, _ := loadIcon.Call(0, uintptr(unsafe.Pointer(path)), IMAGE_ICON, 0, 0, LR_LOADFROMFILE)
		iconHandles[i] = windows.Handle(h)
	}
}

func isProcessRunning(name string) bool {
	snapshot, _ := windows.CreateToolhelp32Snapshot(0x00000002, 0)
	if snapshot == 0 { return false }
	defer windows.CloseHandle(snapshot)
	var proc windows.ProcessEntry32
	proc.Size = uint32(unsafe.Sizeof(proc))
	for windows.Process32Next(snapshot, &proc) == nil {
		if strings.EqualFold(windows.UTF16ToString(proc.ExeFile[:]), name) { return true }
	}
	return false
}

func isInterfaceExisted(name string) bool {
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(i.Name, name) { return true }
	}
	return false
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
	windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_HIDE)
}
