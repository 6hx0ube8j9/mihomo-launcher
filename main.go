package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	API_URL      = "http://127.0.0.1:9090"
	PROXY_ADDR   = "127.0.0.1:7890"
	IPC_PORT     = "127.0.0.1:54321"
	SERVICE_NAME = "Mihomo"
	APP_MUTEX    = "Global\\MihomoLauncherMutex"
)

type Config struct {
	AutoStart   bool
	ServiceMode bool
	TunEnabled  bool
	TrayHidden  bool
	SystemProxy bool
	Mode        string
}

var (
	conf        Config
	confMu      sync.RWMutex
	fullExeP    string
	baseDir     string
	coreExe     string
	svcExe      string
	iniPath     string

	mainCtx, cancel = context.WithCancel(context.Background())
	wg              sync.WaitGroup

	startMu   sync.Mutex
	lastStart time.Time
	hJob      windows.Handle

	httpClient = &http.Client{Timeout: 2 * time.Second}
)

func isAdmin() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

func runAsAdmin() {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := os.Executable()
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	cwd, _ := os.Getwd()
	cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
	var argsPtr *uint16
	if len(os.Args) > 1 {
		argsPtr, _ = syscall.UTF16PtrFromString(strings.Join(os.Args[1:], " "))
	}
	_ = windows.ShellExecute(0, verb, exePtr, argsPtr, cwdPtr, windows.SW_SHOWNORMAL)
}

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(h),
			uintptr(windows.JobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)),
			uintptr(uint32(unsafe.Sizeof(info))),
		)
		hJob = h
	}
}

func initPaths() error {
	p, err := os.Executable()
	if err != nil {
		return err
	}
	fullExeP = p
	baseDir = filepath.Dir(fullExeP)
	coreExe = filepath.Join(baseDir, "mihomo.exe")
	svcExe = filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	iniPath = filepath.Join(baseDir, "mihomo-launcher.ini")
	return nil
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil {
		return false
	}
	self := uint32(os.Getpid())
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) && pe.ProcessID != self {
			return true
		}
		if err := windows.Process32Next(h, &pe); err != nil {
			break
		}
	}
	return false
}

func tryStartCore() {
	startMu.Lock()
	defer startMu.Unlock()

	if time.Since(lastStart) < 5*time.Second || isProcessRunning("mihomo.exe") {
		return
	}
	if _, err := os.Stat(coreExe); os.IsNotExist(err) {
		return
	}

	cmd := exec.Command(coreExe, "-d", baseDir)
	cmd.Dir = baseDir
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}

	if err := cmd.Start(); err == nil {
		lastStart = time.Now()
		if hJob != 0 {
			hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			if hp != 0 {
				_ = windows.AssignProcessToJobObject(hJob, hp)
				_ = windows.CloseHandle(hp)
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cmd.Wait()
		}()
	}
}

func engineKeeper() {
	defer wg.Done()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-mainCtx.Done():
			return
		case <-ticker.C:
			confMu.RLock()
			isSvc := conf.ServiceMode
			confMu.RUnlock()
			if !isSvc {
				resp, err := httpClient.Get(API_URL + "/version")
				if err != nil {
					tryStartCore()
				} else {
					resp.Body.Close()
				}
			}
			syncStateToCore()
		}
	}
}

func syncStateToCore() {
	resp, err := httpClient.Get(API_URL + "/configs")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	data := string(b)

	confMu.Lock()
	defer confMu.Unlock()

	// 解决问题 3：如果 Web 面板开启了 TUN，同步更新本地配置，而不是强制覆盖
	realTun := strings.Contains(data, `"tun":{"enable":true`)
	if conf.TunEnabled != realTun {
		conf.TunEnabled = realTun
		saveIni() 
	}

	if !strings.Contains(data, fmt.Sprintf(`"mode":"%s"`, conf.Mode)) {
		sendPatch(fmt.Sprintf(`{"mode": "%s"}`, conf.Mode))
	}
	if isProxyInReg() != conf.SystemProxy {
		setProxyReg(conf.SystemProxy)
	}
}

