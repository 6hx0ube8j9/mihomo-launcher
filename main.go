package main

import (
	"bytes"
	"embed"
	"encoding/json"
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
	AutoStart   bool `json:"auto_start"`
	ServiceMode bool `json:"service_mode"`
	TunEnabled  bool `json:"tun_enabled"`
	TrayHidden  bool `json:"tray_hidden"`
}

var (
	conf       Config
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	coreExe    = filepath.Join(baseDir, "bin", "mihomo.exe")
	hJob       windows.Handle
)

// --- 修复 1：单实例与唤醒逻辑 (解决隐藏后无法救活) ---

func handleIPC() {
	ln, err := net.Listen("tcp", IPC_PORT)
	if err != nil {
		// 发现已有实例，发送信号
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
			// 修改配置并物理重启，保证 UI 刷新
			conf.TrayHidden = false
			saveConfig()
			restartSelf()
		}
		conn.Close()
	}
}

func restartSelf() {
	verb, _ := syscall.UTF16PtrFromString("open")
	exe, _ := syscall.UTF16PtrFromString(exePath)
	windows.ShellExecute(0, verb, exe, nil, nil, windows.SW_SHOWNORMAL)
	os.Exit(0)
}

// --- 修复 2：内核启动逻辑 (解决拉不起内核) ---

func engineKeeper() {
	for {
		if !conf.ServiceMode {
			ensureCoreProcess()
		}
		syncTunWithConfig()
		time.Sleep(3 * time.Second)
	}
}

func ensureCoreProcess() {
	// 尝试向 API 发包确认存活，比 tasklist 更快更准
	resp, err := http.Get(API_URL + "/version")
	if err == nil {
		resp.Body.Close()
		return // 内核在跑，无需拉起
	}

	// 物理拉起：核心修复在于 Dir 和绝对路径
	cmd := exec.Command(coreExe, "-d", baseDir)
	cmd.Dir = baseDir // 必须强制锁定工作目录
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
	}
	
	if err := cmd.Start(); err == nil && hJob != 0 {
		hProc, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		windows.AssignProcessToJobObject(hJob, hProc)
		windows.CloseHandle(hProc)
	}
}

// --- 修复 3：BAT 路径逻辑 (精准定位脚本) ---

func runServiceBat(action string) {
	// 修正：强制拼接绝对路径，不依赖相对路径逻辑
	batPath := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	batDir := filepath.Dir(batPath)
	
	args := "/c start /d \"" + batDir + "\" " + filepath.Base(batPath)
	if action != "" {
		args += " " + action
	}
	
	cmdPtr, _ := syscall.UTF16PtrFromString("cmd")
	argPtr, _ := syscall.UTF16PtrFromString(args)
	dirPtr, _ := syscall.UTF16PtrFromString(batDir)
	
	windows.ShellExecute(0, nil, cmdPtr, argPtr, dirPtr, windows.SW_HIDE)
}

// --- UI 与 状态 ---

func onReady() {
	systray.SetIcon(getIcon("default.ico"))
	
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", false)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()
	mRule := systray.AddMenuItemCheckbox("规则模式", "", false)
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", false)
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", false)
	systray.AddSeparator()

	mSvcSet := systray.AddMenuItem("启动设置", "")
	mAuto := mSvcSet.AddSubMenuItemCheckbox("开机自动启动", "", conf.AutoStart)
	mInst := mSvcSet.AddSubMenuItem("安装后台服务", "")
	mUninst := mSvcSet.AddSubMenuItem("卸载后台服务", "")
	mBat := mSvcSet.AddSubMenuItem("管理服务 (BAT)", "")
	mRes := mSvcSet.AddSubMenuItem("重启内核", "")
	mExit := mSvcSet.AddSubMenuItem("彻底退出程序", "")

	systray.AddSeparator()
	mDir := systray.AddMenuItem("打开程序目录", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "")

	go func() {
		for {
			refreshStatus(mProxy, mTun, mRule, mGlobal, mDirect, mInst, mUninst)
			time.Sleep(2 * time.Second)
		}
	}()

	for {
		select {
		case <-mWeb.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh: toggleSystemProxy(!isProxyActive())
		case <-mTun.ClickedCh:
			conf.TunEnabled = !mTun.Checked()
			saveConfig()
		case <-mRule.ClickedCh: sendPatch(`{"mode": "rule"}`)
		case <-mGlobal.ClickedCh: sendPatch(`{"mode": "global"}`)
		case <-mDirect.ClickedCh: sendPatch(`{"mode": "direct"}`)
		case <-mAuto.ClickedCh:
			conf.AutoStart = !mAuto.Checked()
			updateAutoStart(conf.AutoStart)
			saveConfig()
		case <-mInst.ClickedCh:
			runServiceBat("install")
			conf.ServiceMode = true
			saveConfig()
		case <-mUninst.ClickedCh:
			runServiceBat("uninstall")
			conf.ServiceMode = false
			saveConfig()
		case <-mRes.ClickedCh:
			if conf.ServiceMode { runServiceBat("restart") } else { killCore() }
		case <-mExit.ClickedCh:
			finalKillAll()
		case <-mDir.ClickedCh: exec.Command("explorer", baseDir).Run()
		case <-mBat.ClickedCh: runServiceBat("")
		case <-mHide.ClickedCh:
			conf.TrayHidden = true
			saveConfig()
			systray.Quit() // 这里只会关闭 UI 线程，main 的 select{} 会维持进程
		}
	}
}

