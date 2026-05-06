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

// --- 资源嵌入 ---

//go:embed icons/*.ico
var iconFs embed.FS

const (
	WAKEUP_PORT = "18579"
	APP_MUTEX   = "Global\\MihomoUltimateManager_V26"
	API_URL     = "http://127.0.0.1:9090"
	CONFIG_FILE = "mihomo-launcher.ini"
	REG_RUN     = `Software\Microsoft\Windows\CurrentVersion\Run`
	APP_NAME    = "MihomoLauncher"

	// 菜单 ID
	IDM_SHOW_UI  = 2000
	IDM_EXIT     = 2001
	IDM_HIDE     = 2002
	IDM_RESTART  = 2003
	IDM_FOLDER   = 2004
	IDM_AUTORUN  = 2005
	IDM_TUN_ON   = 2006
	IDM_TUN_OFF  = 2007
	IDM_MODE_R   = 2101
	IDM_MODE_G   = 2102
	IDM_MODE_D   = 2103

	StateStop = 0; StateError = 1; StateTun = 2; StateProxy = 3; StateDefault = 4
)

var (
	// 原生 DLL 调用声明
	user32           = windows.NewLazySystemDLL("user32.dll")
	shell32          = windows.NewLazySystemDLL("shell32.dll")
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	pRegisterClassW  = user32.NewProc("RegisterClassW")
	pCreateWindowExW = user32.NewProc("CreateWindowExW")
	pDefWindowProcW  = user32.NewProc("DefWindowProcW")
	pPostQuitMessage = user32.NewProc("PostQuitMessage")
	pGetMessageW     = user32.NewProc("GetMessageW")
	pTranslateMsg    = user32.NewProc("TranslateMessage")
	pDispatchMsg     = user32.NewProc("DispatchMessage")
	pGetCursorPos    = user32.NewProc("GetCursorPos")
	pTrackMenu       = user32.NewProc("TrackPopupMenu")
	pNotifyIconW     = shell32.NewProc("Shell_NotifyIconW")
	pLoadIconW       = user32.NewProc("LoadIconW")
	// 用于从内存创建图标
	pCreateIconFromResource = user32.NewProc("CreateIconFromResourceEx")

	isReallyExiting bool
	isHidden        bool
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	configData      = make(map[string]string)
	configMu        sync.RWMutex
	globalNid       NOTIFYICONDATA
	iconHandles     [5]windows.Handle
	lastState       = -1
	mainHwnd        windows.Handle
)

// NOTIFYICONDATA 结构体
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
	GuidItem         [16]byte
	HBalloonIcon     windows.Handle
}

type WNDCLASSW struct {
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
}

type POINT struct{ X, Y int32 }

// --- 窗口消息处理 ---

func windowProc(hWnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case 0x0400 + 1001: // WM_USER_TRAY
		if lParam == 0x0205 { // WM_RBUTTONUP
			showMenu(hWnd)
		} else if lParam == 0x0203 { // WM_LBUTTONDBLCLK
			openWebPanel()
		}
	case 0x0111: // WM_COMMAND
		handleMenuCommand(wParam)
	case 0x0002: // WM_DESTROY
		pPostQuitMessage.Call(0)
	}
	ret, _, _ := pDefWindowProcW.Call(uintptr(hWnd), uintptr(msg), wParam, lParam)
	return ret
}

func handleMenuCommand(id uintptr) {
	switch id {
	case IDM_SHOW_UI: openWebPanel()
	case IDM_MODE_R: setMihomoMode("rule")
	case IDM_MODE_G: setMihomoMode("global")
	case IDM_MODE_D: setMihomoMode("direct")
	case IDM_TUN_ON: setTunMode(true)
	case IDM_TUN_OFF: setTunMode(false)
	case IDM_RESTART: restartKernel()
	case IDM_FOLDER: openConfigFolder()
	case IDM_AUTORUN: toggleAutoRun()
	case IDM_HIDE:
		isHidden = true
		saveIniConfig("tray_hidden", "true")
		removeTrayIcon()
	case IDM_EXIT:
		isReallyExiting = true
		removeTrayIcon()
		os.Exit(0)
	}
}

