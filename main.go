package main

import (
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
	IPC_PORT   = "127.0.0.1:54321"
)

type Config struct {
	AutoStart   bool
	ServiceMode bool
	TunEnabled  bool
	TrayHidden  bool
}

var (
	conf       Config
	exePath, _ = filepath.Abs(os.Args[0])
	baseDir    = filepath.Dir(exePath)
	coreExe    = filepath.Join(baseDir, "bin", "mihomo.exe")
	iniPath    = filepath.Join(baseDir, "mihomo-launcher.ini")
	hJob       windows.Handle
)

// --- 进程与 IPC：解决双击唤醒 ---

func handleIPC() {
	ln, err := net.Listen("tcp", IPC_PORT)
	if err != nil {
		conn, err := net.Dial("tcp", IPC_PORT)
		if err == nil {
			conn.Write([]byte("RESHOW"))
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
		if string(buf[:n]) == "RESHOW" {
			// 收到唤醒信号：静默修改配置并重启 Launcher 界面
			writeIni(false) 
			exec.Command(exePath).Start()
			os.Exit(0)
		}
		conn.Close()
	}
}

// --- 核心修复：原生静默启动与 JobObject 绑定 ---

func initJob() {
	hJob, _ = windows.CreateJobObject(nil, nil)
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
		uintptr(hJob), 10, uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))),
	)
}

func engineKeeper() {
	for {
		if !conf.ServiceMode {
			// 探测 API，失败则物理拉起
			resp, err := http.Get(API_URL + "/version")
			if err != nil {
				startCoreNative()
			} else {
				resp.Body.Close()
			}
		}
		syncTunAndConfig()
		time.Sleep(3 * time.Second)
	}
}

func startCoreNative() {
	// 关键修复：强制设置 Dir 属性，并使用绝对路径指定工作目录
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

// --- 核心修复：无黑框执行 BAT ---

func runSvcBat(action string) {
	bat := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	args := fmt.Sprintf("/c start /d \"%s\" %s %s", filepath.Dir(bat), filepath.Base(bat), action)
	v, _ := syscall.UTF16PtrFromString("cmd")
	a, _ := syscall.UTF16PtrFromString(args)
	d, _ := syscall.UTF16PtrFromString(baseDir)
	windows.ShellExecute(0, nil, v, a, d, windows.SW_HIDE)
}

// --- 模式切换与 UI ---

func onReady() {
	systray.SetIcon(getIcon("default.ico"))
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", isProxyOn())
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()
	mRule := systray.AddMenuItemCheckbox("规则模式", "", false)
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", false)
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", false)
	systray.AddSeparator()
	mSet := systray.AddMenuItem("启动设置", "")
	mAuto := mSet.AddSubMenuItemCheckbox("自动启动", "", conf.AutoStart)
	mInst := mSet.AddSubMenuItem("安装服务", "")
	mUnin := mSet.AddSubMenuItem("卸载服务", "")
	mRes := mSet.AddSubMenuItem("重启内核", "")
	mFull := mSet.AddSubMenuItem("彻底退出程序", "")
	systray.AddSeparator()
	mHide := systray.AddMenuItem("隐藏托盘图标", "")

	go func() {
		for {
			refreshStatus(mProxy, mTun, mRule, mGlobal, mDirect, mInst, mUnin)
			time.Sleep(2 * time.Second)
		}
	}()

	for {
		select {
		case <-mWeb.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh: setProxy(!isProxyOn())
		case <-mTun.ClickedCh:
			conf.TunEnabled = !mTun.Checked()
			writeIni(conf.TrayHidden)
		case <-mRule.ClickedCh: sendPatch(`{"mode": "rule"}`)
		case <-mGlobal.ClickedCh: sendPatch(`{"mode": "global"}`)
		case <-mDirect.ClickedCh: sendPatch(`{"mode": "direct"}`)
		case <-mAuto.ClickedCh:
			conf.AutoStart = !mAuto.Checked()
			regAutoStart(conf.AutoStart)
			writeIni(conf.TrayHidden)
		case <-mInst.ClickedCh: runSvcBat("install"); conf.ServiceMode = true; writeIni(conf.TrayHidden)
		case <-mUnin.ClickedCh: runSvcBat("uninstall"); conf.ServiceMode = false; writeIni(conf.TrayHidden)
		case <-mRes.ClickedCh:
			if conf.ServiceMode { runSvcBat("restart") } else { windows.TerminateProcess(windows.CurrentProcess(), 0) /* 触发重启脚本或手动拉起 */ }
		case <-mFull.ClickedCh:
			setProxy(false)
			if conf.ServiceMode { runSvcBat("stop") }
			os.Exit(0) // JobObject 会自动静默清理内核进程
		case <-mHide.ClickedCh:
			writeIni(true)
			systray.Quit()
		}
	}
}

func refreshStatus(mP, mT, mR, mG, mD, mI, mU *systray.MenuItem) {
	if conf.ServiceMode { mI.Disable(); mU.Enable() } else { mI.Enable(); mU.Disable() }
	resp, err := http.Get(API_URL + "/configs")
	if err != nil {
		systray.SetIcon(getIcon("stop.ico"))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	data := string(body)

	// 同步模式勾选
	if strings.Contains(data, `"mode":"rule"`) { mR.Check(); mG.Uncheck(); mD.Uncheck() }
	if strings.Contains(data, `"mode":"global"`) { mR.Uncheck(); mG.Check(); mD.Uncheck() }
	if strings.Contains(data, `"mode":"direct"`) { mR.Uncheck(); mG.Uncheck(); mD.Check() }

	// 图标状态
	isTun := strings.Contains(data, `"tun":{"enable":true`)
	hasIf := false
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(i.Name, TUN_NAME) && i.Flags&net.FlagUp != 0 { hasIf = true; break }
	}
	if isTun && !hasIf { systray.SetIcon(getIcon("error.ico")) } else if isTun {
		systray.SetIcon(getIcon("tun.ico")); mT.Check()
	} else {
		mT.Uncheck()
		if isProxyOn() { systray.SetIcon(getIcon("proxy.ico")); mP.Check() } else {
			systray.SetIcon(getIcon("default.ico")); mP.Uncheck()
		}
	}
}

// --- 系统工具 ---

func isProxyOn() bool {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

func setProxy(e bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if e { k.SetDWordValue("ProxyEnable", 1); k.SetStringValue("ProxyServer", PROXY_ADDR) } else { k.SetDWordValue("ProxyEnable", 0) }
	k.Close()
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0,0,0,0)
}

func regAutoStart(e bool) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	if e { k.SetStringValue("MihomoLauncher", "\""+exePath+"\" -silent") } else { k.DeleteValue("MihomoLauncher") }
}

