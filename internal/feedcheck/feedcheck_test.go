package feedcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这个包里有一份「最小复刻」的订阅校验（原因见 feedcheck.go 顶部注释）。副本最大的
// 风险是它悄悄比宿主宽松 —— 于是清单在本仓库一路绿灯、到用户那里才失败。所以下面
// 每个「宿主一定会拒绝」的情形都单独立了一个用例。

func digestOf(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写 %s: %v", name, err)
	}
	return p
}

// 严格模式：字段名写错必须直接报错，而不是被静默忽略。
//
// 订阅等于远程代码执行授权。拼错的限制项如果被忽略，会让人误以为它生效了 —— 那是
// 比缺这个功能更糟的状态。
func TestParseIsStrict(t *testing.T) {
	good := `
schema: 1
name: 测试源
plugins:
  - id: demo
    version: 1.0.0
    packages:
      windows/amd64:
        url: https://example.com/demo-windows-amd64.exe
        sha256: "` + strings.Repeat("a", 64) + `"
`
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("合法清单应当解析成功: %v", err)
	}

	bad := strings.Replace(good, "    version: 1.0.0", "    versions: 1.0.0", 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("字段名写错（version → versions）时必须报错")
	}

	if _, err := Parse([]byte("  \n")); err == nil {
		t.Fatal("空内容必须报错")
	}
}

func TestValidateRejectsHostileFeed(t *testing.T) {
	sha := strings.Repeat("a", 64)
	base := `
schema: 1
plugins:
  - id: demo
    version: 1.0.0
    packages:
      windows/amd64:
        url: https://example.com/demo-windows-amd64.exe
        sha256: "%s"
      windows/arm64:
        url: https://example.com/demo-windows-arm64.exe
        sha256: "%s"
`
	valid := fmt.Sprintf(base, sha, sha)

	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"缺少 schema", strings.Replace(valid, "schema: 1\n", "", 1), "schema"},
		{"schema 比宿主新", strings.Replace(valid, "schema: 1", "schema: 3", 1), "请升级 upkit"},
		{"没有插件", "schema: 1\nplugins: []\n", "没有任何插件"},
		{"id 非法", strings.Replace(valid, "id: demo", "id: ../evil", 1), "不合法"},
		{"缺一个架构", strings.Replace(valid, `
      windows/arm64:
        url: https://example.com/demo-windows-arm64.exe
        sha256: "`+sha+`"`, "", 1), "没有 windows/arm64 平台的包"},
		{"摘要太短", strings.Replace(valid, `sha256: "`+sha+`"`, `sha256: "abc"`, 1), "sha256"},
		{"摘要非法字符", strings.Replace(valid, `sha256: "`+sha+`"`, `sha256: "`+strings.Repeat("z", 64)+`"`, 1), "sha256"},
		{"版本号带路径", strings.Replace(valid, "version: 1.0.0", `version: "../../x"`, 1), "version"},
		{
			"协议相对地址",
			strings.Replace(valid, "https://example.com/demo-windows-amd64.exe", "//evil.example.com/demo-windows-amd64.exe", 1),
			"协议相对",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			feed, err := Parse([]byte(c.yaml))
			if err != nil {
				// 有些情形在解析阶段就被拦下（例如字段类型不对），同样算拦住。
				return
			}
			err = feed.ValidateAllPlatforms("1.0.0")
			if err == nil {
				t.Fatalf("必须拒绝这份清单:\n%s", c.yaml)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应提到 %q，实际: %v", c.want, err)
			}
		})
	}
}

// 重复 id 会让「本机已装的是哪一个」变得没有答案，宿主拒绝，这里也必须拒绝。
func TestValidateRejectsDuplicateID(t *testing.T) {
	sha := strings.Repeat("a", 64)
	pkg := fmt.Sprintf(`
    packages:
      windows/amd64:
        url: https://example.com/demo-windows-amd64.exe
        sha256: "%s"
      windows/arm64:
        url: https://example.com/demo-windows-arm64.exe
        sha256: "%s"
`, sha, sha)
	doc := "schema: 1\nplugins:\n  - id: demo\n    version: 1.0.0" + pkg + "  - id: demo\n    version: 2.0.0" + pkg
	feed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := feed.ValidateAllPlatforms("1.0.0"); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 id 必须被拒绝，实际: %v", err)
	}
}

// min_host_version 的判定必须与宿主同向：宿主更旧 → 该条目不发布。
func TestValidateMinHostVersion(t *testing.T) {
	sha := strings.Repeat("a", 64)
	doc := fmt.Sprintf(`
schema: 1
plugins:
  - id: demo
    version: 1.0.0
    min_host_version: "1.2.0"
    packages:
      windows/amd64:
        url: https://example.com/demo-windows-amd64.exe
        sha256: "%s"
      windows/arm64:
        url: https://example.com/demo-windows-arm64.exe
        sha256: "%s"
`, sha, sha)
	feed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := feed.ValidateAllPlatforms("1.1.9"); err == nil {
		t.Fatal("宿主 1.1.9 应被 min_host_version 1.2.0 拦下")
	}
	if err := feed.ValidateAllPlatforms("1.2.0"); err != nil {
		t.Fatalf("宿主 1.2.0 应当通过: %v", err)
	}
	if err := feed.ValidateAllPlatforms(""); err != nil {
		t.Fatalf("不指定宿主版本时不应做这项检查: %v", err)
	}
}

