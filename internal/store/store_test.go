package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	return s, dir
}

func sampleLink(id, name string, port int) domain.Link {
	return domain.Link{
		ID:      id,
		Name:    name,
		Kind:    domain.KindLocalPort,
		Scheme:  domain.SchemeHTTP,
		Port:    port,
		Path:    "/",
		UI:      domain.UIWindow,
		Enabled: true,
	}
}

func TestOpenCreatesDefaults(t *testing.T) {
	t.Parallel()

	s, dir := newTestStore(t)
	if got := s.Settings().PortalPort; got != domain.DefaultPortalPort {
		t.Fatalf("默认端口应为 %d，实际 %d", domain.DefaultPortalPort, got)
	}
	if _, err := os.Stat(filepath.Join(dir, fileSettings)); err != nil {
		t.Fatalf("默认设置文件应已落盘: %v", err)
	}
}

func TestUpsertAndList(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)

	if _, err := s.UpsertLink(sampleLink("", "A", 8080)); err != nil {
		t.Fatalf("新增失败: %v", err)
	}
	// 第二次插入创建时间更晚，应排在列表前面。
	if _, err := s.UpsertLink(sampleLink("", "B", 8081)); err != nil {
		t.Fatalf("新增失败: %v", err)
	}

	list := s.ListLinks()
	if len(list) != 2 {
		t.Fatalf("期望 2 条，实际 %d", len(list))
	}
	for _, l := range list {
		if l.ID == "" {
			t.Error("ID 应被自动生成")
		}
		if l.AppName == "" {
			t.Error("AppName 应被自动派生")
		}
		if err := domain.ValidateAppName(l.AppName); err != nil {
			t.Errorf("派生包名不合法: %v", err)
		}
	}
}

func TestUpsertPreservesCreatedAt(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	saved, err := s.UpsertLink(sampleLink("fixed-id", "A", 8080))
	if err != nil {
		t.Fatal(err)
	}
	created := saved.CreatedAt

	saved.Name = "A renamed"
	saved.Desc = "updated"
	again, err := s.UpsertLink(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !again.CreatedAt.Equal(created) {
		t.Errorf("更新不应改变创建时间: %v -> %v", created, again.CreatedAt)
	}
	if again.Name != "A renamed" {
		t.Errorf("名称未更新: %q", again.Name)
	}
}

func TestUpsertRejectsInvalid(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	bad := domain.Link{ID: "x", Name: "A", Kind: domain.KindLocalPort, Port: 0}
	if _, err := s.UpsertLink(bad); err == nil {
		t.Fatal("端口为 0 应被拒绝")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("应为校验错误，实际: %v", err)
	}
}

func TestAppNameConflictResolution(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)

	// 同一个容器名 + 不同 ID，必须拿到不同的包名，不能互相顶掉。
	base := domain.Link{
		Name: "Web", Kind: domain.KindLocalPort, Scheme: domain.SchemeHTTP, Path: "/",
		Port: 80, Container: domain.Container{Name: "nginx"},
	}

	a := base
	a.ID = "item-aaa111"
	a.Name = "Web A"
	savedA, err := s.UpsertLink(a)
	if err != nil {
		t.Fatal(err)
	}

	b := base
	b.ID = "item-bbb222"
	b.Name = "Web B"
	savedB, err := s.UpsertLink(b)
	if err != nil {
		t.Fatal(err)
	}

	if savedA.AppName == savedB.AppName {
		t.Fatalf("两条链接不应共享包名: %q", savedA.AppName)
	}

	taken := s.TakenAppNames()
	if !taken[savedA.AppName] || !taken[savedB.AppName] {
		t.Error("TakenAppNames 应包含两条链接的包名")
	}
}

