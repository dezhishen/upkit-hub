package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/dezhishen/upkit-hub/internal/feedcheck"
	"github.com/dezhishen/upkit-hub/internal/softwarehub"
	"github.com/dezhishen/upkit/pkg/plugin"
)

// nopLogger 吞掉插件日志：单测里不需要往 stderr 写东西。
type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

func testConfig() plugin.AppConfig { return plugin.AppConfig{Log: nopLogger{}} }

// catalog 里的每条都必须能被宿主接受：id 合法且唯一、装到哪去与入口都写了。
// 这些字段错了，用户看到的是「装上但用不了」，而发布前的检查只看得到清单本身。
func TestCatalogEntriesAreComplete(t *testing.T) {
	if len(catalog) == 0 {
		t.Fatal("catalog 是空的")
	}
	seen := map[string]bool{}
	for _, spec := range catalog {
		if !plugin.ValidID(spec.id) {
			t.Errorf("id %q 不合法", spec.id)
		}
		if seen[spec.id] {
			t.Errorf("id %q 重复", spec.id)
		}
		seen[spec.id] = true

		if spec.name == "" || spec.desc == "" || spec.homepage == "" {
			t.Errorf("%s 缺少展示信息: %+v", spec.id, spec)
		}
		if spec.src == nil {
			t.Errorf("%s 没有上游（src）", spec.id)
		}
		if len(spec.provides) == 0 {
			t.Errorf("%s 没有 provides：宿主靠它跨来源去重", spec.id)
		}
		if spec.installPath == "" || spec.method == "" || spec.unpack == "" {
			t.Errorf("%s 缺少装配信息: %+v", spec.id, spec)
		}
		if len(spec.entry) == 0 {
			t.Errorf("%s 没有声明入口可执行文件", spec.id)
		}
	}
}

// {arch} 必须按上游的命名习惯展开 —— 展开错了，另一个架构的机器就挑不到包。
func TestAssetPatternExpandsArch(t *testing.T) {
	if got, want := assetPattern("fzf-*-windows_{arch}.zip", "amd64"), "fzf-*-windows_amd64.zip"; got != want {
		t.Errorf("展开结果 %q，期望 %q", got, want)
	}
	// 上游把 amd64 叫 x64 时（7-Zip），映射表要起作用。
	zip := appSpec{arch: map[string]string{"amd64": "x64"}}
	if got := assetPattern("7z*-{arch}.exe", zip.archToken()); got != "7z*-x64.exe" && runtime.GOARCH == "amd64" {
		t.Errorf("7-Zip 在 amd64 上应展开成 7z*-x64.exe，实际 %q", got)
	}
	// 没有映射的架构直接用 GOARCH。
	if got := (appSpec{}).archToken(); got != runtime.GOARCH {
		t.Errorf("没有映射时应直接用 GOARCH，实际 %q", got)
	}
	// 通配里没有 {arch} 时原样保留。
	if got := assetPattern("tool.zip", "arm64"); got != "tool.zip" {
		t.Errorf("没有占位符时不该改动: %q", got)
	}
}

