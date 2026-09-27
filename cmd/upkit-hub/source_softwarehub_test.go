package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"runtime"
	"strings"
	"testing"

	"github.com/dezhishen/upkit-hub/internal/softwarehub"
)

// catalogSpec 取出 catalog 里的一条，供用例直接测**真实配置**而不是复制一份。
func catalogSpec(t *testing.T, id string) appSpec {
	t.Helper()
	for _, spec := range catalog {
		if spec.id == id {
			return spec
		}
	}
	t.Fatalf("catalog 里没有 %s", id)
	return appSpec{}
}

// siteFixture 拼一份站点数据（只含 Windows 平台）。
func siteFixture(id, version, arch, url string) string {
	return fmt.Sprintf(`{"softwareId":"%s","updatedAt":"2026-09-27T09:55:31Z","platforms":[
  {"platform":"Windows","version":"%s","releaseDate":"2026-05-11","officialUrl":"https://example.test/","packages":[
    {"architecture":"%s","links":[{"type":"direct","label":"安装包","url":"%s"}]}
  ]}]}`, id, version, arch, url)
}

// 模板模式：地址由 origin + urlTemplate 合成，摘要取自同目录的校验文件。
//
// 这段用 catalog 里的真实配置（真实模板 + 真实架构映射），只在测试里把 origin 换成
// 本地服务 —— 复制一份模板进用例就等于测了个假的。
func TestSoftwareHubBuildsArtifactFromTemplate(t *testing.T) {
	spec := catalogSpec(t, "libreoffice")
	src := spec.src.(softwareHub)
	names, _, ok := src.pickArch(runtime.GOARCH)
	if !ok {
		t.Skipf("测试机架构 %s 没有对应产物", runtime.GOARCH)
	}

	const (
		version = "26.8.0"
		sha     = "4aa6c6e1895f4055104effcb556bd3362d20c6ad707c149543304f395ef9db95"
	)
	// 先用一个「转发地址」问出模板合成的路径（转发地址不参与文件名交叉校验，见
	// TestSoftwareHubDetectsNamingDrift），再拿它的文件名去搭站点夹具。
	pick := softwarehub.Pick{Version: version, URL: "https://mirror.example.test/?product=libreoffice"}
	templated, err := src.artifactURL(pick, names)
	if err != nil {
		t.Fatalf("模板合成地址失败: %v", err)
	}
	wantPath := strings.TrimPrefix(templated, src.origin)
	fileName := path.Base(wantPath)
	// 站点的地址带同样的文件名（只是主机与目录层级不同）。
	archLabel := "x64"
	if runtime.GOARCH == "arm64" {
		archLabel = "arm64"
	}
	site := siteFixture("libreoffice", version, archLabel,
		"https://mirror.example.test/libreoffice/libreoffice/stable/"+version+"/win/"+names.dir+"/"+fileName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/versions/libreoffice.json":
			_, _ = w.Write([]byte(site))
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			// 官方校验文件的真实写法：`<hash>  <文件名>`（两个空格）。
			fmt.Fprintf(w, "%s  %s\n", sha, fileName)
		case strings.HasSuffix(r.URL.Path, ".msi"):
			w.Header().Set("Content-Length", "345678901")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src.origin = srv.URL
	src.baseURL = srv.URL
	src.httpClient = srv.Client()
	spec.src = src

	rels, err := src.versions(context.Background(), testConfig(), spec, 3)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("站点只有当前版本，应返回 1 个版本，实际 %d", len(rels))
	}
	rel := rels[0]
	if rel.Version != version {
		t.Errorf("版本号不对: %q", rel.Version)
	}
	if rel.PublishedAt.IsZero() {
		t.Error("发布日期没传下去")
	}
	if !strings.Contains(rel.Notes, "下载导航站") {
		t.Errorf("版本说明应交代产物来源: %q", rel.Notes)
	}
	if len(rel.Artifacts) != 1 {
		t.Fatalf("应只给一个产物（宿主只用 Artifacts[0]），实际 %+v", rel.Artifacts)
	}
	art := rel.Artifacts[0]
	if want := srv.URL + wantPath; art.URL != want {
		t.Errorf("产物地址应由模板合成:\n got %s\nwant %s", art.URL, want)
	}
	if art.Name != fileName {
		t.Errorf("落盘名不对: %s", art.Name)
	}
	if art.Digest != "sha256:"+sha {
		t.Errorf("摘要应以 sha256: 前缀交给宿主: %s", art.Digest)
	}
	if art.Size != 345678901 {
		t.Errorf("产物大小应从 HEAD 取到: %d", art.Size)
	}
}

