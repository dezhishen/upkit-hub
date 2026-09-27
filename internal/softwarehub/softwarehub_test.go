package softwarehub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testClient 起一个把 testdata 当站点根目录的本地服务。
//
// 夹具是从站点上真实抓下来的（index.json / software-list.json / versions-*.json），
// 所以站点改结构时这些用例会跟着失败 —— 那正是它们存在的意义。
func testClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir("testdata")))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestClientReadsIndexAndList(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	idx, err := c.Index(ctx)
	if err != nil {
		t.Fatalf("读取入口失败: %v", err)
	}
	if idx.SoftwareList.Path != "software-list.json" {
		t.Fatalf("入口应给出软件清单的路径，实际 %q", idx.SoftwareList.Path)
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("读取软件清单失败: %v", err)
	}
	if len(list.Items) == 0 {
		t.Fatal("软件清单是空的")
	}
	found := false
	for _, it := range list.Items {
		if it.ID != "libreoffice" {
			continue
		}
		found = true
		if it.Source.Path != "versions/libreoffice.json" {
			t.Errorf("软件清单里的数据路径不对: %q", it.Source.Path)
		}
		if it.OfficialWebsite == "" || it.Name == "" {
			t.Errorf("软件清单缺少展示信息: %+v", it)
		}
	}
	if !found {
		t.Fatal("夹具里应当有 libreoffice")
	}
}

// 挑产物必须按本机架构：站点上的架构写法很杂，归一出错就是给用户装错包。
func TestPickChoosesArtifactForArch(t *testing.T) {
	c := testClient(t)
	payload, err := c.Versions(context.Background(), "libreoffice")
	if err != nil {
		t.Fatalf("读取平台数据失败: %v", err)
	}
	if len(payload.Windows()) != 1 {
		t.Fatalf("夹具应当只有一个 Windows 桌面平台: %+v", payload.Platforms)
	}

	amd64, err := payload.Pick(ArchAMD64)
	if err != nil {
		t.Fatalf("amd64 挑选失败: %v", err)
	}
	if !strings.HasSuffix(amd64.URL, "LibreOffice_26.8.0_Win_x86-64.msi") {
		t.Errorf("amd64 挑错了产物: %s", amd64.URL)
	}
	if amd64.Version != "26.8.0" {
		t.Errorf("版本号不对: %q", amd64.Version)
	}
	if amd64.Date.IsZero() {
		t.Error("发布日期没解析出来")
	}
	if amd64.Note != "" {
		t.Errorf("精确命中时不该有额外说明: %q", amd64.Note)
	}

	arm64, err := payload.Pick(ArchARM64)
	if err != nil {
		t.Fatalf("arm64 挑选失败: %v", err)
	}
	if !strings.HasSuffix(arm64.URL, "LibreOffice_26.8.0_Win_aarch64.msi") {
		t.Errorf("arm64 挑错了产物: %s", arm64.URL)
	}

	// 站点上没有原生包的架构：宁可报错，也不要拿别的架构去凑。
	if _, err := payload.Pick("riscv64"); err == nil {
		t.Fatal("没有对应架构时必须报错")
	} else if !strings.Contains(err.Error(), "没有直链安装包") {
		t.Fatalf("错误信息应说明原因，实际: %v", err)
	}
}

// 上游没有 arm64 包时回退到 x64，并且必须把「这是回退」写进 Note。
func TestPickFallsBackAndSaysSo(t *testing.T) {
	c := testClient(t)
	payload, err := c.Versions(context.Background(), "weixin")
	if err != nil {
		t.Fatalf("读取平台数据失败: %v", err)
	}
	pick, err := payload.Pick(ArchARM64)
	if err != nil {
		t.Fatalf("arm64 应当回退到 x64，实际失败: %v", err)
	}
	if pick.Arch != ArchAMD64 {
		t.Fatalf("应当回退到 x64，实际 %q", pick.Arch)
	}
	if !strings.Contains(pick.Note, "回退") {
		t.Errorf("回退必须在说明里交代清楚，实际: %q", pick.Note)
	}
}

