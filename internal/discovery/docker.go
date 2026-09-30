package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 默认的 Docker 守护进程套接字。
const DefaultDockerSocket = "/var/run/docker.sock"

// labelPrefixes 是会被识别为「桌面图标配置」的标签前缀。
//
// 同时支持 watchcow 的键名，是为了让从 Watchcow 迁移过来的用户
// 不需要改动任何 compose 文件就能一键把服务放到桌面。
var labelPrefixes = []string{"watchcow", "watchcow.", "qlink2d.", "qlink."}

// labelKeys 把各种可能的后缀归一到标准字段名。
var labelKeys = map[string]string{
	"enable":    "enable",
	"enabled":   "enable",
	"title":     "title",
	"name":      "title",
	"icon":      "icon",
	"port":      "port",
	"scheme":    "scheme",
	"protocol":  "scheme",
	"path":      "path",
	"allusers":  "allusers",
	"all_users": "allusers",
	"hide":      "hide",
	"nodisplay": "hide",
}

// ContainerSnapshot 是容器信息的只读快照。
type ContainerSnapshot struct {
	ID      string
	Name    string
	Image   string
	Service string
	Status  string
	State   string
	Ports   []PortMapping
	Labels  map[string]string
}

// PortMapping 是一条端口映射。
type PortMapping struct {
	Public  int
	Private int
	Type    string // tcp / udp
}

// DockerClient 直接通过守护进程套接字访问 Docker API。
//
// 刻意不引入官方 SDK：本项目只需要两个只读接口
// （容器列表、容器详情），用标准库发 HTTP 请求即可，
// 而引入 SDK 会把一个几十 MB 的依赖树带进「单二进制零依赖」的分发形态里。
type DockerClient struct {
	socketPath string
	client     *http.Client
	ttl        time.Duration
	now        func() time.Time

	mu         sync.Mutex
	cache      []ContainerSnapshot
	cachedAt   time.Time
	available  bool
	probedOnce bool
}

// NewDockerClient 创建客户端。socketPath 为空时使用默认路径。
func NewDockerClient(socketPath string) *DockerClient {
	if strings.TrimSpace(socketPath) == "" {
		socketPath = DefaultDockerSocket
	}
	c := &DockerClient{
		socketPath: socketPath,
		ttl:        8 * time.Second,
		now:        time.Now,
	}
	c.client = &http.Client{
		Timeout: 4 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
			MaxIdleConns:    2,
			IdleConnTimeout: 60 * time.Second,
		},
	}
	return c
}

// Available 报告守护进程是否可达（结果缓存，避免每次刷新都探测）。
func (c *DockerClient) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probedOnce {
		return c.available
	}
	c.probedOnce = true

	if _, err := net.DialTimeout("unix", c.socketPath, 500*time.Millisecond); err != nil {
		c.available = false
		return false
	}
	c.available = true
	return true
}

// SocketPath 返回使用的套接字路径。
func (c *DockerClient) SocketPath() string { return c.socketPath }