// 站点地址模式：路径里带不可合成的片段时（QQ、钉钉的 CDN 就是这样）照站点给的地址
// 下载，但域名必须与声明的完全一致。
func TestSoftwareHubUsesSiteURLWhenDeclared(t *testing.T) {
	const site = `{"softwareId":"x","platforms":[{"platform":"Windows","version":"1.2.3",
	  "packages":[{"architecture":"x64","links":[{"type":"direct","url":"https://cdn.example.test/a/b-1.2.3-x64.exe"}]}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/versions/x.json" {
			_, _ = w.Write([]byte(site))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := softwareHub{
		siteID: "x", baseURL: srv.URL, httpClient: srv.Client(),
		arch:   map[string]archNames{"amd64": {dir: "d", file: "f"}, "arm64": {dir: "d", file: "f"}},
		digest: stubDigest{sum: "sha256:" + strings.Repeat("a", 64)},
	}
	spec := appSpec{id: "x", src: src}

	// 没声明 downloadHost：地址来自哪里就说不清了，必须报错。
	if _, err := src.versions(context.Background(), testConfig(), spec, 1); err == nil {
		t.Fatal("既没有 origin 也没有 downloadHost 时必须报错")
	}

	// 声明了但与站点给的地址不符：同样报错，而不是悄悄改声明。
	src.downloadHost = "other.example.test"
	if _, err := src.versions(context.Background(), testConfig(), spec, 1); err == nil {
		t.Fatal("地址与声明的域名不符时必须报错")
	} else if !strings.Contains(err.Error(), "downloadHost") {
		t.Fatalf("错误信息应点明 downloadHost，实际: %v", err)
	}

	// 声明一致：直接采用站点给的地址。
	src.downloadHost = "cdn.example.test"
	rels, err := src.versions(context.Background(), testConfig(), spec, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got := rels[0].Artifacts[0].URL; got != "https://cdn.example.test/a/b-1.2.3-x64.exe" {
		t.Errorf("应当原样使用站点给的地址，实际 %s", got)
	}
}

// 上游只发 x64 包时，arm64 回退到 x64 的包，并且必须把「这是回退」写进说明。
func TestSoftwareHubFallsBackToAMD64OnARM64(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("只有在 arm64 机器上才会走到回退分支")
	}
	src := softwareHub{
		arch: map[string]archNames{"amd64": {dir: "win64", file: "x64"}},
	}
	names, arch, ok := src.pickArch("arm64")
	if !ok || arch != "amd64" {
		t.Fatalf("arm64 应当回退到 amd64，实际 (%v, %q, %v)", names, arch, ok)
	}
	if _, arch, ok := src.pickArch("riscv64"); ok || arch != "" {
		t.Fatal("没有对应架构时必须报 not ok")
	}
}

// 站点换了命名规则时必须报错，而不是让我们合成的地址静默 404。
func TestSoftwareHubDetectsNamingDrift(t *testing.T) {
	spec := catalogSpec(t, "libreoffice")
	src := spec.src.(softwareHub)
	names, _, ok := src.pickArch(runtime.GOARCH)
	if !ok {
		t.Skipf("测试机架构 %s 没有对应产物", runtime.GOARCH)
	}
	pick := softwarehub.Pick{Version: "26.8.0", URL: "https://mirror.example.test/lo/LibreOffice-new-naming.msi"}
	if _, err := src.artifactURL(pick, names); err == nil {
		t.Fatal("命名对不上时必须报错")
	} else if !strings.Contains(err.Error(), "命名") {
		t.Fatalf("错误信息应点明命名漂移，实际: %v", err)
	}
	// 站点给的是转发地址（没有文件名）时跳过校验：没得对就不假装对过。
	pick.URL = "https://download.mozilla.org/?product=firefox-msi-latest-ssl&os=win64"
	if _, err := src.artifactURL(pick, names); err != nil {
		t.Fatalf("转发地址不该触发交叉校验: %v", err)
	}
}

// 少写 origin / downloadHost / 摘要来源 / 架构映射时必须在查询时就报错。
func TestSoftwareHubRequiresConfig(t *testing.T) {
	arch := map[string]archNames{"amd64": {dir: "d", file: "f"}, "arm64": {dir: "d", file: "f"}}
	cases := []struct {
		name string
		src  softwareHub
		want string
	}{
		{"缺 origin/downloadHost", softwareHub{siteID: "x", arch: arch, digest: sha256Sidecar{}}, "downloadHost"},
		{"缺摘要来源", softwareHub{siteID: "x", downloadHost: "e.test", arch: arch}, "摘要"},
		{"缺架构映射", softwareHub{siteID: "x", downloadHost: "e.test", digest: sha256Sidecar{}}, "架构"},
	}
	for _, c := range cases {
		spec := appSpec{id: "x", src: c.src}
		_, err := c.src.versions(context.Background(), testConfig(), spec, 1)
		if err == nil {
			t.Errorf("%s：必须报错", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息应点明 %q，实际: %v", c.name, c.want, err)
		}
	}
}

// 版本字段是 "latest" 的软件不能接：那种版本号没法用来判断该不该升级。
func TestSoftwareHubRefusesPlaceholderVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(siteFixture("x", "latest", "x64", "https://cdn.example.test/x.exe")))
	}))
	defer srv.Close()

	src := softwareHub{
		siteID: "x", baseURL: srv.URL, httpClient: srv.Client(), downloadHost: "cdn.example.test",
		arch:   map[string]archNames{"amd64": {dir: "d", file: "f"}, "arm64": {dir: "d", file: "f"}},
		digest: stubDigest{sum: "sha256:" + strings.Repeat("a", 64)},
	}
	spec := appSpec{id: "x", src: src}
	_, err := src.versions(context.Background(), testConfig(), spec, 1)
	if err == nil {
		t.Fatal("版本是占位值时必须报错")
	}
	if !strings.Contains(err.Error(), "latest") || !strings.Contains(err.Error(), "无法接入") {
		t.Fatalf("错误信息应说明原因，实际: %v", err)
	}
}

// 取不到摘要时拒绝该版本：没有摘要就不装，这是本仓库的硬规矩。
func TestSoftwareHubRefusesWithoutDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(siteFixture("x", "1.2.3", "x64", "https://cdn.example.test/x.exe")))
	}))
	defer srv.Close()

	src := softwareHub{
		siteID: "x", baseURL: srv.URL, httpClient: srv.Client(), downloadHost: "cdn.example.test",
		arch:   map[string]archNames{"amd64": {dir: "d", file: "f"}, "arm64": {dir: "d", file: "f"}},
		digest: nil,
	}
	spec := appSpec{id: "x", src: src}
	if _, err := src.versions(context.Background(), testConfig(), spec, 1); err == nil {
		t.Fatal("没有摘要来源时必须报错")
	}
}

// stubDigest 是测试用的摘要来源：固定返回一个摘要。
type stubDigest struct{ sum string }

func (s stubDigest) digest(context.Context, digestRef) (string, error) { return s.sum, nil }

// 校验清单的解析：Mozilla 的 SHA256SUMS 文件名里带空格，必须按整条路径后缀匹配，
// 不能按位置切字段。
func TestFindSHA256(t *testing.T) {
	const sums = `254b88de81d6def593a7558ba848a389b49f5ff83d0825c814858dab13951a83  jsshell/jsshell-win32.zip
7893b5e467a2d603253505609f96a4e73ca1cdd079ff9927c64e2ff8ce193049  win64/zh-CN/Firefox Setup 156.0.1.msi
f1bfb769e4ed3560e5d0000fe35687e6daa051e6dc77a93724168142d5d19ec2  win64/zh-CN/Firefox Setup 156.0.1.exe
`
	got, err := findSHA256(sums,
		"https://download-installer.cdn.mozilla.net/pub/firefox/releases/156.0.1/win64/zh-CN/Firefox%20Setup%20156.0.1.msi")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if want := "sha256:7893b5e467a2d603253505609f96a4e73ca1cdd079ff9927c64e2ff8ce193049"; got != want {
		t.Errorf("取错了摘要（文件名带空格时容易取到相近的一条）:\n got %s\nwant %s", got, want)
	}

	// BSD 风格：SHA256 (file) = hash
	if got, err := findSHA256("SHA256 (tool-1.0.zip) = abcdef\n", "https://e.test/tool-1.0.zip"); err == nil {
		t.Errorf("不合法摘要不该被接受: %s", got)
	}
	sum := strings.Repeat("b", 64)
	got, err = findSHA256(fmt.Sprintf("SHA256 (tool-1.0.zip) = %s\n", sum), "https://e.test/tool-1.0.zip")
	if err != nil || got != "sha256:"+sum {
		t.Errorf("BSD 风格写法应能解析: %q %v", got, err)
	}

	// 清单里没有它 → 报错，不要拿别人的摘要顶上。
	if _, err := findSHA256(sums, "https://e.test/other/Thing.exe"); err == nil {
		t.Error("清单里没有该产物时必须报错")
	}
}

// 构建期摘要表：查得到就用，查不到就报错（上游发了新版时就是这个分支）。
func TestPinnedDigestLookup(t *testing.T) {
	const url = "https://dl.example.test/app_1.0.0.exe"
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	src := pinnedDigest{}

	if _, err := src.digest(context.Background(), digestRef{URL: url}); err == nil {
		t.Fatal("表里没有这个地址时必须报错")
	} else if !strings.Contains(err.Error(), "摘要表") {
		t.Fatalf("错误信息应点明摘要表，实际: %v", err)
	}

	// 表里有（临时往缓存里塞一条，避免依赖真实生成的表）。
	table, err := pinnedTable()
	if err != nil {
		t.Fatalf("读取摘要表失败: %v", err)
	}
	table[url] = pinnedEntry{SHA256: sum, Size: 123, Version: "1.0.0"}
	t.Cleanup(func() { delete(table, url) })

	got, err := src.digest(context.Background(), digestRef{URL: url})
	if err != nil {
		t.Fatalf("查表失败: %v", err)
	}
	if got != "sha256:"+sum {
		t.Errorf("摘要不对: %s", got)
	}
	if size, ok := src.size(url); !ok || size != 123 {
		t.Errorf("大小应从表里取到: %d %v", size, ok)
	}
}

// 表里的数据本身要合法：它是发布前生成的，写坏了等于给用户一个错摘要（比没有更糟）。
func TestPinnedDigestTableIsWellFormed(t *testing.T) {
	table, err := pinnedTable()
	if err != nil {
		t.Fatalf("读取摘要表失败: %v", err)
	}
	if len(table) == 0 {
		t.Skip("摘表还是空的（scripts/gen-digests.sh 尚未运行）")
	}
	for url, entry := range table {
		if !isSHA256Hex(entry.SHA256) {
			t.Errorf("%s 的摘要不合法: %q", url, entry.SHA256)
		}
		if entry.Size <= 0 {
			t.Errorf("%s 没有记大小", url)
		}
		if entry.Version == "" {
			t.Errorf("%s 没有记版本号", url)
		}
		if !strings.Contains(url, entry.Version) {
			t.Errorf("%s 的地址里看不到版本号 %q —— 滚动地址不能进这张表", url, entry.Version)
		}
	}
}

// 域名声明由上游推导：下载域名只能是产物来源，插件自有的要含站点与校验文件所在处。
func TestSoftwareHubHosts(t *testing.T) {
	src := catalogSpec(t, "libreoffice").src.(softwareHub)
	download, pluginOwn := src.hosts()
	if len(download) != 1 || download[0] != "download.documentfoundation.org" {
		t.Errorf("下载域名应当是产物来源: %v", download)
	}
	joined := strings.Join(pluginOwn, ",")
	if !strings.Contains(joined, "download.documentfoundation.org") || !strings.Contains(joined, "software-hub.sdniu.top") {
		t.Errorf("插件自有域名应当含站点与产物来源: %v", pluginOwn)
	}

	// Mozilla 的校验清单在另一个域名上，必须一起声明出来。
	mozilla := catalogSpec(t, "firefox").src.(softwareHub)
	_, pluginOwn = mozilla.hosts()
	if !strings.Contains(strings.Join(pluginOwn, ","), "ftp.mozilla.org") {
		t.Errorf("摘要来源的域名应当出现在插件自有域名里: %v", pluginOwn)
	}
}

// 模板写错时必须在查询前就拒绝（拼不出可安装的产物地址）。
func TestSoftwareHubTemplateValidation(t *testing.T) {
	src := softwareHub{origin: "https://e.test", urlTemplate: "no-leading-slash-{version}.msi"}
	if _, err := src.artifactURL(softwarehub.Pick{Version: "1.0.0"}, archNames{}); err == nil {
		t.Error("路径不以 / 开头时应当报错")
	}
	src.urlTemplate = "/x-{version}.txt"
	if _, err := src.artifactURL(softwarehub.Pick{Version: "1.0.0"}, archNames{}); err == nil {
		t.Error("后缀不是产物类型时应当报错")
	}
}

// VS Code 上游：版本、固定地址与摘要都取自官方更新接口。
func TestVSCodeUpdateSource(t *testing.T) {
	const sha = "2186d345470644f232f6682d4a9a18f335e96e0233eb29c9ab005dc4dee0220e"
	product := vscodeArchiveProducts[runtime.GOARCH]
	if product == "" {
		t.Skipf("测试机架构 %s 没有对应的归档产物", runtime.GOARCH)
	}
	api := fmt.Sprintf(`{"url":"https://cdn.example.test/dbazure/download/stable/deadbeef/VSCode-%s-1.139.1.zip",
	  "name":"1.139.1","version":"deadbeef","productVersion":"1.139.1","sha256hash":"%s","timestamp":1790308685731}`, product, sha)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/update/"+product+"/stable/latest" {
			_, _ = w.Write([]byte(api))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stable") {
			// 固定地址会转发到 CDN，转发之后才有 Content-Length。
			w.Header().Set("Content-Length", "336761238")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	spec := appSpec{id: "vscode", src: vscodeUpdate{products: vscodeArchiveProducts, baseURL: srv.URL}}
	rels, err := spec.src.versions(context.Background(), testConfig(), spec, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("官方接口只回答当前版本，应返回 1 个版本，实际 %d", len(rels))
	}
	rel := rels[0]
	if rel.Version != "1.139.1" {
		t.Errorf("版本号不对: %q", rel.Version)
	}
	if rel.PublishedAt.IsZero() {
		t.Error("发布时间没传下去")
	}
	art := rel.Artifacts[0]
	// 地址必须是**带版本号**的固定地址：滚动地址上没法固定摘要。
	want := fmt.Sprintf("%s/1.139.1/%s/stable", srv.URL, product)
	if art.URL != want {
		t.Errorf("产物地址应为固定地址:\n got %s\nwant %s", art.URL, want)
	}
	if art.Name != fmt.Sprintf("VSCode-%s-1.139.1.zip", product) {
		t.Errorf("落盘名应沿用官方文件名: %s", art.Name)
	}
	if art.Digest != "sha256:"+sha {
		t.Errorf("摘要应为接口给出的 sha256: %s", art.Digest)
	}
	if art.Size != 336761238 {
		t.Errorf("产物大小应从固定地址取到: %d", art.Size)
	}
	if !strings.Contains(rel.Notes, "免安装") {
		t.Errorf("版本说明应交代这是便携版: %q", rel.Notes)
	}
}

// 更新接口没给摘要时不能发布产物；这个架构没有产物时也要明确报「找不到」。
func TestVSCodeUpdateSourceRequiresDigestAndProduct(t *testing.T) {
	product := vscodeArchiveProducts[runtime.GOARCH]
	if product == "" {
		t.Skipf("测试机架构 %s 没有对应的归档产物", runtime.GOARCH)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"productVersion":"1.139.1","url":"https://cdn.example.test/x.zip"}`))
	}))
	defer srv.Close()

	spec := appSpec{id: "vscode"}
	src := vscodeUpdate{products: vscodeArchiveProducts, baseURL: srv.URL}
	spec.src = src
	if _, err := src.versions(context.Background(), testConfig(), spec, 1); err == nil {
		t.Fatal("没有摘要时必须报错")
	}

	missing := vscodeUpdate{products: map[string]string{}, baseURL: srv.URL}
	spec.src = missing
	if _, err := missing.versions(context.Background(), testConfig(), spec, 1); err == nil {
		t.Fatal("没有对应架构的产物时必须报错")
	}
}