func onReady() {
	confMu.RLock()
	systray.SetIcon(getIcon("default.ico"))
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	mDir := systray.AddMenuItem("打开程序目录", "") // 修复：选项回归
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", conf.SystemProxy)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()
	mRule := systray.AddMenuItemCheckbox("规则模式", "", conf.Mode == "rule")
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", conf.Mode == "global")
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", conf.Mode == "direct")
	systray.AddSeparator()
	mSet := systray.AddMenuItem("启动设置", "")
	mAuto := mSet.AddSubMenuItemCheckbox("开机启动", "", conf.AutoStart)
	mInst := mSet.AddSubMenuItem("安装服务", "")
	mUnin := mSet.AddSubMenuItem("卸载服务", "")
	mRes := mSet.AddSubMenuItem("重启内核", "")
	mFull := mSet.AddSubMenuItem("彻底退出程序", "")
	systray.AddSeparator()
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	confMu.RUnlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-mainCtx.Done():
				return
			case <-ticker.C:
				confMu.RLock()
				isSvc, isProxy, isTun := conf.ServiceMode, conf.SystemProxy, conf.TunEnabled
				confMu.RUnlock()
				if isSvc {
					mInst.Disable()
					mUnin.Enable()
				} else {
					mInst.Enable()
					mUnin.Disable()
				}
				// 实时更新菜单勾选状态
				if isProxy { mProxy.Check() } else { mProxy.Uncheck() }
				if isTun { mTun.Check() } else { mTun.Uncheck() }
				refreshIcon()
			}
		}
	}()

	for {
		select {
		case <-mWeb.ClickedCh:
			_ = windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mDir.ClickedCh:
			_ = windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh:
			confMu.Lock()
			conf.SystemProxy = !mProxy.Checked()
			confMu.Unlock()
			saveIni()
		case <-mTun.ClickedCh:
			confMu.Lock()
			conf.TunEnabled = !mTun.Checked()
			confMu.Unlock()
			saveIni()
		case <-mRule.ClickedCh:
			confMu.Lock()
			conf.Mode = "rule"
			confMu.Unlock()
			saveIni()
		case <-mGlobal.ClickedCh:
			confMu.Lock()
			conf.Mode = "global"
			confMu.Unlock()
			saveIni()
		case <-mDirect.ClickedCh:
			confMu.Lock()
			conf.Mode = "direct"
			confMu.Unlock()
			saveIni()
		case <-mAuto.ClickedCh:
			confMu.Lock()
			conf.AutoStart = !mAuto.Checked()
			updateAutoStart(conf.AutoStart)
			confMu.Unlock()
			saveIni()
		case <-mInst.ClickedCh:
			manageService("install")
		case <-mUnin.ClickedCh:
			manageService("uninstall")
		case <-mRes.ClickedCh:
			confMu.RLock()
			isSvc := conf.ServiceMode
			confMu.RUnlock()
			if isSvc {
				manageService("restart")
			} else {
				killProcess("mihomo.exe")
			}
		case <-mFull.ClickedCh:
			cleanExit()
		case <-mHide.ClickedCh:
			confMu.Lock()
			conf.TrayHidden = true
			confMu.Unlock()
			saveIni()
			systray.Quit()
		}
	}
}

func cleanExit() {
	cancel()
	setProxyReg(false)
	confMu.RLock()
	isSvc := conf.ServiceMode
	confMu.RUnlock()

	if isSvc {
		manageService("stop")
	} else {
		killProcess("mihomo.exe")
	}

	if hJob != 0 {
		_ = windows.CloseHandle(hJob)
	}

	wg.Wait()
	os.Exit(0)
}

func manageService(action string) {
	svcDir := filepath.Dir(svcExe)
	switch action {
	case "install":
		_ = runSilent(svcDir, svcExe, "stop")
		_ = runSilent(svcDir, svcExe, "install")
		_ = runSilent(svcDir, svcExe, "start")
	case "uninstall":
		_ = runSilent(svcDir, svcExe, "stop")
		killProcess("mihomo.exe")
		_ = runSilent(svcDir, svcExe, "uninstall")
	default:
		_ = runSilent(svcDir, svcExe, action)
	}
	confMu.Lock()
	conf.ServiceMode = checkServiceRealStatus()
	confMu.Unlock()
	saveIni()
}

func runSilent(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run()
}

func checkServiceRealStatus() bool {
	cmd := exec.Command("sc", "query", SERVICE_NAME)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "SERVICE_NAME")
}

func killProcess(name string) {
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", name)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = cmd.Run()
}

