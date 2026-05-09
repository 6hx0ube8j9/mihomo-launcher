func toggleAutoStart(enable bool) {
	const taskName = "MihomoLauncherTask"
	
	// 1. 清理旧的注册表启动项（保持环境纯净）
	if key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE); err == nil {
		_ = key.DeleteValue(APP_NAME)
		key.Close()
	}

	success := false

	if enable {
		// 这里的路径处理保持你原有的逻辑
		cleanExe := strings.ReplaceAll(exePath, "'", "''")
		cleanDir := strings.ReplaceAll(baseDir, "'", "''")
		
		// 优化后的 PowerShell 脚本：
		// 1. 直接创建触发器并设置 5 秒延迟 (PT5S)
		// 2. 一步到位注册任务，包含工作目录、权限、电池策略
		psScript := fmt.Sprintf(
			"$trigger = New-ScheduledTaskTrigger -AtLogOn; "+
			"$trigger.Delay = 'PT5S'; "+
			"$action = New-ScheduledTaskAction -Execute '%s' -WorkingDirectory '%s'; "+
			"$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); "+
			"Register-ScheduledTask -TaskName '%s' -Trigger $trigger -Action $action -Settings $settings -User $env:USERNAME -RunLevel Highest -Force",
			cleanExe, cleanDir, taskName,
		)
		
		modifyCmd := exec.Command("powershell", "-Command", psScript)
		modifyCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		
		if err := modifyCmd.Run(); err == nil {
			success = true 
		}
	} else {
		// C. 删除任务计划
		deleteCmd := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F")
		deleteCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := deleteCmd.Run(); err == nil || !checkAutoStartStatus() {
			success = true
		}
	}

	if success {
		saveIniConfig("startup_enabled", fmt.Sprint(enable))
		fmt.Printf("[AutoStart] 状态已成功同步为: %v\n", enable)
	} else {
		fmt.Printf("[AutoStart] 操作失败，请检查是否被安全软件拦截")
	}
}