func TestCheckArtifacts(t *testing.T) {
	payload := []byte("real plugin bytes")
	dir := t.TempDir()
	writeFile(t, dir, "demo-windows-amd64.exe", payload)
	writeFile(t, dir, "demo-windows-arm64.exe", payload)

	sha := digestOf(t, payload)
	doc := fmt.Sprintf(`
schema: 1
plugins:
  - id: demo
    version: 1.0.0
    packages:
      windows/amd64:
        url: https://example.com/demo-windows-amd64.exe
        sha256: "%s"
        size: %d
      windows/arm64:
        url: https://example.com/demo-windows-arm64.exe
        sha256: "%s"
        size: %d
`, sha, len(payload), sha, len(payload))

	feed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := feed.CheckArtifacts(dir); err != nil {
		t.Fatalf("清单与产物一致时不应报错: %v", err)
	}

	// 摘要对不上（清单被手改，或产物被换过）。
	bad := strings.Replace(doc, sha, strings.Repeat("b", 64), 1)
	feedBad, err := Parse([]byte(bad))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := feedBad.CheckArtifacts(dir); err == nil {
		t.Fatal("摘要不一致时必须报错")
	}

	// size 对不上。
	feedSize, err := Parse([]byte(strings.Replace(doc, fmt.Sprintf("size: %d", len(payload)), "size: 1", 1)))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := feedSize.CheckArtifacts(dir); err == nil || !strings.Contains(err.Error(), "大小") {
		t.Fatalf("size 不一致时必须报错，实际: %v", err)
	}

	// 地址指向别的产物：URL 与平台不对应，用户会下到 404。
	feedURL, err := Parse([]byte(strings.Replace(doc, "demo-windows-amd64.exe", "other-windows-amd64.exe", 1)))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := feedURL.CheckArtifacts(dir); err == nil || !strings.Contains(err.Error(), "约定") {
		t.Fatalf("URL 与平台不对应时必须报错，实际: %v", err)
	}

	// 产物缺失（例如 artifact 上传漏了文件）。
	if err := feed.CheckArtifacts(t.TempDir()); err == nil {
		t.Fatal("产物缺失时必须报错")
	}
}

// 其它平台的包不参与本地比对，也不应当因此报错：一份清单可能同时服务别的工具。
func TestCheckArtifactsIgnoresForeignPlatforms(t *testing.T) {
	sha := strings.Repeat("a", 64)
	doc := fmt.Sprintf(`
schema: 1
plugins:
  - id: demo
    version: 1.0.0
    packages:
      windows/amd64:
        url: https://example.com/demo-windows-amd64.exe
        sha256: "%s"
      windows/arm64:
        url: https://example.com/demo-windows-arm64.exe
        sha256: "%s"
      linux/amd64:
        url: https://example.com/demo-linux-amd64
        sha256: "%s"
`, sha, sha, sha)
	feed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	dir := t.TempDir()
	writeFile(t, dir, "demo-windows-amd64.exe", []byte("x"))
	writeFile(t, dir, "demo-windows-arm64.exe", []byte("x"))
	if err := feed.CheckArtifacts(dir); err == nil {
		t.Fatal("Windows 产物摘要与清单不符（内容为 x），必须报错")
	}
}

