func monitorIconState() {
	var (
		failCount   int
		curr        int
		lastState   int
		ifaces      []net.Interface
		err         error
		hasTun      bool
		isTunMode   bool // 【修复：在此处预先声明 isTunMode】
	)

	for {
		if atomic.LoadInt32(&isReallyExiting) == 1 {
			return
		}

		// --- 1. 物理进程判定 (最高优先级) ---
		if !isProcessRunning("mihomo.exe") {
			failCount = 0
			if lastState != StateStop {
				updateIconByState(StateStop)
				lastState = StateStop
			}
			goto LoopEnd // 只要后面没有跳过变量声明，goto 就是安全的
		}

		// --- 2. 获取业务与网卡事实 ---
		curr = checkSystemState()
		
		// 使用赋值 (=) 而不是声明 (:=)
		isTunMode = (getIniConfig("tun_enabled") == "true")
		hasTun = false 
		
		ifaces, err = net.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				if isTunInterfaceMatch(i.Name) {
					if (i.Flags & net.FlagUp) != 0 {
						hasTun = true
						break
					}
				}
			}
		}

		// --- 3. 核心判定逻辑 ---
		if (isTunMode && !hasTun) || curr == StateStop {
			failCount++

			if failCount <= 5 {
				// 机会期：显示业务降级 (Proxy/Default)
				backState := curr
				if backState == StateStop {
					if getIniConfig("system_proxy_enabled") == "true" {
						backState = StateProxy
					} else {
						backState = StateDefault
					}
				}
				if lastState != backState {
					updateIconByState(backState)
					lastState = backState
				}
			} else {
				// 机会期满：判定 Error 或压倒性 Stop
				if curr != StateStop && isTunMode && !hasTun {
					if lastState != StateError {
						updateIconByState(StateError)
						lastState = StateError
					}
				} else {
					if lastState != StateStop {
						updateIconByState(StateStop)
						lastState = StateStop
					}
				}
			}
		} else {
			// 一切正常
			failCount = 0
			if curr != lastState {
				updateIconByState(curr)
				lastState = curr
			}
		}

	LoopEnd:
		time.Sleep(1 * time.Second)
	}
}