// 索引站源的端到端（对着本地假站点）：版本名、产物、摘要、说明都要落到宿主认的字段上。
func TestUCBinariesSourcePicksArtifactForArch(t *testing.T) {
	arch := runtime.GOARCH
	dir := map[string]string{"amd64": "64bit", "arm64": "arm64"}[arch]
	if dir == "" {
		t.Skipf("测试机架构 %s 不在索引站的平台目录里", arch)
	}
	// 文件名里的架构片段：amd64 → x64，arm64 → arm64（与站点上的命名一致）。
	token := map[string]string{"amd64": "x64", "arm64": "arm64"}[arch]

	const (
		version = "9.9.9-1"
		sha     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	index := fmt.Sprintf(`<ul><li><a href="/base/releases/windows/%s/%s">%s</a></li></ul>`, dir, version, version)
	release := fmt.Sprintf(`<html><body>
<h2>Release Information</h2><ul>
<li>Author: <a href="https://github.com/u/actions">贡献者甲</a></li>
<li>Publication time (in UTC): <code>2026-09-20 17:27:14.964249+00:00</code></li>
</ul>
<h2>Downloads</h2><ul>
<li><a href="https://example.test/ungoogled-chromium_%s_installer_x64.exe">installer</a><ul>
<li>SHA256: <code>aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa</code></li></ul></li>
<li><a href="https://example.test/ungoogled-chromium_%s_windows_%s.zip">zip</a><ul>
<li>MD5: <code>0000</code></li>
<li>SHA256: <code>%s</code></li></ul></li>
</ul></body></html>`, version, version, token, sha)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/windows/" + dir + "/":
			_, _ = w.Write([]byte(index))
		case "/releases/windows/" + dir + "/" + version:
			_, _ = w.Write([]byte(release))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := ucBinaries{platformDirs: dirFor(arch), baseURL: srv.URL, httpClient: srv.Client()}
	spec := appSpec{id: "ungoogled-chromium", src: src}

	rels, err := src.versions(context.Background(), testConfig(), spec, 3)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("应有 1 个版本，实际 %d: %+v", len(rels), rels)
	}
	rel := rels[0]
	if rel.Version != version {
		t.Errorf("版本号不对: %q", rel.Version)
	}
	if rel.PublishedAt.IsZero() {
		t.Error("发布时间没传下去")
	}
	if !strings.Contains(rel.Notes, "贡献者甲") || !strings.Contains(rel.Notes, "非官方构建") {
		t.Errorf("版本说明应交代二进制来源: %q", rel.Notes)
	}
	if len(rel.Artifacts) != 1 {
		t.Fatalf("应只给一个产物（宿主只用 Artifacts[0]），实际 %+v", rel.Artifacts)
	}
	art := rel.Artifacts[0]
	if !strings.HasSuffix(art.Name, "_windows_"+token+".zip") {
		t.Errorf("挑错了产物（架构不匹配就会装错包）: %s", art.Name)
	}
	if art.URL != "https://example.test/ungoogled-chromium_"+version+"_windows_"+token+".zip" {
		t.Errorf("下载地址不对: %s", art.URL)
	}
	if art.Digest != "sha256:"+sha {
		t.Errorf("摘要应以 sha256: 前缀交给宿主: %s", art.Digest)
	}
}

// 站点没有这个架构的目录时应当明确报「找不到」，而不是返回空列表被当成「没有新版本」。
func TestUCBinariesSourceWithoutPlatformFails(t *testing.T) {
	src := ucBinaries{platformDirs: map[string]string{}}
	spec := appSpec{id: "ungoogled-chromium", src: src}
	if _, err := src.versions(context.Background(), testConfig(), spec, 1); err == nil {
		t.Fatal("没有对应平台目录时必须报错")
	} else if !strings.Contains(err.Error(), "索引站没有") {
		t.Fatalf("错误信息应说明原因，实际: %v", err)
	}
}

func dirFor(arch string) map[string]string {
	switch arch {
	case "amd64":
		return map[string]string{"amd64": "64bit"}
	case "arm64":
		return map[string]string{"arm64": "arm64"}
	}
	return map[string]string{}
}

// TestLiveCatalogSources 对真实上游跑一遍，确认「这个软件现在真的查得到版本与产物」。
// 解析逻辑本身由 internal/ucbinaries 的用例负责，这里验的是端到端接通。
//
// 默认跳过（CI 不联网），本机能访问站点时：
//
//	UPKIT_HUB_LIVE=1 go test ./cmd/upkit-hub -run Live -v
func TestLiveCatalogSources(t *testing.T) {
	if os.Getenv("UPKIT_HUB_LIVE") == "" {
		t.Skip("设置 UPKIT_HUB_LIVE=1 才跑联网用例")
	}
	for _, spec := range catalog {
		// GitHub API 在部分网络环境不可达，联网用例跳过 githubReleases；
		// 其余上游都必须真的查得出「版本 + 带摘要的产物」。
		switch spec.src.(type) {
		case ucBinaries, softwareHub, vscodeUpdate:
		default:
			continue
		}
		rels, err := spec.src.versions(context.Background(), testConfig(), spec, 2)
		if err != nil {
			t.Fatalf("%s: %v", spec.id, err)
		}
		for _, rel := range rels {
			if len(rel.Artifacts) != 1 {
				t.Fatalf("%s v%s: 应只给一个产物，实际 %+v", spec.id, rel.Version, rel.Artifacts)
			}
			art := rel.Artifacts[0]
			if !strings.HasPrefix(art.Digest, "sha256:") || len(art.Digest) != len("sha256:")+64 {
				t.Errorf("%s v%s: 摘要格式不对: %q", spec.id, rel.Version, art.Digest)
			}
			t.Logf("%s v%s -> %s（%s…）", spec.id, rel.Version, art.Name, art.Digest[:22])
		}
	}
}

