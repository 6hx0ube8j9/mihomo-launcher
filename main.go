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

const (
	NIM_ADD = 0x0; NIM_MODIFY = 0x1; NIM_DELETE = 0x2
	NIF_MESSAGE = 0x1; NIF_ICON = 0x2; NIF_TIP = 0x4
	WM_USER_TRAY = 0x0400 + 1001
	WM_LBUTTONDBLCLK = 0x0203; WM_RBUTTONUP = 0x0205
	IMAGE_ICON = 1; LR_LOADFROMFILE = 0x10
)

var (
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

type NOTIFYICONDATA struct {
	CbSize, HWnd, UID, UFlags, UCallbackMessage uint32
	HIcon                                       windows.Handle
	SzTip                                       [128]uint16
	DwState, DwStateMask                        uint32
	SzInfo                                      [256]uint16
	UVersion, SzInfoTitle, DwInfoFlags          uint32
}

// --- 核心逻辑函数 ---

func setTunMode(enable bool) {
	state := "false"
	if enable { state = "true" }
	jsonBody := []byte(fmt.Sprintf(`{"tun": {"enable": %s}}`, state))
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBody))
	resp, err := httpClient.Do(req)
	if err == nil { resp.Body.Close() }
}

func setMihomoMode(mode string) {
	jsonBody := []byte(fmt.Sprintf(`{"mode": "%s"}`, mode))
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBody))
	resp, err := httpClient.Do(req)
	if err == nil { resp.Body.Close() }
}

func toggleAutoRun() {
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE|registry.QUERY_VALUE)
	defer key.Close()
	_, _, err := key.GetStringValue(APP_NAME)
	if err != nil { key.SetStringValue(APP_NAME, exePath) } else { key.DeleteValue(APP_NAME) }
}

func isAutoRunEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.QUERY_VALUE)
	if err != nil { return false }
	defer key.Close()
	_, _, err = key.GetStringValue(APP_NAME)
	return err == nil
}

func restartKernel() { exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T").Run() }
func openWebPanel() { windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, 1) }
func openConfigFolder() { windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(baseDir), nil, nil, 1) }

// --- 窗口与托盘处理 ---

func windowProc(hWnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_USER_TRAY:
		if lParam == WM_RBUTTONUP { showMenu(hWnd) }
		if lParam == WM_LBUTTONDBLCLK { openWebPanel() }
	case windows.WM_COMMAND:
		switch wParam {
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
			windows.PostQuitMessage(0)
			os.Exit(0)
		}
	}
	return windows.DefWindowProc(hWnd, msg, wParam, lParam)
}

func showMenu(hWnd windows.Handle) {
	user32 := windows.NewLazySystemDLL("user32.dll")
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
	if isAutoRunEnabled() { autoFlag = 0x00000008 } 
	addM(IDM_AUTORUN, "随系统启动", autoFlag)
	addM(IDM_RESTART, "重启内核进程", 0)
	addM(IDM_HIDE, "隐藏托盘图标", 0)
	user32.NewProc("AppendMenuW").Call(hMenu, 0x800, 0, 0)
	addM(IDM_EXIT, "彻底退出", 0)

	user32.NewProc("SetForegroundWindow").Call(uintptr(hWnd))
	var pos windows.Point
	windows.GetCursorPos(&pos)
	user32.NewProc("TrackPopupMenu").Call(hMenu, 0x102, uintptr(pos.X), uintptr(pos.Y), 0, uintptr(hWnd), 0)
}

// --- 底层辅助与初始化 ---

func runGuardian() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		curr := checkSystemState()
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

func preloadIcons() {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	user32 := windows.NewLazySystemDLL("user32.dll")
	for i, f := range files {
		path, _ := windows.UTF16PtrFromString(filepath.Join(baseDir, "icons", f))
		h, _, _ := user32.NewProc("LoadImageW").Call(0, uintptr(unsafe.Pointer(path)), IMAGE_ICON, 0, 0, LR_LOADFROMFILE)
		iconHandles[i] = windows.Handle(h)
	}
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
	for _, i := range ifaces {
		if strings.Contains(i.Name, name) { return true }
	}
	return false
}

func isProxyEnabledInRegistry() bool {
	key, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	defer key.Close()
	val, _, err := key.GetIntegerValue("ProxyEnable")
	return err == nil && val == 1
}

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = 0x2000
	windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(uintptr(h), 9, uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))))
	hJob = h
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

func isAdmin() bool {
	var token windows.Token
	windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	defer token.Close()
	return token.IsElevated()
}

func runAsAdmin() {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := syscall.UTF16PtrFromString(exePath)
	cwd, _ := syscall.UTF16PtrFromString(baseDir)
	windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_HIDE)
}

func setupWindow() windows.Handle {
	className, _ := windows.UTF16PtrFromString("MihomoTrayWnd")
	instance, _ := windows.GetModuleHandle(nil)
	wndClass := windows.WNDCLASSW{
		HInstance: instance,
		LpszClassName: className,
		LpfnWndProc: windows.NewCallback(windowProc),
	}
	windows.RegisterClassW(&wndClass)
	hwnd, _ := windows.CreateWindowEx(0, className, className, 0, 0, 0, 0, 0, 0, 0, instance, nil)
	return hwnd
}

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

func main() {
	if !isAdmin() { runAsAdmin(); os.Exit(0) }
	resp, err := httpClient.Get("http://127.0.0.1:" + WAKEUP_PORT + "/wakeup")
	if err == nil { resp.Body.Close(); os.Exit(0) }
	
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hMutex, _ := windows.CreateMutex(nil, false, mName)
	defer windows.CloseHandle(hMutex)
	
	initJobObject()
	loadIniConfigAll()
	preloadIcons()
	
	mainHwnd = setupWindow()
	globalNid.HWnd = mainHwnd
	globalNid.CbSize = uint32(unsafe.Sizeof(globalNid))
	globalNid.UFlags = NIF_ICON | NIF_MESSAGE | NIF_TIP
	globalNid.UCallbackMessage = WM_USER_TRAY
	copy(globalNid.SzTip[:], windows.StringToUTF16("Mihomo Manager"))
	
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
			if isHidden { isHidden = false; saveIniConfig("tray_hidden", "false"); addTrayIcon() }
			fmt.Fprint(w, "OK")
		})
		http.ListenAndServe("127.0.0.1:"+WAKEUP_PORT, mux)
	}()
	
	go runGuardian()
	
	if getIniConfig("tray_hidden") != "true" { addTrayIcon() } else { isHidden = true }
	
	var msg windows.Msg
	for {
		ret, _ := windows.GetMessage(&msg, 0, 0, 0)
		if ret == 0 { break }
		windows.TranslateMessage(&msg)
		windows.DispatchMessage(&msg)
	}
}
