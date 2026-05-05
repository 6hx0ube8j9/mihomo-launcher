package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
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
	APP_MUTEX   = "Global\\MihomoUltimateManager_V21"
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

// --- 1. 启动逻辑：状态驱动 ---

func main() {
	// 管理员权限检查
	if !isAdmin() {
		runAsAdmin()
		os.Exit(0)
	}

	// 单实例检查与双击救活
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 { windows.CloseHandle(h) }
		wakeupExistingInstance()
		os.Exit(0)
	}

	// 环境初始化
	os.Chdir(baseDir)
	initJobObject()
	
	// 启动后台 IPC 服务（监听唤醒指令）
	go startIpcServer()

	// 探测环境并拉起内核
	if isServiceInstalled() {
		startMihomoService() // 服务模式
	} else {
		go monitorKernelDaemon() // 子进程守护模式
	}

	systray.Run(onReady, onExit)
}

// --- 2. 核心功能函数 ---

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

func startMihomoService() {
	svcExe := filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	cmd := exec.Command(svcExe, "start")
	cmd.Dir = filepath.Dir(svcExe)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = cmd.Run()
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
			if hJob != 0 {
				hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				_ = windows.AssignProcessToJobObject(hJob, hp)
				windows.CloseHandle(hp)
			}
		}
		time.Sleep(10 * time.Second)
	}
}

// --- 3. 退出逻辑：环境感知收割 ---

func onExit() {
	if !isReallyExiting { return }

	// 1. 如果有服务，静默停止
	if isServiceInstalled() {
		svcExe := filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
		cmd := exec.Command(svcExe, "stop")
		cmd.Dir = filepath.Dir(svcExe)
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = cmd.Run()
	}

	// 2. 如果是子进程，Job Object 会自动处理，此处显式释放
	if hJob != 0 {
		windows.CloseHandle(hJob)
	}

	// 3. 彻底清场兜底
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	os.Exit(0)
}

// --- 4. 配置与 UI 同步 ---

func onReady() {
	cfg := loadIniConfig()
	isHidden = cfg["tray_hidden"] == "true"

	if isHidden {
		systray.SetIcon([]byte{})
	} else {
		systray.SetIcon(getIcon("default.ico"))
	}

	// 恢复上次状态（TUN/代理模式）
	go func() {
		time.Sleep(2 * time.Second) // 等待内核就绪
		if cfg["tun_on"] == "true" { setCfgRemote(`{"tun":{"enable":true}}`) }
		if mode, ok := cfg["proxy_mode"]; ok { setCfgRemote(`{"mode":"` + mode + `"}`) }
	}()

	mWeb := systray.AddMenuItem("控制面板", "")
	mSvcBat := systray.AddMenuItem("管理服务 (BAT)", "")
	systray.AddSeparator()
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	mExit := systray.AddMenuItem("彻底退出", "")

	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mSvcBat.ClickedCh:
			runServiceBat()
		case <-mHide.ClickedCh:
			isHidden = true
			saveIniConfig("tray_hidden", "true")
			systray.SetIcon([]byte{})
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

// --- 辅助工具函数 (IPC/Admin/INI/etc.) ---

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

func loadIniConfig() map[string]string {
	res := make(map[string]string)
	data, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
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

func setCfgRemote(jsonStr string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(jsonStr)))
	if r, err := httpClient.Do(req); err == nil { r.Body.Close() }
}

func runServiceBat() {
	target := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	verb, _ := syscall.UTF16PtrFromString("runas")
	path, _ := syscall.UTF16PtrFromString(target)
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(target))
	windows.ShellExecute(0, verb, path, nil, dir, windows.SW_SHOWNORMAL)
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

func getIcon(n string) []byte {
	b, _ := iconFs.ReadFile("icons/" + n)
	return b
}
