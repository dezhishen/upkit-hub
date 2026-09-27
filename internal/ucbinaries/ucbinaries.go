// Package ucbinaries 读取 ungoogled-chromium 的官方二进制索引站
// （https://ungoogled-software.github.io/ungoogled-chromium-binaries/）。
//
// 那个站点是**社区贡献者提交二进制的汇聚点**：同一个版本可能来自官方 Actions
// （ungoogled-software/ungoogled-chromium-windows），也可能来自某位贡献者自己的
// 仓库。索引页列出版本，版本页列出每个文件的直链与 MD5/SHA1/SHA256。
//
// 为什么不直接查 GitHub Releases API：
//
//   - 贡献者的仓库各不相同，钉死一个仓库会漏掉别人提交的版本；
//   - 站点的每个文件都带 SHA256，可以直接交给宿主做下载后校验；GitHub API 的
//     digest 字段只有较新上传的资产才有。
//
// 代价是索引是 HTML：列版本 1 次请求，取一个版本的产物 1 次请求。所以调用方要给出
// 想要的版本数（Releases 会并发抓取，且有上限），而不是把索引里的几百个版本全抓一遍。
package ucbinaries

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

const (
	// DefaultBaseURL 是索引站的根地址。
	DefaultBaseURL = "https://ungoogled-software.github.io/ungoogled-chromium-binaries"

	// PlatformWindows64 与 PlatformWindowsARM64 是站点上 Windows 的平台目录名，
	// 对应 /releases/windows/<目录>/。
	PlatformWindows64    = "64bit"
	PlatformWindowsARM64 = "arm64"

	// DefaultVersions 是 limit<=0 时返回的版本数。
	DefaultVersions = 10
	// MaxVersions 是任何情况下返回的版本数上限。
	//
	// 每个版本要抓一页，不设上限的话一次版本查询会对着站点打出几百个请求 ——
	// 索引里有一百多个历史版本。
	MaxVersions = 20

	maxPageBytes       = 4 << 20
	defaultConcurrency = 4

	// maxAttempts 与 retryDelay 只作用于传输层错误（连接被掐掉之类）。
	maxAttempts = 2
	retryDelay  = 300 * time.Millisecond
)

// Client 是索引站的只读客户端。
type Client struct {
	// BaseURL 留空时用 DefaultBaseURL（测试时会指向本地服务）。
	BaseURL string
	// HTTP 留空时用默认客户端（30 秒超时）。
	HTTP *http.Client
	// Concurrency 是并发抓取版本页的数量，留空用 4。
	Concurrency int
}

// File 是某个版本页里的一个可下载文件。
type File struct {
	Name   string
	URL    string
	SHA256 string
}

// Release 是一个版本页解析出来的内容。
type Release struct {
	// Version 是站点上的版本名（目录名），例如 153.0.8010.52-1。
	//
	// 注意它与文件里的打包版本可能不同：github-actions 发布的文件叫
	// ..._153.0.8010.52-1.1_windows_x64.zip，目录名却是 153.0.8010.52-1。
	// 目录名才是站点上稳定的标识，所以以它为准。
	Version     string
	Author      string
	PublishedAt time.Time
	Files       []File
}

// Picked 是一个版本与从它里面挑出来的产物。
type Picked struct {
	Release Release
	File    File
}

// Versions 返回某个平台的版本名（新 → 旧），最多 limit 个。
func (c *Client) Versions(ctx context.Context, platform string, limit int) ([]string, error) {
	page, err := c.get(ctx, c.platformURL(platform))
	if err != nil {
		return nil, err
	}
	all, err := ParseVersions(page, platform)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("索引页里没有 %s 平台的任何版本（页面结构可能变了）", platform)
	}
	return LimitVersions(all, limit), nil
}

// Release 抓取并解析某个版本的页面。
func (c *Client) Release(ctx context.Context, platform, version string) (Release, error) {
	page, err := c.get(ctx, c.releaseURL(platform, version))
	if err != nil {
		return Release{}, err
	}
	rel, err := ParseRelease(page)
	if err != nil {
		return Release{}, err
	}
	rel.Version = version
	return rel, nil
}

// Releases 抓取最近 limit 个版本的详情，并只保留 pick 能挑出产物的版本。
//
// 单个版本页失败（404、站点抖动）不影响其它版本：跳过它继续。全都挑不出来才算失败。
func (c *Client) Releases(ctx context.Context, platform string, limit int, pick func(Release) (File, bool)) ([]Picked, error) {
	names, err := c.Versions(ctx, platform, limit)
	if err != nil {
		return nil, err
	}

	type slot struct {
		picked Picked
		ok     bool
	}
	slots := make([]slot, len(names))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, c.concurrency())
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			rel, err := c.Release(ctx, platform, name)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			file, ok := pick(rel)
			if !ok {
				return
			}
			slots[i] = slot{picked: Picked{Release: rel, File: file}, ok: true}
		}(i, name)
	}
	wg.Wait()

	out := make([]Picked, 0, len(names))
	for _, s := range slots {
		if s.ok {
			out = append(out, s.picked)
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("最近 %d 个版本都取不到可用产物（例如 %w）", len(names), firstErr)
		}
		return nil, fmt.Errorf("最近 %d 个版本里都没有符合要求的产物", len(names))
	}
	return out, nil
}

