package main

import (
	"bytes"
	"embed"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed icons/*.ico
var iconFs embed.FS

// 菜单 ID 定义
const (
	ID_OPEN_UI      = 2001
	ID_OPEN_DIR     = 2002
	ID_MODE_RULE    = 2003
	ID_MODE_GLOBAL  = 2004
	ID_MODE_DIRECT  = 2005
	ID_SYS_PROXY    = 2006
	ID_TUN_MODE     = 2007
	ID_AUTO_START   = 2008
	ID_SVC_INSTALL  = 2009
	ID_HIDE_TRAY    = 2011
	ID_EXIT         = 2012
)

const (
	WM_USER_TRAY = 0x0400 + 2026
	WAKEUP_PORT  = "18579"
	APP_MUTEX    = "Global\\MihomoUltimate_V32_Stable"
)

type NOTIFYICONDATA struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
}

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	shell32          = windows.NewLazySystemDLL("shell32.dll")
	pNotifyIcon      = shell32.NewProc("Shell_NotifyIconW")
	pPostQuitMessage = user32.NewProc("PostQuitMessage")

	mainHwnd   windows.Handle
	nid        NOTIFYICONDATA
	isHidden   bool
	isExiting  bool
	baseDir, _ = filepath.Abs(filepath.Dir(os.Args[0]))
	httpClient = &http.Client{Timeout: 2 * time.Second}

	// 状态记录
	proxyEnabled = false
	tunEnabled   = false
	autoStart    = false
)

// --- 系统工具 ---

func setSystemProxy(enable bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.SET_VALUE)
	if err != nil { return }
	defer key.Close()
	if enable {
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", "127.0.0.1:7890")
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
	wininet := windows.NewLazySystemDLL("wininet.dll")
	wininet.NewProc("InternetSetOptionW").Call(0, 39, 0, 0)
	wininet.NewProc("InternetSetOptionW").Call(0, 37, 0, 0)
	proxyEnabled = enable
}

func setAutoStart(enable bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil { return }
	defer key.Close()
	exe, _ := os.Executable()
	if enable {
		_ = key.SetStringValue("MihomoLauncher", exe)
	} else {
		_ = key.DeleteValue("MihomoLauncher")
	}
	autoStart = enable
}

func getIconHandle(name string) windows.Handle {
	data, err := iconFs.ReadFile("icons/" + name)
	if err != nil { return 0 }
	h, _, _ := user32.NewProc("CreateIconFromResourceEx").Call(uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 1, 0x00030000, 0, 0, 0)
	return windows.Handle(h)
}

func sendPatch(jsonStr string) {
	go func() {
		req, _ := http.NewRequest("PATCH", "http://127.0.0.1:9090/configs", bytes.NewBuffer([]byte(jsonStr)))
		if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
	}()
}

// --- 菜单与指令 ---

func handleCommand(id uintptr) {
	switch id {
	case ID_OPEN_UI:
		exec.Command("cmd", "/c", "start", "http://127.0.0.1:9090/ui").Run()
	case ID_OPEN_DIR:
		exec.Command("explorer", baseDir).Run()
	case ID_MODE_RULE:
		sendPatch(`{"mode":"rule"}`)
	case ID_MODE_GLOBAL:
		sendPatch(`{"mode":"global"}`)
	case ID_MODE_DIRECT:
		sendPatch(`{"mode":"direct"}`)
	case ID_SYS_PROXY:
		setSystemProxy(!proxyEnabled)
	case ID_TUN_MODE:
		tunEnabled = !tunEnabled
		state := "false"
		if tunEnabled { state = "true" }
		sendPatch(`{"tun":{"enable":` + state + `}}`)
	case ID_AUTO_START:
		setAutoStart(!autoStart)
	case ID_SVC_INSTALL:
		svcPath := filepath.Join(baseDir, "mihomo-service.exe")
		exec.Command("powershell", "-Command", "Start-Process '"+svcPath+"' -ArgumentList 'install' -Verb RunAs").Run()
	case ID_HIDE_TRAY:
		pNotifyIcon.Call(2, uintptr(unsafe.Pointer(&nid))) // NIM_DELETE
		isHidden = true
	case ID_EXIT:
		isExiting = true
		setSystemProxy(false)
		pNotifyIcon.Call(2, uintptr(unsafe.Pointer(&nid)))
		exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
		os.Exit(0)
	}
}

func showContextMenu(hWnd windows.Handle) {
	hMenu, _, _ := user32.NewProc("CreatePopupMenu").Call()
	
	add := func(id uintptr, text string, checked bool) {
		flags := uintptr(0)
		if checked { flags = 0x00000008 } // MF_CHECKED
		p, _ := windows.UTF16PtrFromString(text)
		user32.NewProc("AppendMenuW").Call(hMenu, flags, id, uintptr(unsafe.Pointer(p)))
	}
	sep := func() { user32.NewProc("AppendMenuW").Call(hMenu, 0x800, 0, 0) }

	add(ID_OPEN_UI, "打开控制面板", false)
	add(ID_OPEN_DIR, "打开程序目录", false)
	sep()
	add(ID_MODE_RULE, "规则模式 (Rule)", false)
	add(ID_MODE_GLOBAL, "全局模式 (Global)", false)
	add(ID_MODE_DIRECT, "直连模式 (Direct)", false)
	sep()
	add(ID_SYS_PROXY, "系统代理", proxyEnabled)
	add(ID_TUN_MODE, "TUN 模式", tunEnabled)
	add(ID_AUTO_START, "开机自启动", autoStart)
	sep()
	add(ID_SVC_INSTALL, "安装系统服务", false)
	sep()
	add(ID_HIDE_TRAY, "隐藏托盘图标", false)
	add(ID_EXIT, "彻底退出", false)

	var pos struct{ X, Y int32 }
	user32.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&pos)))
	user32.NewProc("SetForegroundWindow").Call(uintptr(hWnd))
	user32.NewProc("TrackPopupMenu").Call(hMenu, 0x100, uintptr(pos.X), uintptr(pos.Y), 0, uintptr(hWnd), 0)
}

func windowProc(hWnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_USER_TRAY:
		if lParam == 0x0205 { showContextMenu(hWnd) }
		if lParam == 0x0201 {
			pNotifyIcon.Call(0, uintptr(unsafe.Pointer(&nid))) // NIM_ADD
			isHidden = false
		}
	case 0x0111: handleCommand(wParam)
	case 0x0002: pPostQuitMessage.Call(0)
	}
	ret, _, _ := user32.NewProc("DefWindowProcW").Call(uintptr(hWnd), uintptr(msg), wParam, lParam)
	return ret
}

func main() {
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hMutex, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if hMutex != 0 { windows.CloseHandle(hMutex) }
		httpClient.Get("http://127.0.0.1:" + WAKEUP_PORT + "/wakeup")
		os.Exit(0)
	}

	// 初始检查
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err == nil {
		v, _, _ := k.GetIntegerValue("ProxyEnable")
		proxyEnabled = (v == 1)
		k.Close()
	}

	clsName, _ := windows.UTF16PtrFromString("MihomoV32Cls")
	wc := struct { Style, LpfnWndProc, CbClsExtra, CbWndExtra, HInstance, HIcon, HCursor, HbrBackground, LpszMenuName, LpszClassName uintptr }{
		LpfnWndProc: windows.NewCallback(windowProc),
		LpszClassName: uintptr(unsafe.Pointer(clsName)),
	}
	user32.NewProc("RegisterClassW").Call(uintptr(unsafe.Pointer(&wc)))
	hwnd, _, _ := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(clsName)), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	mainHwnd = windows.Handle(hwnd)

	nid = NOTIFYICONDATA{
		CbSize: uint32(unsafe.Sizeof(nid)),
		HWnd: mainHwnd, UID: 1, UFlags: 7, UCallbackMessage: WM_USER_TRAY,
		HIcon: getIconHandle("default.ico"),
	}
	copy(nid.SzTip[:], windows.StringToUTF16("Mihomo Launcher"))
	pNotifyIcon.Call(0, uintptr(unsafe.Pointer(&nid)))

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
			pNotifyIcon.Call(0, uintptr(unsafe.Pointer(&nid)))
			isHidden = false
		})
		http.ListenAndServe("127.0.0.1:"+WAKEUP_PORT, mux)
	}()

	go func() {
		for !isExiting {
			_, err := httpClient.Get("http://127.0.0.1:9090")
			if err != nil {
				target := filepath.Join(baseDir, "mihomo.exe")
				cmd := exec.Command(target, "-d", baseDir)
				cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: 0x08000000}
				cmd.Start()
			}
			time.Sleep(10 * time.Second)
		}
	}()

	var msg struct { HWnd windows.Handle; Message uint32; WParam, LParam uintptr; Time uint32; Pt struct{ X, Y int32 } }
	for {
		ret, _, _ := user32.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 { break }
		user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
		user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
	}
	
	// 保持句柄引用防止提前被回收
	_ = hMutex
}