func sendPatch(j string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(j)))
	(&http.Client{Timeout: time.Second}).Do(req)
}

func syncTunAndConfig() {
	resp, err := http.Get(API_URL + "/configs")
	if err != nil { return }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	isOn := strings.Contains(string(b), `"tun":{"enable":true`)
	if conf.TunEnabled && !isOn { sendPatch(`{"tun": {"enable": true}}`) } else if !conf.TunEnabled && isOn { sendPatch(`{"tun": {"enable": false}}`) }
}

func getIcon(n string) []byte { d, _ := iconFs.ReadFile("icons/" + n); return d }

func writeIni(hidden bool) {
	conf.TrayHidden = hidden
	f, _ := os.Create(iniPath)
	defer f.Close()
	fmt.Fprintf(f, "[Settings]\nauto_start = %v\nservice_mode = %v\ntray_hidden = %v\ntun_enabled = %v\n", conf.AutoStart, conf.ServiceMode, conf.TrayHidden, conf.TunEnabled)
}

func loadIni() {
	f, err := os.ReadFile(iniPath)
	if err != nil { writeIni(false); return }
	s := string(f)
	conf.AutoStart = strings.Contains(s, "auto_start = true")
	conf.ServiceMode = strings.Contains(s, "service_mode = true")
	conf.TrayHidden = strings.Contains(s, "tray_hidden = true")
	conf.TunEnabled = strings.Contains(s, "tun_enabled = true")
}

func main() {
	// 启动源判定
	isSilent := false
	for _, a := range os.Args { if a == "-silent" { isSilent = true } }
	
	loadIni()
	if !isSilent { conf.TrayHidden = false } // 用户双击强显图标

	initJob()
	go handleIPC()
	go engineKeeper()

	if conf.TrayHidden {
		select {} // 纯后台模式
	} else {
		systray.Run(onReady, func() {})
	}
}
