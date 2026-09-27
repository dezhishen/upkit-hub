package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/dezhishen/upkit-hub/internal/softwarehub"
	"github.com/dezhishen/upkit/pkg/plugin"
)

// softwareHub 用「常用软件下载导航」（dezhishen/original-software-hub）发布的静态
// JSON 做上游。
//
// 它替我们回答了「这个软件现在是什么版本」，而且每天自动更新 —— 比自己给每个网站写一遍
// 抓取省事得多。但它**只提供版本**，剩下两样 upkit 需要的东西都得在 catalog 里补：
//
//   - 地址：站点给的是它自己挑的镜像直链，路径里常常多一层镜像自己的目录
//     （LibreOffice 在腾讯云镜像上是 /libreoffice/libreoffice/stable/…，官方是
//     /libreoffice/stable/…），也未必是不可变地址。所以地址由 urlTemplate 按官方命名
//     规则合成；站点给的文件名会被拿来**交叉校验**，命名一变就报错，而不是静默 404；
//   - 摘要：站点只给链接，一个摘要都没有。没有摘要就不发布产物，所以每条接入都必须
//     声明 digest。
//
// 还有一个站点的固有局限会如实写进版本说明：它只保留**当前**版本，所以从这里接进来的
// 软件装不了旧版。
type softwareHub struct {
	// siteID 是站点上的 softwareId（与 catalog 里的 id 保持一致，便于两份清单对照）。
	siteID string
	// origin 是产物的官方来源（协议 + 主机）。
	//
	// 必填：清单里的下载域名声明是静态推导出来的，产物来自哪里必须写死在代码里。
	origin string
	// urlTemplate 是产物在 origin 上的路径，以 / 开头，可用占位符：
	//
	//	{version}   版本号（站点给的原文，如 26.8.0）
	//	{archDir}   架构目录名（x86_64 / aarch64）
	//	{archFile}  文件名里的架构片段（x86-64 / aarch64）
	//
	// 用模板而不是站点给的地址，是因为只有模板才能保证「地址带版本号」且与摘要同源。
	urlTemplate string
	// arch 是 GOARCH → 官方命名里的两个架构片段。
	arch map[string]archNames
	// digest 说明摘要从哪里来。必填。
	digest digestSource
	// baseURL 与 httpClient 留空用默认值（测试时指向本地服务）。
	baseURL    string
	httpClient *http.Client
}

// archNames 是上游对架构的两种叫法：目录名与文件名片段未必相同
// （LibreOffice 的目录是 x86_64，文件名里却是 x86-64）。
type archNames struct {
	dir  string
	file string
}

func (s softwareHub) versions(ctx context.Context, cfg plugin.AppConfig, spec appSpec, limit int) ([]plugin.Release, error) {
	names, ok := s.arch[runtime.GOARCH]
	if !ok {
		// 上游没有这个架构的包（站上绝大多数软件都只有 x64）：这台机器就是装不了，
		// 明确说清楚，而不是拿别的架构去凑。
		return nil, fmt.Errorf("%w: 官方没有 Windows %s 的安装包", plugin.ErrNotFound, runtime.GOARCH)
	}
	if strings.TrimSpace(s.origin) == "" || strings.TrimSpace(s.urlTemplate) == "" {
		return nil, fmt.Errorf("软件 %s 没有声明 origin 或 urlTemplate：产物地址必须写死，不能跟着站点走", spec.id)
	}
	if s.digest == nil {
		return nil, fmt.Errorf("软件 %s 没有声明摘要来源：没有摘要就不发布产物", spec.id)
	}

	client := &softwarehub.Client{BaseURL: s.baseURL, HTTP: s.httpClient}
	payload, err := client.Versions(ctx, s.siteID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", plugin.ErrNotFound, err)
	}
	pick, err := payload.Pick(runtime.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", plugin.ErrNotFound, err)
	}
	if softwarehub.PlaceholderVersion(pick.Version) {
		// 站点对「官方只提供一个永远指最新的地址」的软件会写 latest。那不是版本号：
		// 拿它当版本，宿主判断「该不该升级」时会永远得到「不用」，装了旧版也升不上去。
		return nil, fmt.Errorf("%w: 软件 %s 的版本字段是 %q，站点没有可比较的版本号，无法接入",
			plugin.ErrNotFound, spec.id, pick.Version)
	}

	rawURL, err := s.artifactURL(pick.Version, names)
	if err != nil {
		return nil, err
	}
	// 交叉校验：站点的文件名必须与模板合成的一致。不一致说明上游改了命名规则，我们
	// 合成的地址多半已经 404 —— 这时候报错远好过让用户下载失败。
	if want, got := fileNameOf(rawURL), fileNameOf(pick.URL); want != got {
		return nil, fmt.Errorf("软件 %s v%s 的产物命名对不上：站点给的是 %q，模板合成的是 %q（上游可能改了命名规则）",
			spec.id, pick.Version, got, want)
	}

	sum, err := s.digest.digest(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("软件 %s v%s 的产物拿不到摘要，拒绝安装：%w", spec.id, pick.Version, err)
	}

	notes := []string{fmt.Sprintf("版本来自下载导航站（%s），产物与摘要取自 %s",
		softwarehub.HostOf(client.Base()), softwarehub.HostOf(s.origin))}
	if pick.Note != "" {
		notes = append(notes, pick.Note)
	}
	cfg.Log.Info("查询完成", "app", spec.id, "version", pick.Version, "arch", names.file)

	return []plugin.Release{{
		Version:     pick.Version,
		Tag:         pick.Version,
		Channel:     "stable",
		PublishedAt: pick.Date,
		Notes:       strings.Join(notes, "；"),
		Artifacts: []plugin.Artifact{{
			Name:   fileNameOf(rawURL),
			URL:    rawURL,
			Size:   contentLength(ctx, rawURL),
			Digest: sum,
		}},
	}}, nil
}

