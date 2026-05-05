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
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	PIPE_NAME   = `\\.\pipe\MihomoLauncherWakeup`
	APP_MUTEX   = "Global\\MihomoUltimateManager_V25_Final"
	API_URL     = "http://127.0.0.1:9090"
	CONFIG_FILE = "mihomo-launcher.ini"
)

var (
	isReallyExiting bool
	isHidden        bool
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 2 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
)

// --- 1. 核心工具函数 ---

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

// --- 2. 服务管理与进程控制 ---

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

func manageService(action string) {
	svcExe := filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	run := func(args ...string) {
		cmd := exec.Command(svcExe, args...)
		cmd.Dir = filepath.Dir(svcExe)
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = cmd.Run()
	}
	switch action {
	case "start": run("start")
	case "stop": run("stop")
	case "install":
		run("stop"); run("install"); run("start")
	case "uninstall":
		run("stop"); run("uninstall")
		exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	}
}

func runServiceBat() {
	target := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	verb, _ := syscall.UTF16PtrFromString("runas")
	path, _ := syscall.UTF16PtrFromString(target)
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(target))
	windows.ShellExecute(0, verb, path, nil, dir, windows.SW_SHOWNORMAL)
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
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

// --- 3. UI 托盘与功能实现 ---

func onReady() {
	cfg := loadIniConfig()
	isHidden = cfg["tray_hidden"] == "true"

	if isHidden {
		systray.SetIcon([]byte{})
	} else {
		systray.SetIcon(getIcon("default.ico"))
	}

	// 找回所有功能菜单
	mWeb := systray.AddMenuItem("打开控制面板", "")
	mDir := systray.AddMenuItem("打开程序目录", "")
	systray.AddSeparator()

	// 找回核心功能：TUN 与 代理模式
	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", cfg["tun_on"] == "true")
	mProxyMode := systray.AddMenuItem("代理模式切换", "")
	mModeRule := mProxyMode.AddSubMenuItemCheckbox("规则 (Rule)", "", cfg["proxy_mode"] == "rule" || cfg["proxy_mode"] == "")
	mModeGlobal := mProxyMode.AddSubMenuItemCheckbox("全局 (Global)", "", cfg["proxy_mode"] == "global")
	mModeDirect := mProxyMode.AddSubMenuItemCheckbox("直连 (Direct)", "", cfg["proxy_mode"] == "direct")

	systray.AddSeparator()
	mSvcBat := systray.AddMenuItem("管理服务 (BAT脚本)", "")
	mSvcInst := systray.AddMenuItem("安装为系统服务", "")
	mSvcUninst := systray.AddMenuItem("卸载系统服务", "")

	systray.AddSeparator()
	mHide := systray.AddMenuItem("隐藏托盘图标", "隐藏后双击Launcher唤醒")
	mExit := systray.AddMenuItem("彻底退出", "")

	// 状态同步协程
	go func() {
		time.Sleep(2 * time.Second)
		if cfg["tun_on"] == "true" { setCfgRemote(`{"tun":{"enable":true}}`) }
		if mode, ok := cfg["proxy_mode"]; ok && mode != "" { setCfgRemote(`{"mode":"` + mode + `"}`) }
	}()

	// 菜单监听主循环
	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mTun.ClickedCh:
			if mTun.Checked() {
				mTun.Uncheck(); setCfgRemote(`{"tun":{"enable":false}}`); saveIniConfig("tun_on", "false")
			} else {
				mTun.Check(); setCfgRemote(`{"tun":{"enable":true}}`); saveIniConfig("tun_on", "true")
			}
		case <-mModeRule.ClickedCh:
			mModeRule.Check(); mModeGlobal.Uncheck(); mModeDirect.Uncheck()
			setCfgRemote(`{"mode":"rule"}`); saveIniConfig("proxy_mode", "rule")
		case <-mModeGlobal.ClickedCh:
			mModeRule.Uncheck(); mModeGlobal.Check(); mModeDirect.Uncheck()
			setCfgRemote(`{"mode":"global"}`); saveIniConfig("proxy_mode", "global")
		case <-mModeDirect.ClickedCh:
			mModeRule.Uncheck(); mModeGlobal.Uncheck(); mModeDirect.Check()
			setCfgRemote(`{"mode":"direct"}`); saveIniConfig("proxy_mode", "direct")
		case <-mSvcBat.ClickedCh:
			runServiceBat()
		case <-mSvcInst.ClickedCh:
			manageService("install")
		case <-mSvcUninst.ClickedCh:
			manageService("uninstall")
		case <-mHide.ClickedCh:
			isHidden = true; saveIniConfig("tray_hidden", "true"); systray.SetIcon([]byte{})
		case <-mExit.ClickedCh:
			isReallyExiting = true; systray.Quit()
		}
	}
}

// --- 4. 生命周期与 IPC ---

func onExit() {
	if !isReallyExiting { return }
	if isServiceInstalled() { manageService("stop") }
	if hJob != 0 { windows.CloseHandle(hJob) }
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	os.Exit(0)
}

func main() {
	if !isAdmin() { runAsAdmin(); os.Exit(0) }

	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hMutex, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if hMutex != 0 { windows.CloseHandle(hMutex) }
		wakeupExistingInstance()
		os.Exit(0)
	}

	os.Chdir(baseDir)
	initJobObject()
	go startIpcServer()

	if isServiceInstalled() {
		manageService("start")
	} else {
		go monitorKernelDaemon()
	}

	systray.Run(onReady, onExit)
}

// --- 5. 通用辅助函数 ---

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
				systray.SetIcon(getIcon("default.ico"))
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

func getIcon(n string) []byte {
	b, _ := iconFs.ReadFile("icons/" + n)
	return b
}
