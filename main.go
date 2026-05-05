package main

import (
	"bufio"
	"bytes"
	"embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	API_URL    = "http://127.0.0.1:9090"
	PROXY_ADDR = "127.0.0.1:7890"
	TUN_NAME   = "Mihomo"
	IPC_PORT   = "127.0.0.1:54321" // 信号唤醒端口
)

type Config struct {
	AutoStart   bool
	ServiceMode bool
	TunEnabled  bool
	TrayHidden  bool
}

var (
	conf       Config
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	coreExe    = filepath.Join(baseDir, "bin", "mihomo.exe")
	iniPath    = filepath.Join(baseDir, "mihomo-launcher.ini")
	hJob       windows.Handle
	isUserOpen = true // 默认为手动双击启动
)

// --- 核心修复：单实例与信号唤醒 ---

func handleIPC() {
	ln, err := net.Listen("tcp", IPC_PORT)
	if err != nil {
		// 已有实例在运行，发送唤醒信号并退出
		conn, err := net.Dial("tcp", IPC_PORT)
		if err == nil {
			conn.Write([]byte("WAKE_UP"))
			conn.Close()
		}
		os.Exit(0)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil { continue }
		buf := make([]byte, 10)
		n, _ := conn.Read(buf)
		if string(buf[:n]) == "WAKE_UP" {
			// 收到唤醒信号：强制显示图标并重启 UI
			conf.TrayHidden = false
			saveConfig()
			restartWithUI()
		}
		conn.Close()
	}
}

func restartWithUI() {
	verb, _ := syscall.UTF16PtrFromString("open")
	exe, _ := syscall.UTF16PtrFromString(exePath)
	windows.ShellExecute(0, verb, exe, nil, nil, windows.SW_SHOWNORMAL)
	os.Exit(0)
}

// --- 核心修复：内核守护逻辑 (绝对路径锁定) ---

func engineKeeper() {
	for {
		if !conf.ServiceMode {
			checkAndStartCore()
		}
		syncTunState()
		time.Sleep(3 * time.Second)
	}
}

func checkAndStartCore() {
	// 通过 API 探测内核是否响应，比 tasklist 准确
	resp, err := http.Get(API_URL + "/version")
	if err == nil {
		resp.Body.Close()
		return
	}

	// 拉起内核，强制锁定工作目录为 Launcher 所在目录
	cmd := exec.Command(coreExe, "-d", baseDir)
	cmd.Dir = baseDir
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
	}

	if err := cmd.Start(); err == nil && hJob != 0 {
		hProc, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		windows.AssignProcessToJobObject(hJob, hProc)
		windows.CloseHandle(hProc)
	}
}

// --- 核心修复：BAT 路径处理 ---

func runBat(action string) {
	bat := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	batDir := filepath.Dir(bat)
	// 使用 cmd /c start 并锁定 /d 运行目录
	args := fmt.Sprintf("/c start /d \"%s\" %s %s", batDir, filepath.Base(bat), action)
	
	v, _ := syscall.UTF16PtrFromString("cmd")
	a, _ := syscall.UTF16PtrFromString(args)
	d, _ := syscall.UTF16PtrFromString(batDir)
	windows.ShellExecute(0, nil, v, a, d, windows.SW_HIDE)
}

// --- 托盘 UI 逻辑 ---

func onReady() {
	systray.SetIcon(getIcon("default.ico"))
	
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", isProxyEnabled())
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()

	mSvc := systray.AddMenuItem("启动设置", "")
	mAuto := mSvc.AddSubMenuItemCheckbox("开机自动启动", "", conf.AutoStart)
	mInst := mSvc.AddSubMenuItem("安装后台服务", "")
	mUninst := mSvc.AddSubMenuItem("卸载后台服务", "")
	mBat := mSvc.AddSubMenuItem("管理服务 (BAT)", "")
	mRes := mSvc.AddSubMenuItem("重启内核", "")
	mExit := mSvc.AddSubMenuItem("彻底退出程序", "")

	systray.AddSeparator()
	mDir := systray.AddMenuItem("打开程序目录", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "")

	// 动态更新状态
	go func() {
		for {
			if conf.ServiceMode { mI, mU := mInst, mUninst; mI.Disable(); mU.Enable() } else { mInst.Enable(); mUninst.Disable() }
			refreshUI(mProxy, mTun)
			time.Sleep(2 * time.Second)
		}
	}()

	for {
		select {
		case <-mWeb.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh: setProxy(!isProxyEnabled())
		case <-mTun.ClickedCh:
			conf.TunEnabled = !mTun.Checked()
			saveConfig()
		case <-mAuto.ClickedCh:
			conf.AutoStart = !mAuto.Checked()
			updateAutoStart(conf.AutoStart)
			saveConfig()
		case <-mInst.ClickedCh:
			runBat("install")
			conf.ServiceMode = true
			saveConfig()
		case <-mUninst.ClickedCh:
			runBat("uninstall")
			conf.ServiceMode = false
			saveConfig()
		case <-mRes.ClickedCh:
			if conf.ServiceMode { runBat("restart") } else { killCore() }
		case <-mExit.ClickedCh:
			setProxy(false)
			if conf.ServiceMode { runBat("stop") } else { killCore() }
			os.Exit(0)
		case <-mDir.ClickedCh: exec.Command("explorer", baseDir).Run()
		case <-mBat.ClickedCh: runBat("")
		case <-mHide.ClickedCh:
			conf.TrayHidden = true
			saveConfig()
			systray.Quit() // 仅退出 UI 线程，主线程 select{} 会维持 IPC 和内核守护
		}
	}
}

