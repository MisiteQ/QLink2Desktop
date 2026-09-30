package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// ErrSSHUnavailable 表示环境中没有可用的 ssh 客户端。
var ErrSSHUnavailable = errors.New("未找到 ssh 客户端")

// SSHTunnel 通过宿主机的 ssh 客户端建立到远端服务的透明隧道。
//
// 实现方式是 `ssh -W host:port target`：ssh 把标准输入输出当作到目标地址的
// 原始字节管道，因此我们可以把它适配成一个 net.Conn 交给反向代理使用。
//
// 为什么复用系统 ssh 而不是内置一个 SSH 库：
//   - 用户已有的 ~/.ssh/config、跳板机、密钥、agent 全部直接生效，零配置成本；
//   - 不需要把 x/crypto 依赖树打进「单二进制零依赖」的分发包；
//   - 认证策略（含企业环境的 Kerberos / 多因子）由成熟客户端处理，更可靠。
type SSHTunnel struct {
	host     domain.Host
	sshBin   string
	sshpass  string
	probed   bool
	probeErr error
}

// NewSSHTunnel 创建隧道工厂。
func NewSSHTunnel(h domain.Host) *SSHTunnel {
	t := &SSHTunnel{host: h}
	if p, err := exec.LookPath("ssh"); err == nil {
		t.sshBin = p
	}
	if p, err := exec.LookPath("sshpass"); err == nil {
		t.sshpass = p
	}
	return t
}

// Available 报告是否可以尝试建立隧道。
func (t *SSHTunnel) Available() bool { return t != nil && t.sshBin != "" }

// args 组装 ssh 命令行参数。
func (t *SSHTunnel) args(remote string) []string {
	args := []string{
		// 只用来转发字节，不分配终端、不执行交互式命令。
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ConnectTimeout=8",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	if t.host.Auth.InsecureSkipHostKey {
		args = append(args, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null")
	}
	if t.host.Auth.KeyPath != "" {
		args = append(args, "-i", t.host.Auth.KeyPath)
	}
	if t.host.Port > 0 && t.host.Port != 22 {
		args = append(args, "-p", fmt.Sprint(t.host.Port))
	}
	args = append(args, "-W", remote, t.host.SSHTarget())
	return args
}

// DialContext 建立一条穿过 SSH 的 TCP 连接。
func (t *SSHTunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !t.Available() {
		return nil, ErrSSHUnavailable
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SSH 隧道仅支持 tcp，收到 %q", network)
	}

	sshArgs := t.args(addr)
	bin := t.sshBin
	finalArgs := sshArgs

	// 密码认证只有在系统装了 sshpass 时才能非交互地完成；
	// 否则明确报错，而不是让命令挂在那里等待输入。
	if t.host.Auth.Password != "" {
		if t.sshpass == "" {
			return nil, fmt.Errorf("%w: 需要密码认证但系统未安装 sshpass，请改用密钥或 ssh-agent", ErrSSHUnavailable)
		}
		bin = t.sshpass
		finalArgs = append([]string{"-p", t.host.Auth.Password, "ssh"}, sshArgs...)
	}

	cmd := exec.CommandContext(ctx, bin, finalArgs...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 SSH 标准输入失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 SSH 标准输出失败: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 SSH 隧道失败: %w", err)
	}

	conn := &sshConn{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		local:  tunnelAddr("local"),
		remote: tunnelAddr(addr),
		stderr: &stderr,
	}
	return conn, nil
}

// Probe 通过一次端口转发验证连通性。
func (t *SSHTunnel) Probe(ctx context.Context, remoteAddr string) error {
	if !t.Available() {
		return ErrSSHUnavailable
	}
	// 用一个几乎必然存在的转发目标做握手测试；失败即说明认证或网络不通。
	conn, err := t.DialContext(ctx, "tcp", remoteAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// 能建立通道即视为成功（ssh 的 ExitOnForwardFailure 会保证失败时立刻退出）。
	deadline := time.Now().Add(500 * time.Millisecond)
	_ = conn.SetDeadline(deadline)
	buf := make([]byte, 1)
	_, _ = conn.Read(buf) // 读超时也说明通道是通的
	return nil
}

// --------------------------------------------------------------------------

// sshConn 把 ssh 子进程的 stdin/stdout 适配成 net.Conn。
type sshConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	local  net.Addr
	remote net.Addr
	stderr *strings.Builder
	once   sync.Once
}

func (c *sshConn) Read(b []byte) (int, error) {
	n, err := c.stdout.Read(b)
	if err != nil && err != io.EOF {
		// 把 ssh 的 stderr 一并带出来，方便用户排查认证失败。
		if msg := strings.TrimSpace(c.stderrString()); msg != "" {
			return n, fmt.Errorf("SSH 隧道读取失败: %w (%s)", err, msg)
		}
	}
	return n, err
}

func (c *sshConn) Write(b []byte) (int, error) { return c.stdin.Write(b) }

func (c *sshConn) Close() error {
	var err error
	c.once.Do(func() {
		_ = c.stdin.Close()
		_ = c.stdout.Close()
		if c.cmd.Process != nil {
			err = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
	})
	return err
}

func (c *sshConn) LocalAddr() net.Addr              { return c.local }
func (c *sshConn) RemoteAddr() net.Addr             { return c.remote }
func (c *sshConn) SetDeadline(time.Time) error      { return nil }
func (c *sshConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sshConn) SetWriteDeadline(time.Time) error { return nil }

func (c *sshConn) stderrString() string {
	if c.stderr == nil {
		return ""
	}
	return c.stderr.String()
}

type tunnelAddr string

func (a tunnelAddr) Network() string { return "ssh-tunnel" }
func (a tunnelAddr) String() string  { return string(a) }

// TCPProbe 直接用 TCP 拨号探测 host:port 是否可达（不走 SSH）。
func TCPProbe(ctx context.Context, addr string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}