// 只有网页/商店入口的软件必须明确报「没有直链」，而不是给出一条点开是网页的“产物”。
func TestPickRefusesPageOnlyEntry(t *testing.T) {
	c := testClient(t)
	payload, err := c.Versions(context.Background(), "notepad-plus-plus")
	if err != nil {
		t.Fatalf("读取平台数据失败: %v", err)
	}
	if f := payload.Facts(); f.DirectLinks != 0 || f.PageOnlyLinks == 0 {
		t.Fatalf("夹具的形态变了: %+v", f)
	}
	if _, err := payload.Pick(ArchAMD64); err == nil {
		t.Fatal("只有网页入口时必须报错")
	} else if !strings.Contains(err.Error(), "只是网页或商店") {
		t.Fatalf("错误信息应解释原因，实际: %v", err)
	}
}

func TestNormalizeArch(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"x64", ArchAMD64, true},
		{"amd64", ArchAMD64, true},
		{"x86_64", ArchAMD64, true},
		{"64bit", ArchAMD64, true},
		{"x64/x86", ArchAMD64, true},   // 站点上把两个架构写在一起
		{"x64 (deb)", ArchAMD64, true}, // 带打包格式后缀
		{"arm64", ArchARM64, true},
		{"aarch64", ArchARM64, true},
		{"aarch64 (appimage)", ArchARM64, true},
		{"x86", ArchX86, true},
		{"win32", ArchX86, true},
		{"universal", "", false},
		{"通用", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeArch(c.in)
		if ok != c.valid || got != c.want {
			t.Errorf("NormalizeArch(%q) = (%q, %v)，期望 (%q, %v)", c.in, got, ok, c.want, c.valid)
		}
	}
}

func TestPlaceholderVersion(t *testing.T) {
	for _, v := range []string{"latest", "Latest", " latest ", "", "最新"} {
		if !PlaceholderVersion(v) {
			t.Errorf("%q 应当被认成占位值", v)
		}
	}
	for _, v := range []string{"26.8.0", "v2.6.4", "155.0.8059.12"} {
		if PlaceholderVersion(v) {
			t.Errorf("%q 是真实版本号", v)
		}
	}
}

func TestFacts(t *testing.T) {
	c := testClient(t)
	payload, err := c.Versions(context.Background(), "libreoffice")
	if err != nil {
		t.Fatalf("读取平台数据失败: %v", err)
	}
	f := payload.Facts()
	if f.DirectLinks != 3 {
		t.Errorf("应当有 3 条直链，实际 %d", f.DirectLinks)
	}
	if got := strings.Join(f.Arches, ","); got != "amd64,arm64,x86" {
		t.Errorf("架构集合不对: %q", got)
	}
	if f.AllPlaceholderVersions() {
		t.Errorf("26.8.0 不是占位值: %+v", f)
	}
}

// 传输层偶发断开要重试：站点在海外，一次 EOF 不该让用户看到「查不到版本」。
func TestFetchRetriesTransientFailure(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			// 模拟链路被掐：直接断开，不写任何响应。
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("测试服务器不支持 Hijack")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("Hijack 失败: %v", err)
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"softwareId":"x","platforms":[{"platform":"Windows"}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	payload, err := c.Versions(context.Background(), "x")
	if err != nil {
		t.Fatalf("应当重试后成功，实际: %v", err)
	}
	if payload.SoftwareID != "x" {
		t.Errorf("解析结果不对: %+v", payload)
	}
	if attempts < 2 {
		t.Errorf("应当重试，实际只请求了 %d 次", attempts)
	}
}

// 状态码错误不重试：404 就是没有，重试只会拖慢报错。
func TestFetchDoesNotRetryOnStatusError(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.Versions(context.Background(), "x"); err == nil {
		t.Fatal("404 应当报错")
	}
	if attempts != 1 {
		t.Errorf("404 不该重试，实际请求 %d 次", attempts)
	}
}

func TestHostOf(t *testing.T) {
	if got := HostOf("https://download.documentfoundation.org/x.msi"); got != "download.documentfoundation.org" {
		t.Errorf("主机名解析不对: %q", got)
	}
	if got := HostOf("::::"); got != "" {
		t.Errorf("解析不出来时应当返回空串: %q", got)
	}
}