type dockerContainerRaw struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		IP          string `json:"IP"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

// Containers 返回容器快照（带缓存）。
func (c *DockerClient) Containers(force bool) []ContainerSnapshot {
	if !c.Available() {
		return nil
	}

	c.mu.Lock()
	if !force && c.cache != nil && c.now().Sub(c.cachedAt) < c.ttl {
		out := c.cache
		c.mu.Unlock()
		return out
	}
	c.mu.Unlock()

	body, err := c.get("/containers/json")
	if err != nil {
		return nil
	}

	var raw []dockerContainerRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}

	out := make([]ContainerSnapshot, 0, len(raw))
	for _, r := range raw {
		name := ""
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		snap := ContainerSnapshot{
			ID:      r.ID,
			Name:    name,
			Image:   r.Image,
			State:   r.State,
			Status:  r.Status,
			Labels:  r.Labels,
			Service: r.Labels["com.docker.compose.service"],
		}
		for _, p := range r.Ports {
			if p.PublicPort <= 0 {
				continue
			}
			snap.Ports = append(snap.Ports, PortMapping{
				Public:  p.PublicPort,
				Private: p.PrivatePort,
				Type:    strings.ToLower(p.Type),
			})
		}
		out = append(out, snap)
	}

	c.mu.Lock()
	c.cache = out
	c.cachedAt = c.now()
	c.mu.Unlock()
	return out
}

// Invalidate 清空缓存。
func (c *DockerClient) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = nil
	c.available = false
	c.probedOnce = false
}

func (c *DockerClient) get(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker API %s 返回 %d", path, resp.StatusCode)
	}

	// 容器列表通常只有几十 KB，限流到 8MB 足够且能防异常。
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			if len(buf)+n > 8<<20 {
				return nil, fmt.Errorf("docker API %s 响应过大", path)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

// Enrich 就地补全端口列表里的容器信息。
func (c *DockerClient) Enrich(ports []PortInfo) {
	containers := c.Containers(false)
	if len(containers) == 0 {
		return
	}

	// 建立 公网端口/协议 → 容器 索引。
	type key struct {
		port  int
		proto string
	}
	index := make(map[key]*ContainerSnapshot, len(containers)*2)
	for i := range containers {
		for _, pm := range containers[i].Ports {
			index[key{pm.Public, pm.Type}] = &containers[i]
		}
	}

	for i := range ports {
		k := key{ports[i].Port, string(ports[i].Proto)}
		snap, ok := index[k]
		if !ok {
			// 容器可能声明了 tcp 而内核表里是 tcp6，做一次宽松匹配。
			for _, alt := range []string{"tcp", "udp"} {
				if s, found := index[key{ports[i].Port, alt}]; found {
					snap, ok = s, true
					break
				}
			}
		}
		if !ok {
			continue
		}

		ports[i].Container = &ContainerInfo{
			ID:      shortID(snap.ID),
			Name:    snap.Name,
			Image:   snap.Image,
			Service: snap.Service,
			Status:  snap.Status,
		}
		for _, pm := range snap.Ports {
			if pm.Public == ports[i].Port {
				ports[i].Container.PrivatePort = pm.Private
				break
			}
		}
		if hint, ok := ExtractLabelHint(snap.Labels, snap.Name); ok {
			ports[i].LabelHint = &hint
		}
	}
}

// LabelHints 返回所有带桌面配置标签的容器（用于「一键启用」列表）。
func (c *DockerClient) LabelHints() []LabelHint {
	containers := c.Containers(false)
	var out []LabelHint
	seen := map[string]bool{}
	for _, snap := range containers {
		hint, ok := ExtractLabelHint(snap.Labels, snap.Name)
		if !ok || seen[hint.Key()] {
			continue
		}
		seen[hint.Key()] = true
		out = append(out, hint)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// ExtractLabelHint 从容器标签里提取桌面图标配置。
//
// 支持多种前缀与多种键名拼写（enable/enabled、title/name、scheme/protocol…），
// 目的是最大化对既有 compose 文件的兼容性 —— 用户不需要为了用本应用去改容器配置。
func ExtractLabelHint(labels map[string]string, containerName string) (LabelHint, bool) {
	if len(labels) == 0 {
		return LabelHint{}, false
	}

	values := map[string]string{}
	for rawKey, rawVal := range labels {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		prefix := ""
		for _, p := range labelPrefixes {
			if strings.HasPrefix(key, p) {
				prefix = p
				break
			}
		}
		if prefix == "" {
			continue
		}
		suffix := strings.TrimPrefix(strings.TrimPrefix(key, prefix), ".")
		// 裸前缀键（例如 compose 里直接写 `watchcow: "1"`）等价于声明启用，
		// 这是 Watchcow 用户最常见的写法，必须识别。
		if suffix == "" {
			suffix = "enable"
		}
		canonical, ok := labelKeys[suffix]
		if !ok {
			continue
		}
		// 更具体的前缀优先，避免不同前缀互相覆盖。
		if _, exists := values[canonical]; exists && prefix != "watchcow." {
			continue
		}
		values[canonical] = strings.TrimSpace(rawVal)
	}

	if len(values) == 0 {
		return LabelHint{}, false
	}

	// 必须显式声明启用，否则仅凭存在标签就自动放桌面会很意外。
	switch strings.ToLower(values["enable"]) {
	case "1", "true", "yes", "on":
	default:
		return LabelHint{}, false
	}
	if strings.EqualFold(values["hide"], "true") {
		return LabelHint{}, false
	}

	hint := LabelHint{
		Title:         values["title"],
		Icon:          values["icon"],
		Scheme:        strings.ToLower(values["scheme"]),
		Path:          values["path"],
		ContainerName: containerName,
	}
	if p, err := strconv.Atoi(values["port"]); err == nil && p > 0 && p <= 65535 {
		hint.Port = p
	}
	if v := strings.ToLower(values["allusers"]); v == "true" || v == "1" || v == "yes" {
		hint.AllUsers = true
	}
	if hint.Title == "" {
		hint.Title = containerName
	}
	if hint.Scheme != "https" {
		hint.Scheme = "http"
	}
	if hint.Path == "" {
		hint.Path = "/"
	}
	return hint, true
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
