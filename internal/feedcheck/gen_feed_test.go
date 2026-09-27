package feedcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	upkitplugin "github.com/dezhishen/upkit/pkg/plugin"
)

// 本文件锁住发布链路里最容易出问题的那一段：scripts/gen-feed.sh 生成的东西必须
// 真的能被宿主读进来、通过校验，并与磁盘上的产物逐字节对应 —— 否则「一键添加官方
// 源」会在用户那里失败，而我们在 CI 上毫无察觉。
//
// 这些用例原本在 upkit 主仓库的 internal/pluginfeed 里，跟着脚本一起搬了过来。
// 搬过来之后多了一层能力：脚本与产物现在同在本仓库，所以除了「能不能被解析」，
// 还能直接断言「清单里的摘要就是产物目录里那个文件的摘要」。

// requireBashAndScript 找到 bash 与脚本；环境缺少任一时跳过（而不是失败）。
func requireBashAndScript(t *testing.T) (bash, script string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("环境里没有 bash，跳过发布脚本测试")
	}
	script = filepath.Join("..", "..", "scripts", "gen-feed.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("找不到 %s: %v", script, err)
	}
	return bash, script
}

// writeArtifacts 造一批「构建产物」。
func writeArtifacts(t *testing.T, dir string, payload []byte, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), payload, 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
}

// runGenFeed 跑一次脚本，返回合并输出；失败即 Fatal。
func runGenFeed(t *testing.T, bash, script string, args ...string) {
	t.Helper()
	cmd := exec.Command(bash, append([]string{script}, args...)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gen-feed.sh 失败: %v\n%s", err, b)
	}
}

func TestGenFeedScriptProducesValidFeed(t *testing.T) {
	bash, script := requireBashAndScript(t)

	// ── 造一批「构建产物」 ──
	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "plugins")
	payload := []byte("fake plugin binary")
	writeArtifacts(t, pluginsDir, payload,
		"upkit-hub-windows-amd64.exe",
		"upkit-hub-windows-arm64.exe",
		"corp-agent-windows-amd64.exe",
		"corp-agent-windows-arm64.exe",
	)
	// 非产物文件应当被跳过，而不是让脚本失败。
	if err := os.WriteFile(filepath.Join(pluginsDir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写 README: %v", err)
	}

	// ── 跑脚本 ──
	// base-url 故意带一个尾斜杠：脚本应把它归一化，URL 里不能出现 "//"。
	out := filepath.Join(dir, "feed.yaml")
	baseURL := "https://github.com/dezhishen/upkit-hub/releases/download/v1.2.3"
	runGenFeed(t, bash, script,
		"--plugins-dir", pluginsDir,
		"--version", "1.2.3",
		"--base-url", baseURL+"/",
		"-o", out,
		"--name", "upkit-hub=示例插件集",
		"--min-host-version", "1.0.0",
	)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("读生成结果: %v", err)
	}

	// ── 生成物必须能被宿主解析（严格模式：多写一个字段就会在这里报错）──
	feed, err := Parse(raw)
	if err != nil {
		t.Fatalf("生成的清单无法解析: %v\n%s", err, raw)
	}
	for _, platform := range SupportedPlatforms() {
		if err := feed.Validate("1.2.3", platform); err != nil {
			t.Fatalf("%s 上校验失败: %v", platform, err)
		}
	}
	if feed.Schema != SchemaVersion {
		t.Errorf("schema = %d，期望 %d", feed.Schema, SchemaVersion)
	}
	if feed.UpdatedAt.IsZero() {
		t.Errorf("updated_at 没能解析成时间: %s", raw)
	}
	if len(feed.Plugins) != 2 {
		t.Fatalf("插件数 %d，期望 2：%+v", len(feed.Plugins), feed.Plugins)
	}

	// ── 摘要、地址、大小必须与产物严格对应 ──
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])

	var hub *Plugin
	for i := range feed.Plugins {
		if feed.Plugins[i].ID == "upkit-hub" {
			hub = &feed.Plugins[i]
		}
	}
	if hub == nil {
		t.Fatalf("缺少 upkit-hub: %+v", feed.Plugins)
	}
	if hub.Name != "示例插件集" {
		t.Errorf("展示名覆盖没生效: %q", hub.Name)
	}
	if hub.Version != "1.2.3" || hub.Mode != upkitplugin.ModeCatalog {
		t.Errorf("版本/模式不对: version=%q mode=%q", hub.Version, hub.Mode)
	}
	if hub.MinHostVersion != "1.0.0" {
		t.Errorf("min_host_version 丢失: %q", hub.MinHostVersion)
	}
	for _, platform := range SupportedPlatforms() {
		pkg, ok := hub.Packages[platform]
		if !ok {
			t.Fatalf("缺少 %s 的包", platform)
		}
		if got := NormalizeSHA256(pkg.SHA256); got != want {
			t.Errorf("%s 的摘要与产物不符: %q", platform, got)
		}
		if pkg.Size != int64(len(payload)) {
			t.Errorf("%s 的 size 不对: %d", platform, pkg.Size)
		}
		if !strings.HasPrefix(pkg.URL, baseURL+"/") {
			t.Errorf("%s 的地址没有拼上 base-url: %s", platform, pkg.URL)
		}
		if strings.Contains(strings.TrimPrefix(pkg.URL, baseURL), "//") {
			t.Errorf("%s 的地址出现重复斜杠: %s", platform, pkg.URL)
		}
		// release 附件的名字必须就是约定名：feed 里的地址与附件名不一致的话，
		// 用户会下到 404。
		if got, want := filepath.Base(pkg.URL), ArtifactName("upkit-hub", platform); got != want {
			t.Errorf("%s 的地址指向了别的产物: %s，期望 %s", platform, got, want)
		}
	}

	// ── 清单与磁盘上的产物逐字节对应（脚本用的是 sha256sum，这里独立再算一次）──
	if err := feed.CheckArtifacts(pluginsDir); err != nil {
		t.Fatalf("清单与产物不一致: %v", err)
	}
}

