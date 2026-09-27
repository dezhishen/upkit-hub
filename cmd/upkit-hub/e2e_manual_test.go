package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dezhishen/upkit/pkg/plugin"
)

// 这是一个**临时**的端到端验证：把本插件按当前平台编译出来，用 SDK 真实地把它作为
// 子进程拉起来（握手 → List → Versions），验证注册与上游查询在真实进程里成立。
//
// 验完即删，不入库 —— 它需要联网访问索引站，不适合进 CI。
//
// 用法：UPKIT_HUB_LIVE=1 go test ./cmd/upkit-hub -run TestManualE2E -v
func TestManualE2E(t *testing.T) {
	if os.Getenv("UPKIT_HUB_LIVE") == "" {
		t.Skip("设置 UPKIT_HUB_LIVE=1 才跑")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "upkit-hub")
	build := exec.Command("go", "build", "-o", bin, "github.com/dezhishen/upkit-hub/cmd/upkit-hub")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("编译插件失败: %v\n%s", err, out)
	}

	stderr := &strings.Builder{}
	client, err := plugin.NewClient(plugin.ClientConfig{
		Exec:    bin,
		Dir:     dir,
		Timeout: 30 * time.Second,
		Stderr:  stderr,
	})
	if err != nil {
		t.Fatalf("启动插件失败: %v\n--- 插件输出 ---\n%s", err, stderr.String())
	}
	defer client.Kill()

	info := client.Info()
	t.Logf("插件信息: id=%s name=%s version=%s", info.ID, info.Name, info.Version)
	if info.ID != "upkit-hub" {
		t.Fatalf("插件 id 应为 upkit-hub，实际 %q", info.ID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	list, err := client.Source().List(ctx)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != len(catalog) {
		t.Fatalf("应注册 %d 个软件，实际 %d", len(catalog), len(list))
	}
	for _, sw := range list {
		t.Logf("软件: %s（%s）默认安装路径=%s 入口=%v", sw.ID, sw.Name,
			sw.Defaults.Install.Path, sw.Defaults.Install.Entrypoints)
	}
	if client.Ping(ctx) != nil {
		t.Fatalf("Ping 失败")
	}

	for _, appID := range []string{"ungoogled-chromium", "fzf", "7zip", "vscode", "libreoffice", "firefox", "thunderbird", "weixin", "baidunetdisk", "git", "easytier"} {
		rels, err := client.Source().Versions(ctx, plugin.SourceVersionsRequest{AppID: appID, Limit: 2})
		if err != nil {
			t.Errorf("%s 查询失败: %v", appID, err)
			continue
		}
		for _, rel := range rels {
			if len(rel.Artifacts) == 0 {
				t.Errorf("%s v%s 没有任何产物", appID, rel.Version)
				continue
			}
			art := rel.Artifacts[0]
			t.Logf("%s v%s -> %s（大小 %d 摘要 %s）", appID, rel.Version, art.Name, art.Size, art.Digest)
			if art.URL == "" {
				t.Errorf("%s v%s 的产物没有下载地址", appID, rel.Version)
			}
			if strings.Contains(art.Name, "installer") && appID == "ungoogled-chromium" {
				t.Errorf("挑到了安装器而不是便携版: %s", art.Name)
			}
		}
	}
}
