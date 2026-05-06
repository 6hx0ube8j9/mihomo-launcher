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

// --- 资源嵌入 ---
//go:embed icons/*.ico
var iconFs embed.FS

const (
	PIPE_NAME   = `\\.\pipe\MihomoLauncherWakeup`
	APP_MUTEX   = "Global\\MihomoUltimateManager_V27_Official"
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

// --- 图标加载核心修复 ---
func getIconData(name string) []byte {
	// 确保路径和 embed 指令一致
	data, err := iconFs.ReadFile("icons/" + name)
	if err != nil {
		// 如果找不到图标，返回一个最小化的空图标数据防止 systray 崩溃
		return nil
	}
	return data
}

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
	wininet := windows.NewLazySystemDLL("wininet.dll")
	wininet.NewProc("InternetSetOptionW").Call(0, 39, 0, 0)
	wininet.NewProc("InternetSetOptionW").Call(0, 37, 0, 0)
}

// --- 2. 菜单逻辑 ---

func onReady() {
	cfg := loadIniConfig()
	isHidden = cfg["tray_hidden"] == "true"

	// 初始化图标
	if !isHidden {
		systray.SetIcon(getIconData("default.ico"))
	} else {
		systray.SetIcon([]byte{})
	}
	systray.SetTooltip("Mihomo Launcher")

	mWeb := systray.AddMenuItem("打开控制面板", "打开浏览器 UI")
	mDir := systray.AddMenuItem("打开程序目录", "")
	systray.AddSeparator()

	mProxyMode := systray.AddMenuItem("代理模式", "")
	mModeRule := mProxyMode.AddSubMenuItemCheckbox("规则模式", "", cfg["proxy_mode"] == "rule" || cfg["proxy_mode"] == "")
	mModeGlobal := mProxyMode.AddSubMenuItemCheckbox("全局模式", "", cfg["proxy_mode"] == "global")
	mModeDirect := mProxyMode.AddSubMenuItemCheckbox("直连模式", "", cfg["proxy_mode"] == "direct")

	mSysProxy := systray.AddMenuItemCheckbox("系统代理", "", cfg["system_proxy"] == "true")
	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", cfg["tun_on"] == "true")

	systray.AddSeparator()
	mHide := systray.AddMenuItem("隐藏图标", "")
	mExit := systray.AddMenuItem("退出程序", "")

	// 周期性检查内核状态并更新图标（可选，增加灵动感）
	go func() {
		for {
			if !isHidden {
				_, err := httpClient.Get(API_URL)
				if err != nil {
					systray.SetIcon(getIconData("stop.ico"))
				} else {
					systray.SetIcon(getIconData("default.ico"))
				}
			}
			time.Sleep(5 * time.Second)
		}
	}()

	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mModeRule.ClickedCh:
			mModeRule.Check(); mModeGlobal.Uncheck(); mModeDirect.Uncheck()
			setCfgRemote(`{"mode":"rule"}`); saveIniConfig("proxy_mode", "rule")
		case <-mModeGlobal.ClickedCh:
			mModeRule.Uncheck(); mModeGlobal.Check(); mModeDirect.Uncheck()
			setCfgRemote(`{"mode":"global"}`); saveIniConfig("proxy_mode", "global")
		case <-mModeDirect.ClickedCh:
			mModeRule.Uncheck(); mModeGlobal.Uncheck(); mModeCheck := mModeDirect; mModeCheck.Check()
			setCfgRemote(`{"mode":"direct"}`); saveIniConfig("proxy_mode", "direct")
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
		case <-mHide.ClickedCh:
			isHidden = true; saveIniConfig("tray_hidden", "true")
			systray.SetIcon([]byte{})
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

func onExit() {
	setSystemProxy(false)
	if hJob != 0 { windows.CloseHandle(hJob) }
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	os.Exit(0)
}

// --- 3. 守护逻辑 ---

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		_, err := httpClient.Get(API_URL)
		if err != nil && !isProcessRunning("mihomo.exe") {
			cmd := exec.Command(target, "-d", baseDir)
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			_ = cmd.Start()
			if hJob != 0 && cmd.Process != nil {
				hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				windows.AssignProcessToJobObject(hJob, hp)
				windows.CloseHandle(hp)
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func main() {
	// 1. 权限检查：非管理员则请求提权并退出当前进程
	if !isAdmin() {
		runAsAdmin()
		os.Exit(0)
	}

	// 2. 单实例检测 (Mutex)
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	// 修正：使用 _, _ 忽略未使用的变量，直接通过 GetLastError 判断
	_, _ = windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		// 如果已存在实例，发送唤醒信号后退出
		wakeupExistingInstance()
		os.Exit(0)
	}

	// 3. 环境准备
	os.Chdir(baseDir)    // 切换工作目录到程序所在位置
	initJobObject()      // 初始化 Job Object，确保 Launcher 退出时内核跟着退出
	
	// 4. 启动后台服务协程
	go startIpcServer()      // 监听命名管道唤醒信号
	go monitorKernelDaemon() // 守护内核进程，挂了自动重连

	// 5. 启动托盘 UI 循环 (阻塞运行)
	// onReady: 初始化菜单和图标逻辑
	// onExit:  退出时的清理逻辑（如关闭内核、清理代理）
	systray.Run(onReady, onExit)
}

// --- 4. 辅助函数 (保持原样，仅修复路径) ---

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
				systray.SetIcon(getIconData("default.ico"))
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

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		kernel32 := windows.NewLazySystemDLL("kernel32.dll")
		kernel32.NewProc("SetInformationJobObject").Call(
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
