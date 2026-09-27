package ucbinaries

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata 里三个页面是**真实抓取**的（2026-09-27），覆盖两种命名风格：
// github-actions 现在的 windows_x64.zip 与老贡献者 Nifury 的 windows-x64.zip。
// 站点结构一变，这些用例就会红 —— 这正是它们的作用。
const (
	fixtureVersions = "testdata/versions-windows-64bit.html"
	fixtureWin64    = "testdata/release-windows-64bit.html"
	fixtureWinARM64 = "testdata/release-windows-arm64.html"
	fixtureOldStyle = "testdata/release-windows-64bit-old-contributor.html"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读夹具 %s: %v", name, err)
	}
	return raw
}

func mustParseRelease(t *testing.T, name string) Release {
	t.Helper()
	rel, err := ParseRelease(readFixture(t, name))
	if err != nil {
		t.Fatalf("解析 %s: %v", name, err)
	}
	return rel
}

func TestParseVersions(t *testing.T) {
	versions, err := ParseVersions(readFixture(t, fixtureVersions), PlatformWindows64)
	if err != nil {
		t.Fatalf("解析索引页: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("没解析出任何版本")
	}
	// 索引页按新 → 旧排列，第一条就是最新版。
	if versions[0] != "153.0.8010.52-1" {
		t.Errorf("最新版本应是 153.0.8010.52-1，实际 %q", versions[0])
	}
	for _, v := range versions {
		if v == "" || strings.Contains(v, "/") {
			t.Errorf("版本名不合法: %q", v)
		}
		// 导航里有一个 "64-bit" 链接，它不是版本。
		if v == "64bit" || v == "64-bit" {
			t.Errorf("把导航链接当成了版本: %q", v)
		}
	}
}

// 页面结构变了（比如站点换成别的生成器）时必须报错，而不是安静地返回空列表 ——
// 空列表会被上游当成「这个软件没有可用版本」。
func TestVersionsOnGarbagePageFails(t *testing.T) {
	page := []byte("<html><body><p>nothing here</p></body></html>")

	// 解析本身不报错（HTML 合法），只是解析不出东西。
	if got, err := ParseVersions(page, PlatformWindows64); err != nil {
		t.Fatalf("解析不该失败: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("垃圾页面不该解析出版本: %v", got)
	}

	// 但通过 Client 取版本时必须报错：空结果会被上游当成「没有可用版本」。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(page)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	if _, err := c.Versions(context.Background(), PlatformWindows64, 1); err == nil {
		t.Fatal("索引页解析不出任何版本时必须报错")
	}
}

func TestParseReleaseWindows64(t *testing.T) {
	rel := mustParseRelease(t, fixtureWin64)

	if rel.Author != "GitHub Actions" {
		t.Errorf("作者应是 GitHub Actions，实际 %q", rel.Author)
	}
	if rel.PublishedAt.IsZero() {
		t.Error("发布时间没解析出来")
	}
	if len(rel.Files) != 2 {
		t.Fatalf("应有 2 个文件（安装器 + 便携 zip），实际 %d: %+v", len(rel.Files), rel.Files)
	}

	zip, ok := rel.PickPortable("amd64")
	if !ok {
		t.Fatalf("没挑出便携 zip: %+v", rel.Files)
	}
	if zip.Name != "ungoogled-chromium_153.0.8010.52-1.1_windows_x64.zip" {
		t.Errorf("挑错了文件: %s", zip.Name)
	}
	if want := "824857dcd68bca34ff21ffd06f55610fdea98be91b6328a826f3497e4881ea4a"; zip.SHA256 != want {
		t.Errorf("摘要不对: %s", zip.SHA256)
	}
	if !strings.HasPrefix(zip.URL, "https://github.com/") || !strings.HasSuffix(zip.URL, zip.Name) {
		t.Errorf("下载地址不对: %s", zip.URL)
	}

	// arm64 上绝不能退回到 x64 的包 —— 那会装出一个跑不起来的浏览器。
	if _, ok := rel.PickPortable("arm64"); ok {
		t.Error("这份页面里没有 arm64 的包，不该挑得出来")
	}
}

func TestParseReleaseWindowsARM64(t *testing.T) {
	rel := mustParseRelease(t, fixtureWinARM64)

	zip, ok := rel.PickPortable("arm64")
	if !ok {
		t.Fatalf("没挑出 arm64 的便携 zip: %+v", rel.Files)
	}
	if zip.Name != "ungoogled-chromium_153.0.8010.52-1.1_windows_arm64.zip" {
		t.Errorf("挑错了文件: %s", zip.Name)
	}
	if want := "5c5adfb5681c5769f57234c1162c62042608b1ba13b861002d2ad81292f06b50"; zip.SHA256 != want {
		t.Errorf("摘要不对: %s", zip.SHA256)
	}

	if _, ok := rel.PickPortable("amd64"); ok {
		t.Error("这份页面里没有 amd64 的包，不该挑得出来")
	}
}

// 老贡献者用 windows-x64.zip（连字符）与 installer-x64.exe 命名，匹配必须照样成立。
func TestParseReleaseOldContributorNaming(t *testing.T) {
	rel := mustParseRelease(t, fixtureOldStyle)

	if rel.Author != "Nifury" {
		t.Errorf("作者应是 Nifury，实际 %q", rel.Author)
	}
	zip, ok := rel.PickPortable("amd64")
	if !ok {
		t.Fatalf("没挑出便携 zip: %+v", rel.Files)
	}
	if zip.Name != "ungoogled-chromium_100.0.4896.60-1.1_windows-x64.zip" {
		t.Errorf("挑错了文件: %s", zip.Name)
	}
	if want := "69145427e55ebf49d52957d7ca9485204c6baf1161c22559dc0e13019da91615"; zip.SHA256 != want {
		t.Errorf("摘要不对: %s", zip.SHA256)
	}
	if strings.Contains(zip.URL, "installer") {
		t.Errorf("挑到了安装器: %s", zip.URL)
	}
}

// 没有摘要的文件不选：摘要要交给宿主做下载后校验，没有它就等于没有防线。
func TestPickPortableSkipsUnverifiable(t *testing.T) {
	const hex = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	rel := Release{Version: "1", Files: []File{
		{Name: "x_installer_x64.exe", URL: "https://e/x_installer_x64.exe", SHA256: hex},
		{Name: "x_windows_x64.zip", URL: "https://e/x_windows_x64.zip"}, // 无摘要
		{Name: "x_windows_x64.zip.sha256", URL: "https://e/x.sha256", SHA256: hex},
	}}
	if _, ok := rel.PickPortable("amd64"); ok {
		t.Fatal("只有无摘要的 zip 与安装器时不该挑得出东西")
	}

	rel.Files = append(rel.Files, File{Name: "x_windows_x64.zip", URL: "https://e/x2.zip", SHA256: hex})
	got, ok := rel.PickPortable("amd64")
	if !ok {
		t.Fatal("有摘要的便携 zip 应该被选中")
	}
	if got.URL != "https://e/x2.zip" {
		t.Errorf("挑错了: %+v", got)
	}

	// 摘要格式不对（长度或字符不对）同样跳过。
	rel.Files = []File{{Name: "x_windows_x64.zip", URL: "https://e/x.zip", SHA256: "deadbeef"}}
	if _, ok := rel.PickPortable("amd64"); ok {
		t.Fatal("非法摘要不该被接受")
	}
}

// 摘要与标签挤在同一段文字里（不同生成器可能这么写）也要能取到。
func TestParseReleaseSHA256InSameTextNode(t *testing.T) {
	page := []byte(`<html><body><h2>Downloads</h2><ul>
<li><a href="https://e/a_windows_x64.zip">a_windows_x64.zip</a><ul>
<li>SHA256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef</li>
</ul></li></ul></body></html>`)
	rel, err := ParseRelease(page)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rel.Files) != 1 {
		t.Fatalf("应有 1 个文件，实际 %+v", rel.Files)
	}
	if rel.Files[0].SHA256 != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("同段文字里的摘要没取到: %q", rel.Files[0].SHA256)
	}
}