func showMenu(hWnd windows.Handle) {
	hMenu, _, _ := user32.NewProc("CreatePopupMenu").Call()
	addM := func(id uintptr, text string, flags uint32) {
		t, _ := windows.UTF16PtrFromString(text)
		user32.NewProc("AppendMenuW").Call(hMenu, uintptr(flags), id, uintptr(unsafe.Pointer(t)))
	}

	addM(IDM_SHOW_UI, "进入 Web 面板", 0)
	addM(IDM_FOLDER, "打开配置目录", 0)
	user32.NewProc("AppendMenuW").Call(hMenu, 0x800, 0, 0)
	addM(IDM_MODE_R, "规则模式 (Rule)", 0)
	addM(IDM_MODE_G, "全局模式 (Global)", 0)
	addM(IDM_MODE_D, "直连模式 (Direct)", 0)
	user32.NewProc("AppendMenuW").Call(hMenu, 0x800, 0, 0)
	addM(IDM_TUN_ON, "开启 TUN 模式", 0)
	addM(IDM_TUN_OFF, "关闭 TUN 模式", 0)
	user32.NewProc("AppendMenuW").Call(hMenu, 0x800, 0, 0)

	autoFlag := uint32(0)
	if isAutoRunEnabled() { autoFlag = 0x0008 }
	addM(IDM_AUTORUN, "随系统启动", autoFlag)
	addM(IDM_RESTART, "重启内核进程", 0)
	addM(IDM_HIDE, "隐藏托盘图标", 0)
	user32.NewProc("AppendMenuW").Call(hMenu, 0x800, 0, 0)
	addM(IDM_EXIT, "彻底退出", 0)

	user32.NewProc("SetForegroundWindow").Call(uintptr(hWnd))
	var pos POINT
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pos)))
	pTrackMenu.Call(hMenu, 0x102, uintptr(pos.X), uintptr(pos.Y), 0, uintptr(hWnd), 0)
}

// --- 核心守护与状态 ---

func runGuardian() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		curr := checkSystemState()
		if curr == StateStop {
			cmd := exec.Command(target, "-d", baseDir)
			cmd.Dir = baseDir
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
			if err := cmd.Start(); err == nil && hJob != 0 {
				hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				_ = windows.AssignProcessToJobObject(hJob, hp)
				windows.CloseHandle(hp)
			}
		}
		if !isHidden && curr != lastState {
			updateTrayIcon(curr)
			lastState = curr
		}
		time.Sleep(2 * time.Second)
	}
}

func checkSystemState() int {
	_, err := httpClient.Get(API_URL)
	if err != nil && !isProcessRunning("mihomo.exe") { return StateStop }
	if isInterfaceExisted("Mihomo") { return StateTun }
	if isProxyEnabledInRegistry() { return StateProxy }
	return StateDefault
}

func setTunMode(enable bool) {
	state := "false"; if enable { state = "true" }
	jsonBody := []byte(fmt.Sprintf(`{"tun": {"enable": %s}}`, state))
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBody))
	resp, err := httpClient.Do(req); if err == nil { resp.Body.Close() }
}

func setMihomoMode(mode string) {
	jsonBody := []byte(fmt.Sprintf(`{"mode": "%s"}`, mode))
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBody))
	resp, err := httpClient.Do(req); if err == nil { resp.Body.Close() }
}

// --- 底层辅助 (内嵌加载优化) ---

func preloadIcons() {
	// 对应 StateStop=0, StateError=1, StateTun=2, StateProxy=3, StateDefault=4
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	
	for i, f := range files {
		// 从 embed.FS 读取二进制数据
		data, err := iconFs.ReadFile("icons/" + f)
		
		var h uintptr
		if err == nil && len(data) > 0 {
			// 将内存中的 .ico 数据转换为 Windows 句柄
			// 参数说明: 数据指针, 数据长度, 资源类型(1为图标), 版本(0x00030000), 宽, 高, 标志
			h, _, _ = pCreateIconFromResource.Call(
				uintptr(unsafe.Pointer(&data[0])),
				uintptr(len(data)),
				1,          // TRUE (Icon)
				0x00030000, // Version 3.0
				0,          // 使用图标默认宽度
				0,          // 使用图标默认高度
				0,          // LR_DEFAULTCOLOR
			)
		}

		// --- 保底逻辑 ---
		// 如果 embed 读取失败或 CreateIconFromResource 转换失败 (h == 0)
		if h == 0 {
			// 加载系统内置图标 IDI_APPLICATION (32512) 确保程序不崩溃
			h, _, _ = pLoadIconW.Call(0, uintptr(32512))
		}
		
		iconHandles[i] = windows.Handle(h)
	}
}

func setupWindow() windows.Handle {
	className, _ := windows.UTF16PtrFromString("MihomoTrayWnd")
	hInstance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)

	wc := WNDCLASSW{
		HInstance:     windows.Handle(hInstance),
		LpszClassName: className,
		LpfnWndProc:   windows.NewCallback(windowProc),
	}
	pRegisterClassW.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, 0, hInstance, 0)
	return windows.Handle(hwnd)
}

func isProcessRunning(name string) bool {
	snapshot, _ := windows.CreateToolhelp32Snapshot(0x00000002, 0)
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
	for _, i := range ifaces { if strings.Contains(i.Name, name) { return true } }
	return false
}

func isProxyEnabledInRegistry() bool {
	key, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	defer key.Close()
	val, _, err := key.GetIntegerValue("ProxyEnable")
	return err == nil && val == 1
}

