package main

import (
	"bytes"
	"embed"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	WM_USER_TRAY = 0x0400 + 2026
	WAKEUP_PORT  = "18579"
	APP_MUTEX    = "Global\\MihomoUltimate_V29_Final"
)

// Win32 结构体定义
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
	user32       = windows.NewLazySystemDLL("user32.dll")
	shell32      = windows.NewLazySystemDLL("shell32.dll")
	pNotifyIcon  = shell32.NewProc("Shell_NotifyIconW")
	
	mainHwnd     windows.Handle
	nid          NOTIFYICONDATA
	isHidden     bool
	isExiting    bool
	baseDir, _   = filepath.Abs(filepath.Dir(os.Args[0]))
	httpClient   = &http.Client{Timeout: 2 * time.Second}
)

// --- 1. 原生图标加载 (解决黑块/显示失败) ---
func getIconHandle(name string) windows.Handle {
	data, err := iconFs.ReadFile("icons/" + name)
	if err != nil {
		return 0
	}
	h, _, _ := user32.NewProc("CreateIconFromResourceEx").Call(
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		1,          // TRUE 为图标
		0x00030000, // 默认版本
		0, 0, 0,    // 自动大小
	)
	return windows.Handle(h)
}

// --- 2. 托盘管理逻辑 ---
func refreshTray(action uintptr) { // 0: ADD, 1: MODIFY, 2: DELETE
	pNotifyIcon.Call(action, uintptr(unsafe.Pointer(&nid)))
}

func showTray() {
	if isHidden || nid.HIcon == 0 {
		nid.HIcon = getIconHandle("default.ico")
		refreshTray(0) // NIM_ADD
		isHidden = false
	}
}

func hideTray() {
	refreshTray(2) // NIM_DELETE (彻底移除，不留黑块)
	isHidden = true
}

// --- 3. 菜单与指令处理 ---
func handleCommand(id uintptr) {
	switch id {
	case 2001: // 打开面板
		exec.Command("cmd", "/c", "start", "http://127.0.0.1:9090/ui").Run()
	case 2002: // 切换模式 (示例)
		go http.Post("http://127.0.0.1:9090/configs", "application/json", bytes.NewBuffer([]byte(`{"mode":"rule"}`)))
	case 2005: // 隐藏
		hideTray()
	case 2006: // 彻底退出
		isExiting = true
		hideTray()
		exec.Command("taskkill", "/F", "/IM", "mihomo.exe").Run()
		os.Exit(0)
	}
}

// --- 4. 窗口回调 (核心消息处理) ---
func windowProc(hWnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_USER_TRAY:
		if lParam == 0x0205 { // WM_RBUTTONUP (右键菜单)
			showContextMenu(hWnd)
		} else if lParam == 0x0201 { // WM_LBUTTONDOWN (左键点击可唤醒/打开)
			showTray()
		}
	case 0x0111: // WM_COMMAND
		handleCommand(wParam)
	case 0x0002: // WM_DESTROY
		windows.PostQuitMessage(0)
	}
	ret, _, _ := user32.NewProc("DefWindowProcW").Call(uintptr(hWnd), uintptr(msg), wParam, lParam)
	return ret
}

func showContextMenu(hWnd windows.Handle) {
	hMenu, _, _ := user32.NewProc("CreatePopupMenu").Call()
	addMenu := func(id uintptr, text string) {
		p, _ := windows.UTF16PtrFromString(text)
		user32.NewProc("AppendMenuW").Call(hMenu, 0, id, uintptr(unsafe.Pointer(p)))
	}

	addMenu(2001, "打开控制面板")
	user32.NewProc("AppendMenuW").Call(hMenu, 0x800, 0, 0) // 分隔线
	addMenu(2002, "模式: 规则 (Rule)")
	user32.NewProc("AppendMenuW").Call(hMenu, 0x800, 0, 0)
	addMenu(2005, "隐藏托盘图标")
	addMenu(2006, "彻底退出程序")

	var pos struct{ X, Y int32 }
	user32.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&pos)))
	user32.NewProc("SetForegroundWindow").Call(uintptr(hWnd))
	user32.NewProc("TrackPopupMenu").Call(hMenu, 0x100, uintptr(pos.X), uintptr(pos.Y), 0, uintptr(hWnd), 0)
}

// --- 5. 主程序 ---
func main() {
	// A. 互斥锁检测：杜绝多开
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hMutex, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		// 第二次启动：尝试唤醒第一个实例
		httpClient.Get("http://127.0.0.1:" + WAKEUP_PORT + "/wakeup")
		windows.CloseHandle(hMutex)
		os.Exit(0)
	}

	// B. 注册并创建隐藏窗口 (作为托盘宿主)
	clsName, _ := windows.UTF16PtrFromString("MihomoLogClass")
	wc := struct {
		Style, LpfnWndProc, CbClsExtra, CbWndExtra, HInstance, HIcon, HCursor, HbrBackground, LpszMenuName, LpszClassName uintptr
	}{
		LpfnWndProc:   windows.NewCallback(windowProc),
		LpszClassName: uintptr(unsafe.Pointer(clsName)),
	}
	user32.NewProc("RegisterClassW").Call(uintptr(unsafe.Pointer(&wc)))
	hwnd, _, _ := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(clsName)), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	mainHwnd = windows.Handle(hwnd)

	// C. 初始化托盘结构体
	nid = NOTIFYICONDATA{
		CbSize:           uint32(unsafe.Sizeof(nid)),
		HWnd:             mainHwnd,
		UID:              1,
		UFlags:           7, // NIF_MESSAGE | NIF_ICON | NIF_TIP
		UCallbackMessage: WM_USER_TRAY,
		HIcon:            getIconHandle("default.ico"),
	}
	copy(nid.SzTip[:], windows.StringToUTF16("Mihomo Launcher (已就绪)"))
	showTray()

	// D. 开启唤醒监听 (用于响应双击 EXE)
	go func() {
		server := http.NewServeMux()
		server.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
			showTray() // 收到信号，强制显示图标
		})
		http.ListenAndServe("127.0.0.1:"+WAKEUP_PORT, server)
	}()

	// E. 守护进程逻辑 (每10秒检查内核)
	go func() {
		for !isExiting {
			_, err := httpClient.Get("http://127.0.0.1:9090")
			if err != nil {
				// 如果内核没跑，静默启动它
				target := filepath.Join(baseDir, "mihomo.exe")
				cmd := exec.Command(target, "-d", baseDir)
				cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
				cmd.Start()
			}
			time.Sleep(10 * time.Second)
		}
	}()

	// F. 消息循环 (Windows 程序的心跳)
	var msg struct {
		HWnd windows.Handle; Message uint32; WParam, LParam uintptr; Time uint32; Pt struct{ X, Y int32 }
	}
	for {
		ret, _, _ := user32.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 { break }
		user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
		user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
	}
}