// 站点挂在 GitHub Pages 后面，偶尔会把连接掐掉。GET 是幂等的，重试一次应当自己恢复，
// 而不是把一次抖动直接变成用户看到的「查询失败」。
func TestGetRetriesTransportFailure(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			// 直接断开连接，制造传输层错误（而不是 HTTP 状态码错误）。
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte(`<li><a href="/releases/windows/64bit/1.0">1.0</a></li>`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	versions, err := c.Versions(context.Background(), PlatformWindows64, 1)
	if err != nil {
		t.Fatalf("重试后应当成功: %v", err)
	}
	if hits != 2 {
		t.Errorf("应当只重试一次，实际请求 %d 次", hits)
	}
	if len(versions) != 1 || versions[0] != "1.0" {
		t.Errorf("版本列表不对: %v", versions)
	}
}

func TestLimitVersions(t *testing.T) {
	all := make([]string, 25)
	for i := range all {
		all[i] = "v"
	}
	cases := []struct{ limit, want int }{
		{0, DefaultVersions},
		{-1, DefaultVersions},
		{3, 3},
		{MaxVersions + 5, MaxVersions},
	}
	for _, c := range cases {
		if got := len(LimitVersions(all, c.limit)); got != c.want {
			t.Errorf("limit=%d 时返回 %d 个，期望 %d", c.limit, got, c.want)
		}
	}
	if got := len(LimitVersions(all[:2], 10)); got != 2 {
		t.Errorf("版本数少于 limit 时应返回全部，实际 %d", got)
	}
}

func TestReleaseFetchesFromServer(t *testing.T) {
	c := &Client{BaseURL: "https://example.test", Concurrency: 2}
	if got, want := c.platformURL(PlatformWindowsARM64), "https://example.test/releases/windows/arm64/"; got != want {
		t.Errorf("索引地址: %s", got)
	}
	if got, want := c.releaseURL(PlatformWindows64, "1.2.3-1"), "https://example.test/releases/windows/64bit/1.2.3-1"; got != want {
		t.Errorf("版本页地址: %s", got)
	}
	if got := (&Client{}).base(); got != DefaultBaseURL {
		t.Errorf("默认 base 应为 %s，实际 %s", DefaultBaseURL, got)
	}
}

// TestLive 走一遍真实站点：夹具会随站点结构漂移，这个用例专门盯这件事。
// 默认跳过（CI 不联网），需要时：
//
//	UPKIT_HUB_LIVE=1 go test ./internal/ucbinaries/ -run Live -v
func TestLive(t *testing.T) {
	if os.Getenv("UPKIT_HUB_LIVE") == "" {
		t.Skip("设置 UPKIT_HUB_LIVE=1 才跑联网用例")
	}
	for _, platform := range []string{PlatformWindows64, PlatformWindowsARM64} {
		client := &Client{HTTP: &http.Client{Timeout: 60 * time.Second}}
		picked, err := client.Releases(context.Background(), platform, 2, func(rel Release) (File, bool) {
			arch := "amd64"
			if platform == PlatformWindowsARM64 {
				arch = "arm64"
			}
			return rel.PickPortable(arch)
		})
		if err != nil {
			t.Fatalf("%s: %v", platform, err)
		}
		if len(picked) == 0 {
			t.Fatalf("%s: 没拿到任何版本", platform)
		}
		first := picked[0]
		if !ValidSHA256(first.File.SHA256) {
			t.Errorf("%s: 摘要不合法: %q", platform, first.File.SHA256)
		}
		if first.File.Name == "" || !strings.HasSuffix(first.File.URL, first.File.Name) {
			t.Errorf("%s: 产物字段不完整: %+v", platform, first.File)
		}
		t.Logf("%s: %s -> %s（%s）", platform, first.Release.Version, first.File.Name, first.File.SHA256[:16]+"…")
	}
}