// PickPortable 挑出该版本在指定 GOARCH 上的便携 zip。
//
// 两个筛选条件都是刻意的：
//
//   - 只要便携 zip，不要安装器 —— upkit 自己负责解包与落地，安装器会绕开它的
//     备份与回滚；
//   - 没有 SHA256 的文件不选。摘要交给宿主才有意义，宁可不提供这个版本，也不给
//     一个无法校验的下载。
//
// 名字里的分隔符各家贡献者不统一（windows_x64.zip / windows-x64.zip），所以比较前
// 统一成下划线。
func (r Release) PickPortable(goarch string) (File, bool) {
	tokens, ok := archTokens[goarch]
	if !ok {
		return File{}, false
	}
	for _, f := range r.Files {
		name := normalizeName(f.Name)
		if !strings.HasSuffix(name, ".zip") || strings.Contains(name, "installer") {
			continue
		}
		if !ValidSHA256(f.SHA256) {
			continue
		}
		for _, token := range tokens {
			if strings.HasSuffix(name, "_"+token+".zip") {
				return f, true
			}
		}
	}
	return File{}, false
}

// archTokens 是 GOARCH → 上游文件名里可能出现的架构片段，按优先级排列。
var archTokens = map[string][]string{
	"amd64": {"x64", "amd64", "win64"},
	"arm64": {"arm64", "aarch64"},
}

// LimitVersions 把版本列表裁剪到 limit 个（<=0 用 DefaultVersions，且不超过 MaxVersions）。
func LimitVersions(names []string, limit int) []string {
	n := limit
	if n <= 0 {
		n = DefaultVersions
	}
	if n > MaxVersions {
		n = MaxVersions
	}
	if n > len(names) {
		n = len(names)
	}
	return names[:n]
}

// ParseVersions 从索引页里抽出全部版本名，保持页面顺序（新 → 旧）。
//
// 页面里除了版本列表还有导航链接，所以只认含
// "<base>/releases/windows/<platform>/" 的 href，并取其后的一段作为版本名。
func ParseVersions(page []byte, platform string) ([]string, error) {
	marker := "/releases/windows/" + platform + "/"
	seen := make(map[string]bool)
	out := make([]string, 0, 16)

	z := html.NewTokenizer(bytes.NewReader(page))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if z.Err() == io.EOF {
				break
			}
			return nil, fmt.Errorf("解析索引页: %w", z.Err())
		}
		if tt != html.StartTagToken {
			continue
		}
		if name, _ := z.TagName(); string(name) != "a" {
			continue
		}
		href := attr(z, "href")
		i := strings.Index(href, marker)
		if i < 0 {
			continue
		}
		version := strings.Trim(href[i+len(marker):], "/")
		if version == "" || strings.Contains(version, "/") || seen[version] {
			continue
		}
		seen[version] = true
		out = append(out, version)
	}
	return out, nil
}

// ParseRelease 从版本页里抽出贡献者、发布时间与产物清单。
//
// 页面是站点生成的固定结构：
//
//	<h2>Release Information</h2> ... <li>Author: <a>名字</a></li>
//	                    ... <li>Publication time (in UTC): <code>时间</code></li>
//	<h2>Downloads</h2>
//	<li><a href="直链">文件名</a><ul><li>MD5: <code>…</code></li>
//	    <li>SHA1: …</li><li>SHA256: <code>…</code></li></ul></li>
//
// 文件名一律取自直链的最后一段，而不是链接文字：链接文字是给人看的，直链才是要下载
// 的东西，两者不一致时应当以直链为准。
func ParseRelease(page []byte) (Release, error) {
	var (
		rel        Release
		files      []File
		byURL      = make(map[string]int)
		cur        = -1
		wantSHA    bool
		wantAuthor bool
		wantTime   bool
		inLink     bool
	)

	z := html.NewTokenizer(bytes.NewReader(page))
loop:
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if z.Err() == io.EOF {
				break loop
			}
			return Release{}, fmt.Errorf("解析版本页: %w", z.Err())

		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) == "a" {
				inLink = true
				if f, ok := fileFromHref(attr(z, "href")); ok {
					if idx, dup := byURL[f.URL]; dup {
						cur = idx
					} else {
						files = append(files, f)
						byURL[f.URL] = len(files) - 1
						cur = len(files) - 1
					}
				} else {
					// 导航与作者之类的链接：后面的摘要值不属于任何产物。
					cur = -1
				}
			}

		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "a" {
				inLink = false
			}

		case html.TextToken:
			text := strings.TrimSpace(string(z.Text()))
			if text == "" {
				continue
			}
			// 先消费「上一段文字开了个头」的值，再看别处。
			switch {
			case wantAuthor:
				rel.Author = text
				wantAuthor = false
				continue
			case wantTime:
				if ts, err := parsePublishedAt(text); err == nil {
					rel.PublishedAt = ts
				}
				wantTime = false
				continue
			}
			if inLink {
				continue
			}
			switch {
			case hasLabel(text, "SHA256"):
				if value, ok := hexAfterLabel(text, "SHA256"); ok {
					if cur >= 0 {
						files[cur].SHA256 = value
					}
					continue
				}
				wantSHA = true
			case wantSHA && isHex64(text):
				if cur >= 0 {
					files[cur].SHA256 = strings.ToLower(text)
				}
				wantSHA = false
			case hasLabel(text, "Author"):
				rel.Author = ""
				wantAuthor = true
			case hasLabel(text, "Publication time"):
				wantTime = true
			}
		}
	}

	rel.Files = files
	return rel, nil
}