func TestVerifyURLs(t *testing.T) {
	payload := []byte("served plugin bytes")
	sha := digestOf(t, payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/demo-") {
			_, _ = w.Write(payload)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	doc := fmt.Sprintf(`
schema: 1
plugins:
  - id: demo
    version: 1.0.0
    packages:
      windows/amd64:
        url: %s/demo-windows-amd64.exe
        sha256: "%s"
        size: %d
      windows/arm64:
        url: %s/demo-windows-arm64.exe
        sha256: "%s"
        size: %d
`, srv.URL, sha, len(payload), srv.URL, sha, len(payload))

	feed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := feed.VerifyURLs(context.Background(), srv.Client(), srv.URL+"/feed.yaml"); err != nil {
		t.Fatalf("内容与摘要一致时不应报错: %v", err)
	}

	// 摘要不对：说明地址上的东西不是发布的那份。
	wrong := strings.Replace(doc, sha, strings.Repeat("c", 64), 1)
	feedWrong, err := Parse([]byte(wrong))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := feedWrong.VerifyURLs(context.Background(), srv.Client(), srv.URL+"/feed.yaml"); err == nil {
		t.Fatal("下载内容与摘要不符时必须报错")
	}

	// 404：附件名写错或漏传。
	missing := strings.Replace(doc, "demo-windows-amd64.exe", "nope-windows-amd64.exe", -1)
	feedMissing, err := Parse([]byte(missing))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := feedMissing.VerifyURLs(context.Background(), srv.Client(), srv.URL+"/feed.yaml"); err == nil {
		t.Fatal("附件不存在时必须报错")
	}
}

// 相对地址（./<文件>）必须相对**清单位置**解析 —— 这正是内置订阅地址能直接用的原因：
// .../releases/latest/download/feed.yaml 与同一次发布的产物同源。
func TestResolveURL(t *testing.T) {
	const feed = "https://github.com/dezhishen/upkit-hub/releases/latest/download/feed.yaml"
	cases := []struct {
		name    string
		feedURL string
		raw     string
		want    string
		wantErr string
	}{
		{
			name: "绝对地址原样返回",
			raw:  "https://cdn.example.com/x.exe", want: "https://cdn.example.com/x.exe",
		},
		{
			name:    "相对地址相对清单位置解析",
			feedURL: feed, raw: "./x.exe",
			want: "https://github.com/dezhishen/upkit-hub/releases/latest/download/x.exe",
		},
		{
			name:    "不带 ./ 的相对地址同样成立",
			feedURL: feed, raw: "x.exe",
			want: "https://github.com/dezhishen/upkit-hub/releases/latest/download/x.exe",
		}, {
			// 这条锁住 gen-feed.sh 为何必须写 ./ 而非 /：以 / 开头是「相对 origin 根」，
			// 不是相对清单目录。对发布布局而言它会变成 https://github.com/x.exe —— 404。
			// 已在 upkit 侧用它的 ResolveLocation 实测确认，两边语义一致。
			name:    "根相对是相对 origin 根，不是清单目录",
			feedURL: feed, raw: "/x.exe",
			want: "https://github.com/x.exe",
		}, {
			name:    "协议相对地址会换域，必须拒绝",
			feedURL: feed, raw: "//evil.example.com/x.exe",
			wantErr: "不同源",
		},
		{
			name: "没有清单位置就解析不了相对地址",
			raw:  "./x.exe", wantErr: "-feed-url",
		},
		{
			name:    "不支持的协议",
			raw:     "ftp://example.com/x.exe",
			wantErr: "http/https",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveURL(c.feedURL, c.raw)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("应当报错（期望提到 %q），实际得到 %q", c.wantErr, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("错误信息应提到 %q，实际: %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != c.want {
				t.Fatalf("解析为 %q，期望 %q", got, c.want)
			}
		})
	}
}

func TestVerifyURLsRelative(t *testing.T) {
	payload := []byte("relative plugin bytes")
	sha := digestOf(t, payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "demo-windows-amd64.exe") || strings.HasSuffix(r.URL.Path, "demo-windows-arm64.exe") {
			_, _ = w.Write(payload)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	doc := fmt.Sprintf(`
schema: 1
plugins:
  - id: demo
    version: 1.0.0
    packages:
      windows/amd64:
        url: ./demo-windows-amd64.exe
        sha256: "%s"
      windows/arm64:
        url: ./demo-windows-arm64.exe
        sha256: "%s"
`, sha, sha)
	feed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if err := feed.VerifyURLs(context.Background(), srv.Client(), srv.URL+"/feed.yaml"); err != nil {
		t.Fatalf("相对地址应能解析并核对: %v", err)
	}

	if err := feed.VerifyURLs(context.Background(), srv.Client(), ""); err == nil {
		t.Fatal("不给清单位置时应当报错，而不是默默跳过")
	} else if !strings.Contains(err.Error(), "-feed-url") {
		t.Fatalf("错误信息应告诉用户怎么补: %v", err)
	}
}

func TestArtifactNameFollowsConvention(t *testing.T) {
	cases := []struct{ id, platform, want string }{
		{"upkit-hub", "windows/amd64", "upkit-hub-windows-amd64.exe"},
		{"upkit-hub", "windows/arm64", "upkit-hub-windows-arm64.exe"},
		{"corp.agent", "windows/amd64", "corp.agent-windows-amd64.exe"},
	}
	for _, c := range cases {
		if got := ArtifactName(c.id, c.platform); got != c.want {
			t.Errorf("ArtifactName(%q, %q) = %q，期望 %q", c.id, c.platform, got, c.want)
		}
	}
}

// 版本比较是 min_host_version 判定的基础：rc9 < rc10 这类边界弄错，就会把「要求更
// 新的宿主」判成满足，进而在更旧的宿主上放出一份装不了的插件。
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.4", -1},
		{"v1.2.0", "1.2.0", 0},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc2", "1.0.0-rc10", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"", "1.0.0", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}