// --- 底层辅助 ---

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

	isTunOn := strings.Contains(data, `"tun":{"enable":true`)
	hasNet := false
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(i.Name, TUN_NAME) && i.Flags&net.FlagUp != 0 { hasNet = true; break }
	}

	if isTunOn && !hasNet { systray.SetIcon(getIcon("error.ico")) } else if isTunOn {
		systray.SetIcon(getIcon("tun.ico")); mT.Check()
	} else {
		mT.Uncheck()
		if isProxyActive() { systray.SetIcon(getIcon("proxy.ico")); mP.Check() } else {
			systray.SetIcon(getIcon("default.ico")); mP.Uncheck()
		}
	}
	// 模式略... (同前逻辑)
}

func finalKillAll() {
	toggleSystemProxy(false)
	if conf.ServiceMode { runServiceBat("stop") }
	killCore()
	os.Exit(0)
}

func killCore() {
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
}

func isProxyActive() bool {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

func toggleSystemProxy(e bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if e { k.SetDWordValue("ProxyEnable", 1); k.SetStringValue("ProxyServer", PROXY_ADDR) } else { k.SetDWordValue("ProxyEnable", 0) }
	k.Close()
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func updateAutoStart(e bool) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	if e { k.SetStringValue("MihomoLauncher", "\""+exePath+"\"") } else { k.DeleteValue("MihomoLauncher") }
}

func sendPatch(j string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(j)))
	(&http.Client{Timeout: time.Second}).Do(req)
}

func syncTunWithConfig() {
	resp, err := http.Get(API_URL + "/configs")
	if err != nil { return }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	isApiOn := strings.Contains(string(b), `"tun":{"enable":true`)
	if conf.TunEnabled && !isApiOn { sendPatch(`{"tun": {"enable": true}}`) } else if !conf.TunEnabled && isApiOn { sendPatch(`{"tun": {"enable": false}}`) }
}

func getIcon(n string) []byte { data, _ := iconFs.ReadFile("icons/" + n); return data }
func saveConfig() { d, _ := json.MarshalIndent(conf, "", " "); os.WriteFile(filepath.Join(baseDir, "config.json"), d, 0644) }
func loadConfig() {
	d, err := os.ReadFile(filepath.Join(baseDir, "config.json"))
	if err == nil { json.Unmarshal(d, &conf) } else { conf = Config{}; saveConfig() }
}

func isAdmin() bool {
	var t windows.Token
	windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &t)
	return t.IsElevated()
}

func initJob() {
	hJob, _ = windows.CreateJobObject(nil, nil)
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(uintptr(hJob), 10, uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))))
}

func main() {
	if !isAdmin() {
		v, _ := syscall.UTF16PtrFromString("runas")
		e, _ := syscall.UTF16PtrFromString(exePath)
		windows.ShellExecute(0, v, e, nil, nil, windows.SW_HIDE)
		return
	}
	loadConfig()
	initJob()
	go handleIPC()
	go engineKeeper()

	if conf.TrayHidden {
		select {} // 关键：隐藏模式下主协程不退出，维持内核运行
	} else {
		systray.Run(onReady, func() {})
	}
}
