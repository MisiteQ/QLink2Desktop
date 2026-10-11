package forward

// PortProbe 是一次「这个端口能不能用」的探测结果。
//
// 存在的意义：绑定失败时的报错是**事后**的，而且只在真正启动那一刻才出现。
// 用户想用的端口（比如 18080）被占着时，他需要的是**事先**就知道
// "被谁占着、能不能想办法"，而不是等保存完再看一条失败提示。
type PortProbe struct {
	// Available 为 true 表示本应用可以绑定这个端口。
	Available bool `json:"available"`
	// Mode 是实际能绑上的形态（dual / ipv4 / ipv6），未绑上时为空。
	//
	// 必须返回它：探测走的是与真实启动同一条逻辑（含自动退让），
	// 所以"能监听"可能只是"退到单栈后能监听"。只说 available
	// 会让用户以为两侧都能用，实际上一侧的访客根本连不上。
	Mode string `json:"mode,omitempty"`
	// Degraded 表示只能退化为单栈监听（另一侧被占用）。
	Degraded bool `json:"degraded,omitempty"`
	// Protocol 是探测的协议（tcp / udp）。
	Protocol string `json:"protocol"`
	// Port 是探测的端口。
	Port int `json:"port"`
	// Error 是绑定失败的原因，已翻译成用户可读文案。
	Error string `json:"error,omitempty"`
	// Holders 是查到的占用者（非 Linux 或查不到时为空）。
	Holders []Holder `json:"holders,omitempty"`
	// Self 表示占用者中包含本应用自己。
	Self bool `json:"self,omitempty"`
}

// ProbePort 探测某个监听配置能否绑定，并尽量查出占用者。
//
// 它会真的尝试绑定一次再立刻释放：这是唯一可靠的判定方式 ——
// 只看进程列表无法覆盖"协议栈某一侧被占"这类情况，
// 而那恰恰是本项目最容易踩的一种（见 listen.go 的说明）。
// 占用窗口在毫秒级，不会影响正在转发的连接。
func ProbePort(host string, port int, proto string) PortProbe {
	res := PortProbe{Port: port, Protocol: proto}

	if proto == "udp" {
		conn, mode, err := listenUDP(host, port)
		if err == nil {
			_ = conn.Close()
			res.Available = true
			res.Mode = string(mode)
			res.Degraded = mode.degraded()
			return res
		}
		res.Error = describeListenErr(err, port, proto)
	} else {
		ln, mode, err := listenTCP(host, port)
		if err == nil {
			_ = ln.Close()
			res.Available = true
			res.Mode = string(mode)
			res.Degraded = mode.degraded()
			return res
		}
		res.Error = describeListenErr(err, port, proto)
	}

	res.Holders = whoHoldsPort(port, proto)
	for _, h := range res.Holders {
		if h.Self {
			res.Self = true
		}
	}
	return res
}
