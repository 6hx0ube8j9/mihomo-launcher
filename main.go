package main

import (
	"bufio"
	"bytes"
	"embed"
	"fmt"
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
	PIPE_NAME   = `\\.\pipe\MihomoLauncherWakeup`
	APP_MUTEX   = "Global\\MihomoUltimateManager_V25_Official"
	API_URL     = "http://127.0.0.1:9090"
	CONFIG_FILE = "mihomo-launcher.ini"
	PROXY_ADDR  = "127.0.0.1:7890"
)

var (
	isReallyExiting bool
	isHidden        bool
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 2 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
)

// --- 1. 系统底层工具 ---

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
	// 通知系统刷新代理设置
	windows.NewLazySystemDLL("wininet.dll").NewProc("InternetSetOptionW").Call(0, 39, 0, 0) // INTERNET_OPTION_SETTINGS_CHANGED
	windows.NewLazySystemDLL("wininet.dll").NewProc("InternetSetOptionW").Call(0, 37, 0, 0) // INTERNET_OPTION_REFRESH
}

// --- 2. 菜单与 UI 逻辑 ---

func onReady() {
	cfg := loadIniConfig()
	isHidden = cfg["tray_hidden"] == "true"

	// 初始化图标
	updateIcon(isHidden)

	// 1. 基础操作类
	mWeb := systray.AddMenuItem("打开控制面板", "打开浏览器 UI")
	mDir := systray.AddMenuItem("打开程序目录", "打开资源管理器")
	systray.AddSeparator()

	// 2. 内核状态控制类
	// 代理模式
	mProxyMode := systray.AddMenuItem("代理模式切换", "")
	mModeRule := mProxyMode.AddSubMenuItemCheckbox("规则模式 (Rule)", "", cfg["proxy_mode"] == "rule" || cfg["proxy_mode"] == "")
	mModeGlobal := mProxyMode.AddSubMenuItemCheckbox("全局模式 (Global)", "", cfg["proxy_mode"] == "global")
	mModeDirect := mProxyMode.AddSubMenuItemCheckbox("直连模式 (Direct)", "", cfg["proxy_mode"] == "direct")

	// 系统代理与 TUN
	mSysProxy := systray.AddMenuItemCheckbox("系统代理 (System Proxy)", "", cfg["system_proxy"] == "true")
	mTun := systray.AddMenuItemCheckbox("TUN 模式开关", "", cfg["tun_on"] == "true")

	systray.AddSeparator()

	// 3. 服务管理类 (二级菜单)
	mSvcRoot := systray.AddMenuItem("系统服务管理", "")
	mSvcInst := mSvcRoot.AddSubMenuItem("安装/修复 服务", "Stop -> Install -> Start")
	mSvcUninst := mSvcRoot.AddSubMenuItem("卸载系统服务", "")
	mSvcBat := mSvcRoot.AddSubMenuItem("运行管理脚本 (BAT)", "")

	systray.AddSeparator()

	// 4. 交互控制
	mHide := systray.AddMenuItem("隐藏托盘图标", "双击可重新唤醒")
	mExit := systray.AddMenuItem("彻底退出程序", "")

	// 启动初始化同步
	go func() {
		time.Sleep(2 * time.Second)
		if cfg["system_proxy"] == "true" { setSystemProxy(true) }
		if cfg["tun_on"] == "true" { setCfgRemote(`{"tun":{"enable":true}}`) }
		if mode := cfg["proxy_mode"]; mode != "" { setCfgRemote(`{"mode":"` + mode + `"}`) }
	}()

	// 菜单监听循环
	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		
		// 模式切换逻辑 (三位一体)
		case <-mModeRule.ClickedCh:
			mModeRule.Check(); mModeGlobal.Uncheck(); mModeDirect.Uncheck()
			setCfgRemote(`{"mode":"rule"}`); saveIniConfig("proxy_mode", "rule")
		case <-mModeGlobal.ClickedCh:
			mModeRule.Uncheck(); mModeGlobal.Check(); mModeDirect.Uncheck()
			setCfgRemote(`{"mode":"global"}`); saveIniConfig("proxy_mode", "global")
		case <-mModeDirect.ClickedCh:
			mModeRule.Uncheck(); mModeGlobal.Uncheck(); mModeDirect.Check()
			setCfgRemote(`{"mode":"direct"}`); saveIniConfig("proxy_mode", "direct")
			// 特殊逻辑：切到直连可选自动关代理（此处按需求仅切换模式）

		case <-mSysProxy.ClickedCh:
			if mSysProxy.Checked() {
				mSysProxy.Uncheck(); setSystemProxy(false); saveIniConfig("system_proxy", "false")
			} else {
				mSysProxy.Check(); setSystemProxy(true); saveIniConfig("system_proxy", "true")
			}

		case <-mTun.ClickedCh:
			if mTun.Checked() {
				mTun.Uncheck(); setCfgRemote(`{"tun":{"enable":false}}`); saveIniConfig("tun_on", "false")
			} else {
				mTun.Check(); setCfgRemote(`{"tun":{"enable":true}}`); saveIniConfig("tun_on", "true")
			}

		// 服务管理逻辑
		case <-mSvcInst.ClickedCh:
			manageService("install")
		case <-mSvcUninst.ClickedCh:
			manageService("uninstall")
		case <-mSvcBat.ClickedCh:
			runServiceBat()

		case <-mHide.ClickedCh:
			isHidden = true; saveIniConfig("tray_hidden", "true"); updateIcon(true)
		case <-mExit.ClickedCh:
			isReallyExiting = true; systray.Quit()
		}
	}
}