// --- 底层辅助 ---

func refreshUI(mP, mT *systray.MenuItem) {
	resp, err := http.Get(API_URL + "/configs")
	if err != nil {
		systray.SetIcon(getIcon("stop.ico"))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	data := string(body)

	isTunOn := strings.Contains(data, `"tun":{"enable":true`)
	hasIf := false
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(i.Name, TUN_NAME) && i.Flags&net.FlagUp != 0 { hasIf = true; break }
	}

	if isTunOn && !hasIf {
		systray.SetIcon(getIcon("error.ico"))
	} else if isTunOn {
		systray.SetIcon(getIcon("tun.ico")); mT.Check()
	} else {
		mT.Uncheck()
		if isProxyEnabled() { systray.SetIcon(getIcon("proxy.ico")); mP.Check() } else {
			systray.SetIcon(getIcon("default.ico")); mP.Uncheck()
		}
	}
}

func syncTunState() {
	resp, err := http.Get(API_URL + "/configs")
	if err != nil { return }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	isApiOn := strings.Contains(string(b), `"tun":{"enable":true`)
	if conf.TunEnabled && !isApiOn { patchApi(`{"tun": {"enable": true}}`) } else if !conf.TunEnabled && isApiOn { patchApi(`{"tun": {"enable": false}}`) }
}

func patchApi(j string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(j)))
	(&http.Client{Timeout: time.Second}).Do(req)
}

func killCore() { exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run() }

func isProxyEnabled() bool {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

func setProxy(e bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if e { k.SetDWordValue("ProxyEnable", 1); k.SetStringValue("ProxyServer", PROXY_ADDR) } else { k.SetDWordValue("ProxyEnable", 0) }
	k.Close()
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func updateAutoStart(e bool) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	if e { k.SetStringValue("MihomoLauncher", "\""+exePath+"\" -silent") } else { k.DeleteValue("MihomoLauncher") }
}

func getIcon(n string) []byte { data, _ := iconFs.ReadFile("icons/" + n); return data }

func saveConfig() {
	f, _ := os.Create(iniPath)
	defer f.Close()
	fmt.Fprintf(f, "[Settings]\nauto_start = %v\nservice_mode = %v\ntray_hidden = %v\ntun_enabled = %v\n", conf.AutoStart, conf.ServiceMode, conf.TrayHidden, conf.TunEnabled)
}

func loadConfig() {
	f, err := os.Open(iniPath)
	if err != nil { conf = Config{}; saveConfig(); return }
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		l := s.Text()
		if strings.HasPrefix(l, "auto_start = true") { conf.AutoStart = true }
		if strings.HasPrefix(l, "service_mode = true") { conf.ServiceMode = true }
		if strings.HasPrefix(l, "tray_hidden = true") { conf.TrayHidden = true }
		if strings.HasPrefix(l, "tun_enabled = true") { conf.TunEnabled = true }
	}
}

func isAdmin() bool {
	var t windows.Token
	windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &t)
	return t.IsElevated()
}

func main() {
	// 判断是否为自启（带 -silent 参数）
	for _, arg := range os.Args {
		if arg == "-silent" { isUserOpen = false; break }
	}

	if !isAdmin() {
		v, _ := syscall.UTF16PtrFromString("runas")
		e, _ := syscall.UTF16PtrFromString(exePath)
		windows.ShellExecute(0, v, e, nil, nil, windows.SW_HIDE)
		return
	}

	loadConfig()
	
	// 如果是手动双击，强制不隐藏
	if isUserOpen {
		conf.TrayHidden = false
		saveConfig()
	}

	// 初始化 Job Object 防止内核残留
	hJob, _ = windows.CreateJobObject(nil, nil)
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(uintptr(hJob), 10, uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))))

	go handleIPC()    // 监听唤醒信号
	go engineKeeper() // 守护内核进程

	if conf.TrayHidden {
		select {} // 隐藏模式：主线程阻塞，后台协程继续运行
	} else {
		systray.Run(onReady, func() {})
	}
}
