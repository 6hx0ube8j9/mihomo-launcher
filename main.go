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

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	API_URL    = "http://127.0.0.1:9090"
	PROXY_ADDR = "127.0.0.1:7890"
	IPC_PORT   = "127.0.0.1:54321"
	SERVICE_NAME = "Mihomo"
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
	conf       Config
	exePath, _ = filepath.Abs(os.Args[0])
	baseDir    = filepath.Dir(exePath)
	coreExe    = filepath.Join(baseDir, "mihomo.exe")
	svcExe     = filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	iniPath    = filepath.Join(baseDir, "mihomo-launcher.ini")
)

// --- 基础工具：静默执行指令 ---

func runSilent(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run()
}

// --- 服务检测与操作 ---

func checkServiceRealStatus() bool {
	// 静默检测服务是否存在
	cmd := exec.Command("sc", "query", SERVICE_NAME)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, _ := cmd.Output()
	return strings.Contains(string(out), "SERVICE_NAME")
}

func manageService(action string) {
	// 安装：stop -> install -> start
	if action == "install" {
		runSilent(filepath.Dir(svcExe), svcExe, "stop")
		runSilent(filepath.Dir(svcExe), svcExe, "install")
		runSilent(filepath.Dir(svcExe), svcExe, "start")
	} else if action == "uninstall" {
		// 卸载：stop -> kill -> uninstall
		runSilent(filepath.Dir(svcExe), svcExe, "stop")
		killProcess("mihomo.exe")
		runSilent(filepath.Dir(svcExe), svcExe, "uninstall")
	} else {
		runSilent(filepath.Dir(svcExe), svcExe, action)
	}
	// 执行完操作后，根据实际情况更新配置并写入
	conf.ServiceMode = checkServiceRealStatus()
	saveIni()
}

func killProcess(name string) {
	// 原生静默杀进程，替代 taskkill
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", name)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.Run()
}

// --- 内核拉起逻辑 (路径锁定) ---

func engineKeeper() {
	for {
		if !conf.ServiceMode {
			resp, err := http.Get(API_URL + "/version")
			if err != nil {
				// 强制在根目录拉起内核
				cmd := exec.Command(coreExe, "-d", baseDir)
				cmd.Dir = baseDir
				cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
				cmd.Start()
			} else {
				resp.Body.Close()
			}
		}
		syncStateToCore()
		time.Sleep(3 * time.Second)
	}
}

// --- 状态同步：将 .ini 意图下发给内核 ---

func syncStateToCore() {
	resp, err := http.Get(API_URL + "/configs")
	if err != nil { return }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	data := string(b)

	// 同步 TUN
	isTunOn := strings.Contains(data, `"tun":{"enable":true`)
	if conf.TunEnabled != isTunOn {
		sendPatch(fmt.Sprintf(`{"tun": {"enable": %v}}`, conf.TunEnabled))
	}
	// 同步 Mode
	if !strings.Contains(data, fmt.Sprintf(`"mode":"%s"`, conf.Mode)) {
		sendPatch(fmt.Sprintf(`{"mode": "%s"}`, conf.Mode))
	}
	// 同步系统代理
	if isProxyInReg() != conf.SystemProxy {
		setProxyReg(conf.SystemProxy)
	}
}

// --- 托盘 UI ---

func onReady() {
	systray.SetIcon(getIcon("default.ico"))
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
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

	go func() {
		for {
			// 刷新服务按钮状态
			if conf.ServiceMode { mI, mU := mInst, mUnin; mI.Disable(); mU.Enable() } else { mInst.Enable(); mUnin.Disable() }
			refreshIcon(mProxy, mTun, mRule, mGlobal, mDirect)
			time.Sleep(2 * time.Second)
		}
	}()

	for {
		select {
		case <-mWeb.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh: 
			conf.SystemProxy = !mProxy.Checked()
			saveIni()
		case <-mTun.ClickedCh:
			conf.TunEnabled = !mTun.Checked()
			saveIni()
		case <-mRule.ClickedCh: conf.Mode = "rule"; saveIni()
		case <-mGlobal.ClickedCh: conf.Mode = "global"; saveIni()
		case <-mDirect.ClickedCh: conf.Mode = "direct"; saveIni()
		case <-mAuto.ClickedCh:
			conf.AutoStart = !mAuto.Checked()
			updateAutoStart(conf.AutoStart)
			saveIni()
		case <-mInst.ClickedCh: manageService("install")
		case <-mUnin.ClickedCh: manageService("uninstall")
		case <-mRes.ClickedCh:
			if conf.ServiceMode { manageService("restart") } else { killProcess("mihomo.exe") }
		case <-mFull.ClickedCh:
			setProxyReg(false)
			if conf.ServiceMode { manageService("stop") } else { killProcess("mihomo.exe") }
			os.Exit(0)
		case <-mHide.ClickedCh:
			conf.TrayHidden = true
			saveIni()
			systray.Quit()
		}
	}
}