// --- 3. 核心管理函数 ---

func manageService(action string) {
	svcExe := filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	run := func(args ...string) {
		cmd := exec.Command(svcExe, args...)
		cmd.Dir = filepath.Dir(svcExe)
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = cmd.Run()
	}
	switch action {
	case "install":
		run("stop"); run("install"); run("start")
	case "uninstall":
		run("stop"); run("uninstall")
		exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	case "start": run("start")
	case "stop": run("stop")
	}
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		// 检查 API 是否存活
		_, err := httpClient.Get(API_URL)
		if err != nil && !isProcessRunning("mihomo.exe") {
			cmd := exec.Command(target, "-d", baseDir)
			cmd.Dir = baseDir
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			_ = cmd.Start()
			if hJob != 0 && cmd.Process != nil {
				hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				_ = windows.AssignProcessToJobObject(hJob, hp)
				windows.CloseHandle(hp)
			}
		}
		time.Sleep(10 * time.Second)
	}
}

// --- 4. 生命周期与 IPC ---

func onExit() {
	if !isReallyExiting { return }
	// 彻底清场
	setSystemProxy(false) // 恢复注册表，防止断网
	if isServiceInstalled() {
		manageService("stop")
	}
	if hJob != 0 { windows.CloseHandle(hJob) }
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	os.Exit(0)
}

func main() {
	if !isAdmin() { runAsAdmin(); os.Exit(0) }

	// 互斥体锁死逻辑
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hMutex, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if hMutex != 0 { windows.CloseHandle(hMutex) }
		wakeupExistingInstance()
		os.Exit(0) // 存在即退出
	}

	os.Chdir(baseDir)
	initJobObject()
	go startIpcServer()

	// 环境感知启动
	if isServiceInstalled() {
		manageService("start")
	} else {
		go monitorKernelDaemon()
	}

	systray.Run(onReady, onExit)
}

// --- 5. 辅助工具 ---

func updateIcon(hidden bool) {
	if hidden {
		systray.SetIcon([]byte{})
	} else {
		b, _ := iconFs.ReadFile("icons/default.ico")
		systray.SetIcon(b)
	}
}

func startIpcServer() {
	l, err := net.Listen("pipe", PIPE_NAME)
	if err != nil { return }
	for {
		conn, err := l.Accept()
		if err != nil { continue }
		go func(c net.Conn) {
			defer c.Close()
			scanner := bufio.NewScanner(c)
			if scanner.Scan() && scanner.Text() == "WAKEUP" {
				isHidden = false
				saveIniConfig("tray_hidden", "false")
				updateIcon(false)
			}
		}(conn)
	}
}

func wakeupExistingInstance() {
	conn, err := net.Dial("pipe", PIPE_NAME)
	if err == nil {
		fmt.Fprintln(conn, "WAKEUP")
		conn.Close()
	}
}

func isServiceInstalled() bool {
	m, _ := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if m == 0 { return false }
	defer windows.CloseServiceHandle(m)
	s, err := windows.OpenService(m, windows.StringToUTF16Ptr("mihomo"), windows.SERVICE_QUERY_CONFIG)
	if err == nil {
		windows.CloseServiceHandle(s)
		return true
	}
	return false
}

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(h), uintptr(windows.JobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))),
		)
		hJob = h
	}
}

func isProcessRunning(name string) bool {
	snapshot, _ := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if snapshot == 0 { return false }
	defer windows.CloseHandle(snapshot)
	var proc windows.ProcessEntry32
	proc.Size = uint32(unsafe.Sizeof(proc))
	for windows.Process32Next(snapshot, &proc) == nil {
		if strings.EqualFold(windows.UTF16ToString(proc.ExeFile[:]), name) { return true }
	}
	return false
}

func setCfgRemote(jsonStr string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(jsonStr)))
	if r, err := httpClient.Do(req); err == nil { r.Body.Close() }
}

func loadIniConfig() map[string]string {
	res := make(map[string]string)
	data, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) == 2 { res[parts[0]] = parts[1] }
	}
	return res
}

func saveIniConfig(key, val string) {
	cfg := loadIniConfig()
	cfg[key] = val
	var buf bytes.Buffer
	for k, v := range cfg { buf.WriteString(fmt.Sprintf("%s=%s\n", k, v)) }
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func runServiceBat() {
	target := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	verb, _ := syscall.UTF16PtrFromString("runas")
	path, _ := syscall.UTF16PtrFromString(target)
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(target))
	windows.ShellExecute(0, verb, path, nil, dir, windows.SW_SHOWNORMAL)
}
