package main

import (
	"bufio"
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
	APP_MUTEX   = "Global\\MihomoUltimateManager_V19"
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

// --- 基础工具 ---

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
		if strings.EqualFold(windows.UTF16ToString(proc.ExeFile[:]), name) {
			return true
		}
	}
	return false
}

// --- 核心修复：服务管理逻辑 ---

func runServiceBat() {
	// 对应 \mihomo\mihomo-service\mihomo-service.bat
	target := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	verbPtr, _ := syscall.UTF16PtrFromString("runas")
	pathPtr, _ := syscall.UTF16PtrFromString(target)
	dirPtr, _ := syscall.UTF16PtrFromString(filepath.Dir(target))
	windows.ShellExecute(0, verbPtr, pathPtr, nil, dirPtr, windows.SW_SHOWNORMAL)
}

func manageService(action string) {
	svcExe := filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	svcDir := filepath.Dir(svcExe)

	run := func(args ...string) {
		cmd := exec.Command(svcExe, args...)
		cmd.Dir = svcDir
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = cmd.Run()
	}

	if action == "install" {
		run("stop")
		run("install")
		run("start")
	} else if action == "uninstall" {
		run("stop")
		// 杀掉相关进程
		exec.Command("taskkill", "/f", "/t", "/im", "mihomo-launcher.exe").Run()
		exec.Command("taskkill", "/f", "/t", "/im", "mihomo.exe").Run()
		run("uninstall")
	}
}

// --- IPC 唤醒逻辑 ---

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
				saveHiddenState(false)
				systray.SetIcon(getIcon("default.ico")) // 重新显示图标
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

// --- 配置持久化 ---

func loadHiddenState() bool {
	data, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	return strings.Contains(string(data), "hidden=true")
}

func saveHiddenState(h bool) {
	s := "hidden=false"
	if h { s = "hidden=true" }
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), []byte(s), 0644)
}

// --- 主程序 ---

func main() {
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		wakeupExistingInstance()
		os.Exit(0)
	}

	initJobObject()
	os.Chdir(baseDir)
	isHidden = loadHiddenState()

	go startIpcServer()
	go monitorKernel()

	systray.Run(onReady, onExit)
}

func monitorKernel() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		_, err := httpClient.Get(API_URL)
		// 仅在 API 失败且 进程不存在时才启动，防止无限多开
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
		time.Sleep(5 * time.Second)
	}
}

func onReady() {
	if isHidden {
		systray.SetIcon([]byte{})
	} else {
		systray.SetIcon(getIcon("default.ico"))
	}

	mWeb := systray.AddMenuItem("控制面板", "")
	mDir := systray.AddMenuItem("打开程序目录", "")
	systray.AddSeparator()

	mSvcBat := systray.AddMenuItem("管理服务 (BAT)", "")
	mSvcInst := systray.AddMenuItem("安装服务", "")
	mSvcUninst := systray.AddMenuItem("卸载服务", "")
	systray.AddSeparator()

	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	mExit := systray.AddMenuItem("彻底退出", "")

	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mSvcBat.ClickedCh:
			runServiceBat() // 核心修复：调用指定的 .bat
		case <-mSvcInst.ClickedCh:
			manageService("install")
		case <-mSvcUninst.ClickedCh:
			manageService("uninstall")
		case <-mHide.ClickedCh:
			isHidden = true
			saveHiddenState(true)
			systray.SetIcon([]byte{}) // 隐藏图标但不退出
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

func getIcon(n string) []byte {
	b, _ := iconFs.ReadFile("icons/" + n)
	return b
}

func onExit() {
	if isReallyExiting {
		if hJob != 0 { windows.CloseHandle(hJob) }
		os.Exit(0)
	}
}