func refreshIcon(mP, mT, mR, mG, mD *systray.MenuItem) {
	resp, err := http.Get(API_URL + "/configs")
	if err != nil { systray.SetIcon(getIcon("stop.ico")); return }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	s := string(b)

	if strings.Contains(s, `"mode":"rule"`) { mR.Check(); mG.Uncheck(); mD.Uncheck() }
	if strings.Contains(s, `"mode":"global"`) { mR.Uncheck(); mG.Check(); mD.Uncheck() }
	if strings.Contains(s, `"mode":"direct"`) { mR.Uncheck(); mG.Uncheck(); mD.Check() }

	if strings.Contains(s, `"tun":{"enable":true`) {
		systray.SetIcon(getIcon("tun.ico")); mT.Check()
	} else if isProxyInReg() {
		systray.SetIcon(getIcon("proxy.ico")); mP.Check()
	} else {
		systray.SetIcon(getIcon("default.ico")); mP.Uncheck(); mT.Uncheck()
	}
}

// --- 注册表与配置 ---

func isProxyInReg() bool {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

func setProxyReg(e bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if e { k.SetDWordValue("ProxyEnable", 1); k.SetStringValue("ProxyServer", PROXY_ADDR) } else { k.SetDWordValue("ProxyEnable", 0) }
	k.Close()
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0,0,0,0)
}

func updateAutoStart(e bool) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	if e { k.SetStringValue("MihomoLauncher", "\""+exePath+"\" -silent") } else { k.DeleteValue("MihomoLauncher") }
}

func sendPatch(j string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(j)))
	(&http.Client{Timeout: time.Second}).Do(req)
}

func getIcon(n string) []byte { d, _ := iconFs.ReadFile("icons/" + n); return d }

func saveIni() {
	f, _ := os.Create(iniPath)
	defer f.Close()
	fmt.Fprintf(f, "[Settings]\nauto_start = %v\ntray_hidden = %v\ntun_enabled = %v\nsystem_proxy = %v\nmode = %s\nservice_mode = %v\n", 
		conf.AutoStart, conf.TrayHidden, conf.TunEnabled, conf.SystemProxy, conf.Mode, conf.ServiceMode)
}

func loadIni() {
	conf.Mode = "rule" // 默认值
	f, err := os.ReadFile(iniPath)
	if err != nil { saveIni(); return }
	s := string(f)
	conf.AutoStart = strings.Contains(s, "auto_start = true")
	conf.TrayHidden = strings.Contains(s, "tray_hidden = true")
	conf.TunEnabled = strings.Contains(s, "tun_enabled = true")
	conf.SystemProxy = strings.Contains(s, "system_proxy = true")
	if strings.Contains(s, "mode = global") { conf.Mode = "global" }
	if strings.Contains(s, "mode = direct") { conf.Mode = "direct" }
	// 启动时真实检测服务，修正 .ini 里的 service_mode
	conf.ServiceMode = checkServiceRealStatus()
}

func main() {
	isSilent := false
	for _, a := range os.Args { if a == "-silent" { isSilent = true } }
	
	loadIni()
	if !isSilent { conf.TrayHidden = false }

	go func() {
		ln, err := net.Listen("tcp", IPC_PORT)
		if err != nil {
			conn, _ := net.Dial("tcp", IPC_PORT)
			if conn != nil { conn.Write([]byte("RESHOW")); conn.Close() }
			os.Exit(0)
		}
		for {
			c, _ := ln.Accept()
			b := make([]byte, 10); n, _ := c.Read(b)
			if string(b[:n]) == "RESHOW" {
				exec.Command(exePath).Start()
				os.Exit(0)
			}
			c.Close()
		}
	}()

	go engineKeeper()

	if conf.TrayHidden {
		select {}
	} else {
		systray.Run(onReady, func() {})
	}
}