func refreshIcon() {
	resp, err := httpClient.Get(API_URL + "/configs")
	if err != nil {
		systray.SetIcon(getIcon("stop.ico"))
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	s := string(b)

	if strings.Contains(s, `"tun":{"enable":true`) {
		systray.SetIcon(getIcon("tun.ico"))
	} else if isProxyInReg() {
		systray.SetIcon(getIcon("proxy.ico"))
	} else {
		systray.SetIcon(getIcon("default.ico"))
	}
}

func isProxyInReg() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

func setProxyReg(e bool) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if err != nil {
		return
	}
	defer k.Close()
	if e {
		_ = k.SetDWordValue("ProxyEnable", 1)
		_ = k.SetStringValue("ProxyServer", PROXY_ADDR)
	} else {
		_ = k.SetDWordValue("ProxyEnable", 0)
		_ = k.DeleteValue("ProxyServer")
	}
	_, _, _ = windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func updateAutoStart(e bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	if err != nil {
		return
	}
	defer k.Close()
	if e {
		_ = k.SetStringValue("MihomoLauncher", "\""+fullExeP+"\" -silent")
	} else {
		_ = k.DeleteValue("MihomoLauncher")
	}
}

func sendPatch(j string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(j)))
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

func getIcon(n string) []byte {
	d, err := iconFs.ReadFile("icons/" + n)
	if err != nil {
		return nil
	}
	return d
}

func saveIni() {
	f, err := os.Create(iniPath)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[Settings]\nauto_start = %v\ntray_hidden = %v\ntun_enabled = %v\nsystem_proxy = %v\nmode = %s\nservice_mode = %v\n",
		conf.AutoStart, conf.TrayHidden, conf.TunEnabled, conf.SystemProxy, conf.Mode, conf.ServiceMode)
}

func loadIni() {
	conf.Mode = "rule"
	f, err := os.ReadFile(iniPath)
	if err != nil {
		return
	}
	s := string(f)
	conf.AutoStart = strings.Contains(s, "auto_start = true")
	conf.TrayHidden = strings.Contains(s, "tray_hidden = true")
	conf.TunEnabled = strings.Contains(s, "tun_enabled = true")
	conf.SystemProxy = strings.Contains(s, "system_proxy = true")
	if strings.Contains(s, "mode = global") {
		conf.Mode = "global"
	} else if strings.Contains(s, "mode = direct") {
		conf.Mode = "direct"
	}
	conf.ServiceMode = checkServiceRealStatus()
}

func main() {
	if !isAdmin() {
		runAsAdmin()
		return
	}

	if err := initPaths(); err != nil {
		os.Exit(1)
	}
	initJobObject()

	// 单实例检测
	_, err := windows.CreateMutex(nil, false, syscall.StringToUTF16Ptr(APP_MUTEX))
	if err != nil {
		// 解决问题 2：如果已经运行，仅发送唤醒指令并立即静默退出
		conn, err := net.DialTimeout("tcp", IPC_PORT, time.Second)
		if err == nil {
			_, _ = conn.Write([]byte("WAKE_UP_PLZ"))
			conn.Close()
		}
		os.Exit(0) // 关键：此处直接退出，不清理环境，不杀内核
	}

	isSilent := false
	for _, a := range os.Args {
		if a == "-silent" {
			isSilent = true
		}
	}

	confMu.Lock()
	loadIni()
	if !isSilent {
		conf.TrayHidden = false
	}
	confMu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ln, err := net.Listen("tcp", IPC_PORT)
		if err != nil {
			return
		}
		go func() {
			<-mainCtx.Done()
			_ = ln.Close()
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 11)
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := c.Read(buf)
			if n == 11 && string(buf) == "WAKE_UP_PLZ" {
				// 收到唤醒：如果当前是隐藏状态，强制显示托盘
				confMu.RLock()
				hidden := conf.TrayHidden
				confMu.RUnlock()
				if hidden {
					_ = exec.Command(fullExeP).Start() // 启动一个带 UI 的新副本
					cleanExit() // 当前后台副本由于要交接 UI，安全退出
				}
			}
			c.Close()
		}
	}()

	wg.Add(1)
	go engineKeeper()

	confMu.RLock()
	isHidden := conf.TrayHidden
	confMu.RUnlock()

	if isHidden {
		<-mainCtx.Done()
	} else {
		systray.Run(onReady, func() {})
	}
	wg.Wait()
}
