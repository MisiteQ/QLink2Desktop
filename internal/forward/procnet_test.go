package forward

import "testing"

// 真实 /proc/net/tcp 的片段：列宽、表头、十六进制端口、末尾的统计列
// 全部按内核原样抄。这种"照抄真实格式"的样本是必须的——
// 一旦按想象里的格式写样本，解析函数就只是在测自己。
const realProcNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 31453 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 31454 1 0000000000000000 100 0 0 10 0
   2: 0100007F:8AE2 0100007F:1F90 01 00000000:00000000 00:00000000 00000000     0        0 31500 1 0000000000000000 20 4 30 10 -1
`

const realProcNetUDP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
  512: 00000000:14E9 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 41234 2 0000000000000000 0
`

func TestParseListenInodesMatchesHexPort(t *testing.T) {
	got := parseListenInodes(realProcNetTCP, 8080, "tcp") // 8080 = 0x1F90
	if len(got) != 1 || got[0] != "31453" {
		t.Fatalf("8080 端口的 inode = %v，期望 [31453]", got)
	}
}

func TestParseListenInodesUsesLocalPortNotRemote(t *testing.T) {
	// 第 3 行的 remote_address 也是 1F90，但 st=01（已建立），且它的
	// local_address 是 8AE2 —— 命中它就说明我们比错了字段。
	if got := parseListenInodes(realProcNetTCP, 8080, "tcp"); len(got) != 1 {
		t.Fatalf("把已建立连接的对端端口当成了监听，命中 %v", got)
	}
	if got := parseListenInodes(realProcNetTCP, 35554, "tcp"); len(got) != 0 { // 0x8AE2
		t.Fatalf("非 LISTEN 状态不该被算作占用，却命中 %v", got)
	}
}

func TestParseListenInodesSkipsEstablished(t *testing.T) {
	// 53 = 0x35，第 2 行是 LISTEN。
	if got := parseListenInodes(realProcNetTCP, 53, "tcp"); len(got) != 1 || got[0] != "31454" {
		t.Fatalf("53 端口 inode = %v，期望 [31454]", got)
	}
}

func TestParseListenInodesUDPStateIsSeven(t *testing.T) {
	if got := parseListenInodes(realProcNetUDP, 5353, "udp"); len(got) != 1 || got[0] != "41234" {
		t.Fatalf("UDP 5353（0x14E9）inode = %v，期望 [41234]", got)
	}
	// 同样的内容用 TCP 规则解析应当找不到：状态码体系不同（TCP 用 0A）。
	if got := parseListenInodes(realProcNetUDP, 5353, "tcp"); len(got) != 0 {
		t.Fatalf("UDP 的表不该被 TCP 解析器命中，却得到 %v", got)
	}
}

func TestParseListenInodesIgnoresHeaderAndGarbage(t *testing.T) {
	junk := "  sl  local_address rem_address   st tx_queue rx_queue\n" +
		"garbage\n" +
		"   0: 00000000:1F90 00000000:0000 0A\n" // 被截断，列数不足
	got := parseListenInodes(junk, 8080, "tcp")
	if len(got) != 0 {
		t.Fatalf("表头 / 残缺行不该被当成占用记录，却得到 %v", got)
	}
}

func TestSocketInodeOfLink(t *testing.T) {
	cases := []struct {
		link  string
		inode string
		ok    bool
	}{
		{"socket:[31453]", "31453", true},
		{"socket:[]", "", false},
		{"anon_inode:[eventfd]", "", false},
		{"pipe:[12345]", "", false},
		{"/dev/null", "", false},
		{"/var/lib/data.json", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := socketInodeOfLink(c.link)
		if ok != c.ok || got != c.inode {
			t.Errorf("socketInodeOfLink(%q) = (%q, %v)，期望 (%q, %v)",
				c.link, got, ok, c.inode, c.ok)
		}
	}
}
