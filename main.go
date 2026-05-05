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
	"syscall"
	"time"

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
	fullExeP, _ = os.Executable()
	baseDir     = filepath.Dir(fullExeP)
	coreExe     = filepath.Join(baseDir, "mihomo.exe")
	svcExe      = filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	iniPath     = filepath.Join(baseDir, "mihomo-launcher.ini")
	ctx, cancel = context.WithCancel(context.Background())
)

func runSilent(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run()
}

func checkServiceRealStatus() bool {
	cmd := exec.Command("sc", "query", SERVICE_NAME)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, _ := cmd.Output()
	return strings.Contains(string(out), "SERVICE_NAME")
}

func manageService(action string) {
	svcDir := filepath.Dir(svcExe)
	if action == "install" {
		_ = runSilent(svcDir, svcExe, "stop")
		_ = runSilent(svcDir, svcExe, "install")
		_ = runSilent(svcDir, svcExe, "start")
	} else if action == "uninstall" {
		_ = runSilent(svcDir, svcExe, "stop")
		killProcess("mihomo.exe")
		_ = runSilent(svcDir, svcExe, "uninstall")
	} else {
		_ = runSilent(svcDir, svcExe, action)
	}
	conf.ServiceMode = checkServiceRealStatus()
	saveIni()
}

func killProcess(name string) {
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", name)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = cmd.Run()
}

func engineKeeper() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !conf.ServiceMode {
				resp, err := http.Get(API_URL + "/version")
				if err != nil {
					cmd := exec.Command(coreExe, "-d", baseDir)
					cmd.Dir = baseDir
					cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
					_ = cmd.Start()
				} else {
					resp.Body.Close()
				}
			}
			syncStateToCore()
		}
	}
}

func syncStateToCore() {
	resp, err := http.Get(API_URL + "/configs")
	if err != nil { return }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	data := string(b)

	if conf.TunEnabled != strings.Contains(data, `"tun":{"enable":true`) {
		sendPatch(fmt.Sprintf(`{"tun": {"enable": %v}}`, conf.TunEnabled))
	}
	if !strings.Contains(data, fmt.Sprintf(`"mode":"%s"`, conf.Mode)) {
		sendPatch(fmt.Sprintf(`{"mode": "%s"}`, conf.Mode))
	}
	if isProxyInReg() != conf.SystemProxy {
		setProxyReg(conf.SystemProxy)
	}
}

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
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if conf.ServiceMode { mInst.Disable(); mUnin.Enable() } else { mInst.Enable(); mUnin.Disable() }
				refreshIcon(mProxy, mTun, mRule, mGlobal, mDirect)
			}
		}
	}()

	for {
		select {
		case <-mWeb.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh: conf.SystemProxy = !mProxy.Checked(); saveIni()
		case <-mTun.ClickedCh: conf.TunEnabled = !mTun.Checked(); saveIni()
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
			cancel()
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

	if strings.Contains(s, `"mode":"rule"`) { mR.Check(); mG.Uncheck(); mD.Uncheck()
	} else if strings.Contains(s, `"mode":"global"`) { mR.Uncheck(); mG.Check(); mD.Uncheck()
	} else if strings.Contains(s, `"mode":"direct"`) { mR.Uncheck(); mG.Uncheck(); mD.Check() }

	if strings.Contains(s, `"tun":{"enable":true`) {
		systray.SetIcon(getIcon("tun.ico")); mT.Check()
	} else if isProxyInReg() {
		systray.SetIcon(getIcon("proxy.ico")); mP.Check(); mT.Uncheck()
	} else {
		systray.SetIcon(getIcon("default.ico")); mP.Uncheck(); mT.Uncheck()
	}
}

func isProxyInReg() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil { return false }
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

func setProxyReg(e bool) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if err != nil { return }
	defer k.Close()
	if e {
		_ = k.SetDWordValue("ProxyEnable", 1)
		_ = k.SetStringValue("ProxyServer", PROXY_ADDR)
	} else {
		_ = k.SetDWordValue("ProxyEnable", 0)
		_ = k.DeleteValue("ProxyServer") // 彻底清除
	}
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func updateAutoStart(e bool) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	if e { _ = k.SetStringValue("MihomoLauncher", "\""+fullExeP+"\" -silent") } else { _ = k.DeleteValue("MihomoLauncher") }
	k.Close()
}

func sendPatch(j string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(j)))
	client := &http.Client{Timeout: time.Second}
	if resp, err := client.Do(req); err == nil { resp.Body.Close() }
}

func getIcon(n string) []byte {
	d, err := iconFs.ReadFile("icons/" + n)
	if err != nil { return nil }
	return d
}

func saveIni() {
	f, err := os.Create(iniPath)
	if err != nil { return }
	defer f.Close()
	fmt.Fprintf(f, "[Settings]\nauto_start = %v\ntray_hidden = %v\ntun_enabled = %v\nsystem_proxy = %v\nmode = %s\nservice_mode = %v\n",
		conf.AutoStart, conf.TrayHidden, conf.TunEnabled, conf.SystemProxy, conf.Mode, conf.ServiceMode)
}

func loadIni() {
	conf.Mode = "rule"
	f, err := os.ReadFile(iniPath)
	if err != nil { saveIni(); return }
	s := string(f)
	conf.AutoStart = strings.Contains(s, "auto_start = true")
	conf.TrayHidden = strings.Contains(s, "tray_hidden = true")
	conf.TunEnabled = strings.Contains(s, "tun_enabled = true")
	conf.SystemProxy = strings.Contains(s, "system_proxy = true")
	if strings.Contains(s, "mode = global") { conf.Mode = "global" } else if strings.Contains(s, "mode = direct") { conf.Mode = "direct" }
	conf.ServiceMode = checkServiceRealStatus()
}

func main() {
	isSilent := false
	for _, a := range os.Args { if a == "-silent" { isSilent = true } }
	loadIni()
	if !isSilent { conf.TrayHidden = false }

	ln, err := net.Listen("tcp", IPC_PORT)
	if err != nil {
		conn, err := net.DialTimeout("tcp", IPC_PORT, time.Second)
		if err == nil {
			_, _ = conn.Write([]byte("WAKE_UP_PLZ"))
			conn.Close()
		}
		os.Exit(0)
	}

	go func() {
		defer ln.Close()
		for {
			c, err := ln.Accept()
			if err != nil { return }
			buf := make([]byte, 11)
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := c.Read(buf)
			if n == 11 && string(buf) == "WAKE_UP_PLZ" {
				_ = exec.Command(fullExeP).Start()
				cancel()
				os.Exit(0)
			}
			c.Close()
		}
	}()

	go engineKeeper()

	if conf.TrayHidden {
		select {}
	} else {
		systray.Run(onReady, func() {
			if conf.TrayHidden { select {} }
		})
	}
}