func toggleAutoRun() {
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE|registry.QUERY_VALUE)
	defer key.Close()
	_, _, err := key.GetStringValue(APP_NAME)
	if err != nil { key.SetStringValue(APP_NAME, exePath) } else { key.DeleteValue(APP_NAME) }
}

func isAutoRunEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.QUERY_VALUE)
	if err != nil { return false }; defer key.Close()
	_, _, err = key.GetStringValue(APP_NAME)
	return err == nil
}

func saveIniConfig(key, val string) {
	configMu.Lock(); defer configMu.Unlock()
	configData[key] = val
	var buf bytes.Buffer
	for k, v := range configData { buf.WriteString(fmt.Sprintf("%s=%s\n", k, v)) }
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func getIniConfig(key string) string {
	configMu.RLock(); defer configMu.RUnlock()
	return configData[key]
}

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) == 2 { configData[parts[0]] = parts[1] }
	}
}

func addTrayIcon() {
	globalNid.UFlags = 0x7 // NIF_MESSAGE | NIF_ICON | NIF_TIP
	pNotifyIconW.Call(0, uintptr(unsafe.Pointer(&globalNid)))
}

func updateTrayIcon(state int) {
	globalNid.HIcon = iconHandles[state]
	pNotifyIconW.Call(1, uintptr(unsafe.Pointer(&globalNid)))
}

func removeTrayIcon() {
	pNotifyIconW.Call(2, uintptr(unsafe.Pointer(&globalNid)))
}

func restartKernel() { exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T").Run() }
func openWebPanel() { windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, 1) }
func openConfigFolder() { windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(baseDir), nil, nil, 1) }

func main() {
	// 1. 管理员权限自提（增加参数判断，防止极少数环境下的死循环）
	isRestart := false
	for _, arg := range os.Args {
		if arg == "--restarted" { isRestart = true; break }
	}

	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err == nil {
		if !token.IsElevated() && !isRestart {
			verb, _ := syscall.UTF16PtrFromString("runas")
			exe, _ := syscall.UTF16PtrFromString(exePath)
			arg, _ := syscall.UTF16PtrFromString("--restarted")
			windows.ShellExecute(0, verb, exe, arg, nil, 0)
			os.Exit(0)
		}
	}

	// 2. 健壮的单实例检测与唤醒机制
	// 先尝试监听端口，如果失败则说明已有实例
	ln, err := net.Listen("tcp", "127.0.0.1:"+WAKEUP_PORT)
	if err != nil {
		// 发送唤醒信号给已存在的实例
		httpClient.Get("http://127.0.0.1:" + WAKEUP_PORT + "/wakeup")
		os.Exit(0) // 退出当前新实例
	}
	// 成功抢占端口，ln 将在后面的协程中使用，不要在这里 Close

	// 3. 进程绑定到 Job Object（实现 Launcher 退出时内核同步退出）
	hJob, _ = windows.CreateJobObject(nil, nil)
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	kernel32.NewProc("SetInformationJobObject").Call(uintptr(hJob), 9, uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))))

	// 4. 加载配置与图标
	loadIniConfigAll()
	preloadIcons()

	// 5. 创建隐藏的消息窗口
	mainHwnd = setupWindow()
	if mainHwnd == 0 {
		os.Exit(1) // 窗口创建失败则退出
	}

	// 初始化托盘数据结构
	globalNid.HWnd = mainHwnd
	globalNid.CbSize = uint32(unsafe.Sizeof(globalNid))
	globalNid.UID = 1001
	globalNid.UCallbackMessage = 0x0400 + 1001 // WM_USER_TRAY
	copy(globalNid.SzTip[:], windows.StringToUTF16("Mihomo Launcher"))

	// 6. 启动唤醒 HTTP 服务 (复用上面成功的监听器)
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
			if isHidden {
				isHidden = false
				saveIniConfig("tray_hidden", "false")
				addTrayIcon()
			}
			// 激活并置顶 Web 面板 (可选)
			openWebPanel()
		})
		http.Serve(ln, mux)
	}()

	// 7. 启动内核守护协程
	go runGuardian()

	// 8. 根据配置决定是否显示托盘
	if getIniConfig("tray_hidden") != "true" {
		addTrayIcon()
	} else {
		isHidden = true
	}

	// 9. 核心消息循环 (保持进程存活的关键)
	var msg struct {
		HWnd    windows.Handle
		Message uint32
		WParam  uintptr
		LParam  uintptr
		Time    uint32
		Pt      POINT
	}
	for {
		// GetMessageW 会在这里阻塞，直到接收到系统消息
		ret, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		pTranslateMsg.Call(uintptr(unsafe.Pointer(&msg)))
		pDispatchMsg.Call(uintptr(unsafe.Pointer(&msg)))
	}
}
