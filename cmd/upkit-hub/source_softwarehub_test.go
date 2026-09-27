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

// 站点上游的端到端（对着本地假站点）：版本来自站点、地址来自模板、摘要来自同目录的
// 校验文件，三者都要落到宿主认的字段上。
//
// 这段用 catalog 里的真实配置（真实模板 + 真实架构映射），只在测试里把 origin 换成
// 本地服务 —— 复制一份模板进用例就等于测了个假的。
func TestSoftwareHubSourceBuildsArtifactFromTemplate(t *testing.T) {
	spec := catalogSpec(t, "libreoffice")
	src, ok := spec.src.(softwareHub)
	if !ok {
		t.Fatalf("libreoffice 的上游应当换成 softwareHub，实际 %T", spec.src)
	}
	names, ok := src.arch[runtime.GOARCH]
	if !ok {
		t.Skipf("测试机架构 %s 没有对应产物", runtime.GOARCH)
	}

	const (
		version = "26.8.0"
		sha     = "4aa6c6e1895f4055104effcb556bd3362d20c6ad707c149543304f395ef9db95"
	)
	// 模板合成出来的路径（origin 还是官方的，这里只要路径部分）。
	templated, err := src.artifactURL(version, names)
	if err != nil {
		t.Fatalf("模板合成地址失败: %v", err)
	}
	wantPath := strings.TrimPrefix(templated, src.origin)
	fileName := path.Base(wantPath)

	// 站点给的是一条镜像地址：主机不同、路径多一层（腾讯云镜像就是这样），
	// 但文件名必须一致 —— 不一致时我们的模板就不可信了。
	site := fmt.Sprintf(`{"softwareId":"libreoffice","updatedAt":"2026-09-27T09:55:31Z","platforms":[
  {"platform":"Windows","version":"%s","releaseDate":"2026-05-11","officialUrl":"https://www.libreoffice.org/","packages":[
    {"architecture":"x64","links":[{"type":"direct","label":"安装包","url":"https://mirror.example.test/libreoffice/libreoffice/stable/%s/win/%s/%s"}]},
    {"architecture":"arm64","links":[{"type":"direct","label":"安装包","url":"https://mirror.example.test/libreoffice/libreoffice/stable/%s/win/aarch64/LibreOffice_%s_Win_aarch64.msi"}]}
  ]}]}`, version, version, names.dir, fileName, version, version)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/versions/libreoffice.json":
			_, _ = w.Write([]byte(site))
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			// 官方校验文件的真实写法：`<hash>  <文件名>`（两个空格）。
			fmt.Fprintf(w, "%s  %s\n", sha, fileName)
		case strings.HasSuffix(r.URL.Path, ".msi"):
			// 大小是尽力而为的附加信息（HEAD）。
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

// 站点换了命名规则时必须报错，而不是让我们合成的地址静默 404。
func TestSoftwareHubSourceDetectsNamingDrift(t *testing.T) {
	spec := catalogSpec(t, "libreoffice")
	src := spec.src.(softwareHub)
	if _, ok := src.arch[runtime.GOARCH]; !ok {
		t.Skipf("测试机架构 %s 没有对应产物", runtime.GOARCH)
	}
	const site = `{"softwareId":"libreoffice","platforms":[{"platform":"Windows","version":"26.8.0",
	  "packages":[{"architecture":"x64","links":[{"type":"direct","url":"https://mirror.example.test/lo/LibreOffice-26.8.0-x64-new-naming.msi"}]},
	              {"architecture":"arm64","links":[{"type":"direct","url":"https://mirror.example.test/lo/LibreOffice-26.8.0-arm64-new-naming.msi"}]}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/versions/libreoffice.json" {
			_, _ = w.Write([]byte(site))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src.origin, src.baseURL, src.httpClient = srv.URL, srv.URL, srv.Client()
	spec.src = src
	_, err := src.versions(context.Background(), testConfig(), spec, 1)
	if err == nil {
		t.Fatal("命名对不上时必须报错")
	}
	if !strings.Contains(err.Error(), "命名对不上") {
		t.Fatalf("错误信息应点明命名漂移，实际: %v", err)
	}
}

// 上游没有这个架构的包时（站上绝大多数软件只有 x64）必须明确报「找不到」。
func TestSoftwareHubSourceWithoutArchFails(t *testing.T) {
	src := softwareHub{siteID: "x", origin: "https://e.test", urlTemplate: "/x_{version}.msi",
		arch: map[string]archNames{"amd64": {dir: "d", file: "f"}}, digest: sha256Sidecar{}}
	spec := appSpec{id: "x", src: src}
	if runtime.GOARCH == "amd64" {
		// 这台机器有对应架构，换一个不存在的架构来验同一条分支。
		src.arch = map[string]archNames{}
		spec.src = src
	}
	if _, err := src.versions(context.Background(), testConfig(), spec, 1); err == nil {
		t.Fatal("没有对应架构时必须报错")
	} else if !strings.Contains(err.Error(), "官方没有") {
		t.Fatalf("错误信息应说明原因，实际: %v", err)
	}
}

// 少写 origin / urlTemplate / 摘要来源时必须在查询时就报错，而不是发布一个没有校验的产物。
func TestSoftwareHubSourceRequiresConfig(t *testing.T) {
	arch := map[string]archNames{"amd64": {dir: "d", file: "f"}, "arm64": {dir: "d", file: "f"}}
	cases := []struct {
		name string
		src  softwareHub
		want string
	}{
		{"缺 origin", softwareHub{siteID: "x", urlTemplate: "/x_{version}.msi", arch: arch, digest: sha256Sidecar{}}, "origin"},
		{"缺 urlTemplate", softwareHub{siteID: "x", origin: "https://e.test", arch: arch, digest: sha256Sidecar{}}, "urlTemplate"},
		{"缺摘要来源", softwareHub{siteID: "x", origin: "https://e.test", urlTemplate: "/x_{version}.msi", arch: arch}, "摘要"},
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

// 上游不发校验文件时拒绝该版本：没有摘要就不装，这是本仓库的硬规矩。
func TestSoftwareHubSourceRefusesWithoutSidecar(t *testing.T) {
	const site = `{"softwareId":"x","platforms":[{"platform":"Windows","version":"1.2.3",
	  "packages":[{"architecture":"x64","links":[{"type":"direct","url":"https://mirror.example.test/lo/x_1.2.3.msi"}]},
	              {"architecture":"arm64","links":[{"type":"direct","url":"https://mirror.example.test/lo/x_1.2.3.msi"}]}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/versions/x.json" {
			_, _ = w.Write([]byte(site))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := softwareHub{
		siteID: "x", origin: srv.URL, urlTemplate: "/x_{version}.msi",
		arch:   map[string]archNames{"amd64": {dir: "d", file: "f"}, "arm64": {dir: "d", file: "f"}},
		digest: sha256Sidecar{}, baseURL: srv.URL, httpClient: srv.Client(),
	}
	spec := appSpec{id: "x", src: src}
	_, err := src.versions(context.Background(), testConfig(), spec, 1)
	if err == nil {
		t.Fatal("取不到摘要时必须报错，而不是发布一个没有校验的产物")
	}
	if !strings.Contains(err.Error(), "拒绝安装") {
		t.Fatalf("错误信息应说明拒绝安装，实际: %v", err)
	}
}

// 版本字段是 "latest" 的软件不能接：那种版本号没法用来判断该不该升级。
func TestSoftwareHubSourceRefusesPlaceholderVersion(t *testing.T) {
	const site = `{"softwareId":"x","platforms":[{"platform":"Windows","version":"latest",
	  "packages":[{"architecture":"x64","links":[{"type":"direct","url":"https://mirror.example.test/x.exe"}]},
	              {"architecture":"arm64","links":[{"type":"direct","url":"https://mirror.example.test/x.exe"}]}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/versions/x.json" {
			_, _ = w.Write([]byte(site))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := softwareHub{
		siteID: "x", origin: srv.URL, urlTemplate: "/x_{version}.exe",
		arch:   map[string]archNames{"amd64": {dir: "d", file: "f"}, "arm64": {dir: "d", file: "f"}},
		digest: sha256Sidecar{}, baseURL: srv.URL, httpClient: srv.Client(),
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

// 域名声明由上游推导：下载域名只能是产物来源，插件自有的要含站点与校验文件所在处。
func TestSoftwareHubSourceHosts(t *testing.T) {
	src := catalogSpec(t, "libreoffice").src.(softwareHub)
	download, pluginOwn := src.hosts()
	if len(download) != 1 || download[0] != "download.documentfoundation.org" {
		t.Errorf("下载域名应当是产物来源: %v", download)
	}
	joined := strings.Join(pluginOwn, ",")
	if !strings.Contains(joined, "download.documentfoundation.org") || !strings.Contains(joined, "software-hub.sdniu.top") {
		t.Errorf("插件自有域名应当含站点与产物来源: %v", pluginOwn)
	}
}

// 模板写错时必须在查询前就拒绝（拼不出可安装的产物地址）。
func TestSoftwareHubTemplateValidation(t *testing.T) {
	src := softwareHub{origin: "https://e.test", urlTemplate: "no-leading-slash-{version}.msi"}
	if _, err := src.artifactURL("1.0.0", archNames{}); err == nil {
		t.Error("路径不以 / 开头时应当报错")
	}
	src.urlTemplate = "/x-{version}.txt"
	if _, err := src.artifactURL("1.0.0", archNames{}); err == nil {
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
	if len(rel.Artifacts) != 1 {
		t.Fatalf("应只给一个产物，实际 %+v", rel.Artifacts)
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