func TestEditingKeepsOwnAppName(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	saved, err := s.UpsertLink(sampleLink("keep-me", "A", 8080))
	if err != nil {
		t.Fatal(err)
	}
	original := saved.AppName

	saved.Name = "A edited"
	again, err := s.UpsertLink(saved)
	if err != nil {
		t.Fatal(err)
	}
	if again.AppName != original {
		t.Fatalf("编辑自身不应改变包名: %q -> %q", original, again.AppName)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	saved, _ := s.UpsertLink(sampleLink("del-me", "A", 8080))

	removed, err := s.DeleteLink(saved.ID)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if removed.ID != saved.ID {
		t.Errorf("返回的被删对象不对: %+v", removed)
	}
	if len(s.ListLinks()) != 0 {
		t.Error("删除后列表应为空")
	}
	if _, err := s.DeleteLink(saved.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("重复删除应返回 ErrNotFound，实际: %v", err)
	}
}

func TestSetLinkEnabled(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	saved, _ := s.UpsertLink(sampleLink("t1", "A", 8080))

	updated, err := s.SetLinkEnabled(saved.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled {
		t.Error("启用位应被置为 false")
	}
	if _, err := s.SetLinkEnabled("missing", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("对不存在 ID 应返回 ErrNotFound，实际: %v", err)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s1.UpsertLink(sampleLink("persist", "持久化", 9999))
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.UpdateSettings(func(st *domain.Settings) error {
		st.PortalName = "我的入口"
		st.PortalPort = 6001
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.SetToggle("watchcow-abc", true); err != nil {
		t.Fatal(err)
	}

	// 重新打开同一个目录，数据必须完整回来。
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}

	got, ok := s2.GetLink(saved.ID)
	if !ok {
		t.Fatal("链接未能持久化")
	}
	if got.Name != "持久化" || got.Port != 9999 || got.AppName != saved.AppName {
		t.Errorf("链接字段不一致: %+v", got)
	}
	if s2.Settings().PortalName != "我的入口" || s2.Settings().PortalPort != 6001 {
		t.Errorf("设置未持久化: %+v", s2.Settings())
	}
	if !s2.Toggle("watchcow-abc", false) {
		t.Error("开关状态未持久化")
	}
}

func TestLinkByAppName(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	saved, _ := s.UpsertLink(sampleLink("byapp", "A", 8080))

	found, ok := s.LinkByAppName(saved.AppName)
	if !ok || found.ID != saved.ID {
		t.Fatalf("按包名反查失败: %v %+v", ok, found)
	}
	if _, ok := s.LinkByAppName("qlink2d.not-exist"); ok {
		t.Error("不存在的包名不应命中")
	}
	if _, ok := s.LinkByAppName(""); ok {
		t.Error("空包名不应命中")
	}
}

func TestUpdateSettingsMutationErrorLeavesStateUntouched(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	before := s.Settings()

	sentinel := errors.New("boom")
	if err := s.UpdateSettings(func(st *domain.Settings) error {
		st.PortalPort = 12345
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("应原样返回回调错误，实际: %v", err)
	}
	if s.Settings().PortalPort != before.PortalPort {
		t.Error("回调返回错误时不应写入任何变更")
	}
}

func TestUpdateSettingsNormalizesInput(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	if err := s.UpdateSettings(func(st *domain.Settings) error {
		st.PortalPort = -1
		st.PortalName = "   "
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := s.Settings()
	if got.PortalPort != domain.DefaultPortalPort {
		t.Errorf("非法端口应回落到默认值，实际 %d", got.PortalPort)
	}
	if strings.TrimSpace(got.PortalName) == "" {
		t.Error("空名称应回落到默认值")
	}
}

func TestCorruptFileIsQuarantinedNotLost(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// 先写入一份坏掉的 links.json 模拟掉电 / 磁盘写满。
	if err := os.WriteFile(filepath.Join(dir, fileLinks), []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("遇到损坏文件也不应启动失败: %v", err)
	}
	if len(s.ListLinks()) != 0 {
		t.Error("损坏文件应被重置为空列表")
	}

	// 现场必须保留下来供排查。
	entries, _ := os.ReadDir(dir)
	var found bool
	for _, e := range entries {
		if strings.Contains(e.Name(), "links.json.corrupt-") {
			found = true
		}
	}
	if !found {
		t.Error("损坏文件应被备份为 *.corrupt-* 而不是删除")
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	t.Parallel()

	s, dir := newTestStore(t)
	for i := 0; i < 5; i++ {
		if _, err := s.UpsertLink(sampleLink("", "X", 9000+i)); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("原子写不应残留临时文件: %s", e.Name())
		}
	}
}

func TestToggleDefaults(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	if !s.Toggle("never-set", true) {
		t.Error("未设置时应返回默认值")
	}
	if s.Toggle("never-set", false) {
		t.Error("未设置时应返回默认值 false")
	}
}