// artifactURL 用站点给的版本号与官方命名规则合成产物地址。
func (s softwareHub) artifactURL(version string, names archNames) (string, error) {
	p := strings.NewReplacer(
		"{version}", version,
		"{archDir}", names.dir,
		"{archFile}", names.file,
	).Replace(s.urlTemplate)
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("urlTemplate %q 必须以 / 开头", s.urlTemplate)
	}
	if !strings.HasSuffix(p, ".exe") && !strings.HasSuffix(p, ".msi") && !strings.HasSuffix(p, ".zip") {
		return "", fmt.Errorf("urlTemplate %q 的后缀不是可安装的产物类型", s.urlTemplate)
	}
	return strings.TrimSuffix(s.origin, "/") + p, nil
}

// hosts 报告这个上游用到的域名。
//
//   - 下载：产物来自声明的官方 origin；
//   - 插件自有：站点数据（版本索引）与同目录的校验文件（也在 origin 上）。
func (s softwareHub) hosts() (download, pluginOwn []string) {
	download = []string{softwarehub.HostOf(s.origin)}
	pluginOwn = dedupeHosts(softwarehub.HostOf(s.base()), softwarehub.HostOf(s.origin))
	return download, pluginOwn
}

// base 返回实际使用的数据根地址。
func (s softwareHub) base() string {
	if base := strings.TrimSpace(s.baseURL); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return softwarehub.DefaultBaseURL
}

// digestSource 说明「产物的摘要从哪里来」。
//
// 站点数据里没有摘要，而本仓库的规矩是「只发布带摘要的产物」—— 于是每个以站点为上游的
// 软件都必须在 catalog 里声明摘要来源。取不到摘要就不提供这个版本：宁可不装，也不要在
// 没有任何校验的前提下，往用户机器上落一个安装器。
type digestSource interface {
	digest(ctx context.Context, rawURL string) (string, error)
}

// sha256Sidecar 从**同目录**的 .sha256 校验文件取摘要。
//
// 官方发布渠道常见的做法（LibreOffice 的每个 .msi 旁边就有一个 .msi.sha256），好处是
// 摘要与产物同源、随版本一起更新，不需要我们另外维护一张按 URL 记账的摘要表 —— 那种表
// 一旦上游换了文件就会静默失效。
type sha256Sidecar struct{}

func (sha256Sidecar) digest(ctx context.Context, rawURL string) (string, error) {
	side := rawURL + ".sha256"
	resp, err := getWithRetry(ctx, side)
	if err != nil {
		return "", fmt.Errorf("读取校验文件 %s 失败: %w", side, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("上游没有发布校验文件 %s（HTTP %d）", side, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", fmt.Errorf("读取校验文件 %s 失败: %w", side, err)
	}
	// 校验文件的写法不止一种（`<hash>  <文件名>`、`SHA256 (file) = <hash>`），
	// 所以直接扫出第一个 64 位十六进制串，而不是按位置取字段。
	for _, field := range strings.Fields(string(body)) {
		field = strings.Trim(field, "()=")
		if isSHA256Hex(field) {
			return "sha256:" + strings.ToLower(field), nil
		}
	}
	return "", fmt.Errorf("校验文件 %s 里没有 sha256 摘要（内容可能变了）", side)
}

// contentLength 尽力取产物大小，失败时返回 0（宿主把 0 显示成「未知」）。
//
// 大小只影响界面上「要下多大」的提示，所以这里不发 GET、也不重试：多花一次 HEAD 换一个
// 有用的提示是划算的，但上游不支持 HEAD 或链路抖动时，绝不能因此让版本查询失败。
func contentLength(ctx context.Context, rawURL string) int64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
		return 0
	}
	return resp.ContentLength
}

// getWithRetry 发一个 GET，对**传输层**错误重试。
//
// 上游在海外时链路偶发断开是常事（实测同一地址连打三次会断一次）—— 不重试的话，用户
// 看到的是「查不到版本」，而其实再试一次就好了。状态码错误不重试：404 就是没有。
func getWithRetry(ctx context.Context, rawURL string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < maxFetchAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(fetchRetryDelay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// 只作用于传输层错误的次数与间隔。
const (
	maxFetchAttempts = 2
	fetchRetryDelay  = 300 * time.Millisecond
)

// isSHA256Hex 报告字符串是否是 64 位十六进制摘要。
func isSHA256Hex(s string) bool {
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

// fileNameOf 取地址里的文件名（宿主用它做落盘的默认名字）。
func fileNameOf(rawURL string) string {
	trimmed := rawURL
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if name := path.Base(trimmed); name != "" && name != "/" && name != "." {
		return name
	}
	return "package"
}

// dedupeHosts 去掉空串与重复项，保持给定顺序。
func dedupeHosts(hosts ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}