// 漏发一个架构的产物，对应架构的用户会在「校验订阅」时失败 —— 发布前就该拦住。
func TestGenFeedScriptRejectsIncompletePlatformCoverage(t *testing.T) {
	bash, script := requireBashAndScript(t)

	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "plugins")
	// 只造 amd64：arm64 的用户将无法使用这份订阅。
	writeArtifacts(t, pluginsDir, []byte("x"), "upkit-hub-windows-amd64.exe")

	args := []string{script,
		"--plugins-dir", pluginsDir,
		"--version", "1.2.3",
		"--base-url", "https://example.com/dl",
		"-o", filepath.Join(dir, "feed.yaml"),
	}
	if out, err := exec.Command(bash, args...).CombinedOutput(); err == nil {
		t.Fatalf("缺架构时应当失败，实际成功:\n%s", out)
	} else if !strings.Contains(string(out), "windows/arm64") {
		t.Errorf("错误信息里应指出缺哪个平台:\n%s", out)
	}

	// 显式放行时应当成功。
	if out, err := exec.Command(bash, append(args, "--allow-partial")...).CombinedOutput(); err != nil {
		t.Fatalf("--allow-partial 时不应失败: %v\n%s", err, out)
	}
}

// 脚本里写死的平台列表必须与本包（进而与宿主）一致，否则会出现「宿主认为支持
// arm64、流水线却没检查它」这种静默偏差。
func TestPlatformListMatchesScript(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "gen-feed.sh"))
	if err != nil {
		t.Skipf("读不到 gen-feed.sh: %v", err)
	}
	re := regexp.MustCompile(`(?m)^SUPPORTED_PLATFORMS="([^"]*)"`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatal("gen-feed.sh 里找不到 SUPPORTED_PLATFORMS")
	}
	if got, want := strings.Fields(string(m[1])), SupportedPlatforms(); !slices.Equal(got, want) {
		t.Fatalf("脚本里的平台列表 %v 与代码里的 %v 不一致", got, want)
	}
}