// 构建期摘要表必须覆盖每个用它做摘要来源的软件：漏一条，那个软件在用户那边就会
// 「查得到版本却装不了」。这条要联网（清单里的地址来自站点上的当前版本）。
//
//	UPKIT_HUB_LIVE=1 go test ./cmd/upkit-hub -run Live -v
func TestLivePinnedDigestsCoverCatalog(t *testing.T) {
	if os.Getenv("UPKIT_HUB_LIVE") == "" {
		t.Skip("设置 UPKIT_HUB_LIVE=1 才跑联网用例")
	}
	table, err := pinnedTable()
	if err != nil {
		t.Fatalf("读取构建期摘要表失败: %v", err)
	}
	if len(table) == 0 {
		t.Skip("摘要表还是空的，先跑 scripts/gen-digests.sh")
	}
	client := &softwarehub.Client{}
	for _, spec := range catalog {
		src, ok := spec.src.(softwareHub)
		if !ok {
			continue
		}
		if _, ok := src.digest.(pinnedDigest); !ok {
			continue
		}
		payload, err := client.Versions(context.Background(), src.siteID)
		if err != nil {
			t.Errorf("%s: 读站点数据失败: %v", spec.id, err)
			continue
		}
		arches := make([]string, 0, len(src.arch))
		for arch := range src.arch {
			arches = append(arches, arch)
		}
		sort.Strings(arches)
		for _, arch := range arches {
			pick, err := payload.Pick(arch)
			if err != nil {
				t.Errorf("%s/%s: %v", spec.id, arch, err)
				continue
			}
			names, _, ok := src.pickArch(arch)
			if !ok {
				t.Errorf("%s/%s: 没有架构命名片段", spec.id, arch)
				continue
			}
			rawURL, err := src.artifactURL(pick, names)
			if err != nil {
				t.Errorf("%s/%s: %v", spec.id, arch, err)
				continue
			}
			if _, ok := table[rawURL]; !ok {
				t.Errorf("%s/%s 的产物（v%s）不在构建期摘要表里：需要重新生成摘要表；%s",
					spec.id, arch, pick.Version, rawURL)
			}
		}
	}
}

// 清单里的域名声明由上游推导，这里盯两件事：每个软件都报得出域名，且汇总出的主机名
// 都是合法形式 —— 写错了要等到发布门禁那一步才报错，那时已经晚了。
func TestDeclarationsAreDerivableAndValid(t *testing.T) {
	for _, spec := range catalog {
		download, pluginOwn := spec.src.hosts()
		if len(download) == 0 {
			t.Errorf("%s 没有声明任何下载域名（宿主会拒绝它给出的每一个地址）", spec.id)
		}
		for _, h := range append(append([]string{}, download...), pluginOwn...) {
			if !feedcheck.ValidHost(h) {
				t.Errorf("%s 声明了不合法的主机名 %q", spec.id, h)
			}
		}
	}

	d := Declarations()
	if len(d.Download) == 0 || len(d.Plugin) == 0 {
		t.Fatalf("声明不应为空: %+v", d)
	}
	// 排序 + 去重：清单是给人看的，输出稳定才好核对。
	if !sort.StringsAreSorted(d.Download) || !sort.StringsAreSorted(d.Plugin) {
		t.Errorf("声明应当排序: %+v", d)
	}
	for _, h := range append(append([]string{}, d.Download...), d.Plugin...) {
		if !feedcheck.ValidHost(h) {
			t.Errorf("汇总出的主机名不合法: %q", h)
		}
	}

	// Fragment 是给 gen-feed.sh 直接接在命令行后面的：两行、值里不含空格，
	// 调用方靠词分割展开它。
	frag := d.Fragment()
	for _, want := range []string{"--download-hosts=", "--plugin-hosts="} {
		if !strings.Contains(frag, want) {
			t.Errorf("声明片段里应包含 %q，实际:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, " ") {
		t.Errorf("声明片段里不该有空格（调用方靠词分割展开它）:\n%s", frag)
	}
}