// ValidSHA256 报告摘要是否是合法的 sha256（允许 "sha256:" 前缀）。
func ValidSHA256(digest string) bool {
	d := strings.ToLower(strings.TrimSpace(digest))
	d = strings.TrimPrefix(d, "sha256:")
	return isHex64(d)
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// fileFromHref 判断一个链接是不是可下载的产物，是则给出文件名与地址。
func fileFromHref(href string) (File, bool) {
	href = strings.TrimSpace(href)
	u, err := url.Parse(href)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return File{}, false
	}
	name := path.Base(u.Path)
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".zip") && !strings.HasSuffix(lower, ".exe") {
		return File{}, false
	}
	return File{Name: name, URL: href}, true
}

// normalizeName 把文件名统一成小写、`-` 换成 `_`，便于跨贡献者比较命名。
func normalizeName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
}

// hasLabel 报告这段文字是不是某个字段的标签（"Author: "、"SHA256: "），
// 也接受值与标签挤在同一段文字里的写法。
func hasLabel(text, label string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), label)
}

// hexAfterLabel 从 "SHA256: <64 位十六进制>" 这种同一段文字的写法里取值。
func hexAfterLabel(text, label string) (string, bool) {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), label))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, ":"))
	if isHex64(rest) {
		return strings.ToLower(rest), true
	}
	return "", false
}

// parsePublishedAt 解析页面里的发布时间（形如 2026-09-20 17:27:14.964249+00:00）。
func parsePublishedAt(text string) (time.Time, error) {
	text = strings.TrimSpace(text)
	layouts := []string{
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, layout := range layouts {
		if ts, err := time.Parse(layout, text); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析发布时间 %q", text)
}

func (c *Client) concurrency() int {
	if c.Concurrency > 0 {
		return c.Concurrency
	}
	return defaultConcurrency
}

func (c *Client) base() string {
	b := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if b == "" {
		b = DefaultBaseURL
	}
	return b
}

func (c *Client) platformURL(platform string) string {
	return c.base() + "/releases/windows/" + platform + "/"
}

func (c *Client) releaseURL(platform, version string) string {
	return c.base() + "/releases/windows/" + platform + "/" + url.PathEscape(version)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "upkit-hub/ucbinaries")
	req.Header.Set("Accept", "text/html")

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 站点挂在 GitHub Pages 后面，偶尔会直接把连接掐掉（EOF）。GET 是幂等的，
			// 隔一下重试一次就能过去；不该让用户看到一次抖动就是「查询失败」。
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		// 无 body 的 GET 可以复用同一个请求。
		resp, err := c.httpClient().Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		page, err := readPage(resp, rawURL)
		if err != nil {
			// 状态码不对、页面过大：重试没有意义。
			return nil, err
		}
		return page, nil
	}
	return nil, fmt.Errorf("请求 %s 失败: %w", rawURL, lastErr)
}

// readPage 读取响应体（限制大小），非 200 直接报错。
func readPage(resp *http.Response, rawURL string) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("请求 %s 失败: HTTP %d", rawURL, resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", rawURL, err)
	}
	if len(page) > maxPageBytes {
		return nil, fmt.Errorf("页面 %s 超过 %d 字节上限", rawURL, maxPageBytes)
	}
	return page, nil
}

// attr 读取当前开始标签上的属性值。
func attr(z *html.Tokenizer, key string) string {
	for {
		k, v, more := z.TagAttr()
		if string(k) == key {
			return string(v)
		}
		if !more {
			return ""
		}
	}
}
