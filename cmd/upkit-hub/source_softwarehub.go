package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
// 抓取省事得多。但它**只提供版本**（外加一条参考地址），剩下两样 upkit 需要的东西都得
// 在 catalog 里补：
//
//   - 地址：站点给的是它自己挑的镜像/CDN 地址，未必不可变，也可能带不可预测的片段；
//   - 摘要：站点一个摘要都没有。没有摘要就不发布产物，所以每条接入都必须声明 digest。
//
// 产物地址有两种取得方式，按上游形态二选一：
//
//   - urlTemplate（推荐）：站点给的路径带内容哈希、日期之类的片段时合成不出来，但路径
//     规律的就用这个 —— 由 origin + urlTemplate 按官方命名规则合成，站点给的文件名拿来做
//     交叉校验；
//   - 照站点给的地址下载：路径里确实有合成不出来的片段（QQ、钉钉的 CDN 就是这样）时
//     只能这样，但**必须**声明 downloadHost，运行时核对地址确实落在那个域名上 —— 不然
//     清单里的域名声明就是静态推不出来的空话。
//
// 还有一个站点的固有局限会如实写进版本说明：它只保留**当前**版本，所以从这里接进来的
// 软件装不了旧版。
type softwareHub struct {
	// siteID 是站点上的 softwareId（与 catalog 里的 id 保持一致，便于两份清单对照）。
	siteID string
	// origin 是产物的官方来源（协议 + 主机），配合 urlTemplate 使用。
	origin string
	// urlTemplate 是产物在 origin 上的路径，以 / 开头，可用占位符：
	//
	//	{version}   版本号（站点给的原文，如 26.8.0）
	//	{archDir}   架构目录名（x86_64 / aarch64）
	//	{archFile}  文件名里的架构片段（x86-64 / aarch64）
	urlTemplate string
	// downloadHost 是「照站点地址下载」时，产物必须落在的域名。
	downloadHost string
	// arch 是 GOARCH → 官方命名里的两个架构片段（用站点地址时也要有，用来判断
	// 这台机器到底装不装得了）。
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
	if strings.TrimSpace(s.origin) == "" && strings.TrimSpace(s.downloadHost) == "" {
		return nil, fmt.Errorf("软件 %s 既没声明 origin 也没声明 downloadHost：产物来自哪里必须写死", spec.id)
	}
	if s.digest == nil {
		return nil, fmt.Errorf("软件 %s 没有声明摘要来源：没有摘要就不发布产物", spec.id)
	}
	if len(s.arch) == 0 {
		return nil, fmt.Errorf("软件 %s 没有声明架构映射：挑错架构就是装错包", spec.id)
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
	names, arch, ok := s.pickArch(runtime.GOARCH)
	if !ok {
		return nil, fmt.Errorf("%w: 官方没有 Windows %s 的安装包", plugin.ErrNotFound, runtime.GOARCH)
	}

	rawURL, err := s.artifactURL(pick, names)
	if err != nil {
		return nil, err
	}
	sum, err := s.digest.digest(ctx, digestRef{URL: rawURL, Version: pick.Version})
	if err != nil {
		return nil, fmt.Errorf("软件 %s v%s 的产物拿不到摘要，拒绝安装：%w", spec.id, pick.Version, err)
	}

	notes := []string{fmt.Sprintf("版本来自下载导航站（%s），产物与摘要取自 %s",
		softwarehub.HostOf(client.Base()), strings.Join(s.artifactHosts(), "、"))}
	if arch != runtime.GOARCH {
		notes = append(notes, fmt.Sprintf("上游没有 Windows %s 的原生包，这里装的是 %s 的包（由系统兼容层运行）",
			runtime.GOARCH, arch))
	}
	if pick.Note != "" {
		notes = append(notes, pick.Note)
	}
	cfg.Log.Info("查询完成", "app", spec.id, "version", pick.Version, "arch", arch, "host", softwarehub.HostOf(rawURL))

	return []plugin.Release{{
		Version:     pick.Version,
		Tag:         pick.Version,
		Channel:     "stable",
		PublishedAt: pick.Date,
		Notes:       strings.Join(notes, "；"),
		Artifacts: []plugin.Artifact{{
			Name:   fileNameOf(rawURL),
			URL:    rawURL,
			Size:   s.artifactSize(ctx, rawURL),
			Digest: sum,
		}},
	}}, nil
}

// artifactURL 得到产物地址：能合成则合成，合成不了就用站点给的（并核对域名）。
func (s softwareHub) artifactURL(pick softwarehub.Pick, names archNames) (string, error) {
	if strings.TrimSpace(s.urlTemplate) == "" {
		// 用站点地址：域名必须与声明的完全一致，否则清单里的下载声明就成了空话。
		if want := strings.ToLower(strings.TrimSpace(s.downloadHost)); want != "" {
			if got := strings.ToLower(softwarehub.HostOf(pick.URL)); got != want {
				return "", fmt.Errorf("站点给的地址落在 %s，与声明的 downloadHost %s 不符：站点换了下载源，声明必须跟着改", got, want)
			}
		}
		return pick.URL, nil
	}
	if strings.TrimSpace(s.origin) == "" {
		return "", fmt.Errorf("声明了 urlTemplate 就必须同时声明 origin")
	}
	p := strings.NewReplacer(
		"{version}", pick.Version,
		"{archDir}", names.dir,
		"{archFile}", names.file,
	).Replace(s.urlTemplate)
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("urlTemplate %q 必须以 / 开头", s.urlTemplate)
	}
	if !strings.HasSuffix(p, ".exe") && !strings.HasSuffix(p, ".msi") && !strings.HasSuffix(p, ".zip") {
		return "", fmt.Errorf("urlTemplate %q 的后缀不是可安装的产物类型", s.urlTemplate)
	}
	rawURL := strings.TrimSuffix(s.origin, "/") + p
	// 交叉校验：站点的文件名必须与模板合成的一致。不一致说明上游改了命名规则，我们
	// 合成的地址多半已经 404 —— 这时候报错远好过让用户下载失败。
	//
	// 站点给的是转发地址（形如 ?product=xxx，路径里没有文件名）时跳过：能对就一定要
	// 对，没得对就不假装对过。
	if hasFileName(pick.URL) && fileNameOf(pick.URL) != fileNameOf(rawURL) {
		return "", fmt.Errorf("站点给的产物名是 %q，模板合成的是 %q（上游可能改了命名规则）",
			fileNameOf(pick.URL), fileNameOf(rawURL))
	}
	return rawURL, nil
}

// pickArch 返回本机架构在官方命名里的片段。
//
// 上游只发 x64 包时（这是常态），arm64 的机器回退到 amd64 的包 —— Windows 11 ARM 自带
// x64 兼容层，装得上也跑得起来；回退会写进版本说明，不让用户以为拿到的是原生版本。
func (s softwareHub) pickArch(goarch string) (archNames, string, bool) {
	for _, want := range archFallbackChain(goarch) {
		if names, ok := s.arch[want]; ok {
			return names, want, true
		}
	}
	return archNames{}, "", false
}

// archFallbackChain 返回可以接受的架构，按优先级排列。
func archFallbackChain(goarch string) []string {
	switch goarch {
	case "amd64":
		return []string{"amd64", "x86"}
	case "arm64":
		return []string{"arm64", "amd64", "x86"}
	default:
		return []string{goarch}
	}
}

// artifactHosts 是产物可能来自的域名（用于版本说明与域名声明）。
func (s softwareHub) artifactHosts() []string {
	if host := strings.TrimSpace(s.downloadHost); host != "" {
		return []string{host}
	}
	return []string{softwarehub.HostOf(s.origin)}
}

// hosts 报告这个上游用到的域名。
//
//   - 下载：产物来自声明的官方 origin / downloadHost；
//   - 插件自有：站点数据（版本索引）、产物所在处（同目录的校验文件），以及摘要来源
//     自己声明的域名（例如 Mozilla 的校验清单在另一个域名上）。
func (s softwareHub) hosts() (download, pluginOwn []string) {
	download = dedupeHosts(s.artifactHosts()...)
	pluginOwn = dedupeHosts(append([]string{softwarehub.HostOf(s.base())}, s.artifactHosts()...)...)
	if h, ok := s.digest.(hosted); ok {
		pluginOwn = dedupeHosts(append(pluginOwn, h.hosts()...)...)
	}
	return download, pluginOwn
}

// base 返回实际使用的数据根地址。
func (s softwareHub) base() string {
	if base := strings.TrimSpace(s.baseURL); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return softwarehub.DefaultBaseURL
}

// artifactSize 取产物大小：摘要来源知道就用它的（构建期摘要表下载过），
// 否则退回 HEAD 去问，都拿不到就 0（宿主显示为「未知」）。
func (s softwareHub) artifactSize(ctx context.Context, rawURL string) int64 {
	if sizer, ok := s.digest.(sized); ok {
		if size, ok := sizer.size(rawURL); ok {
			return size
		}
	}
	return contentLength(ctx, rawURL)
}

// digestRef 是「要算摘要的那个产物」：地址加版本号。
//
// 带上版本号是因为有的上游把摘要集中放在「每版一份」的校验清单里（Mozilla 的
// SHA256SUMS），那种地址要靠版本号才能拼出来。
type digestRef struct {
	URL     string
	Version string
}

// digestSource 说明「产物的摘要从哪里来」。
//
// 站点数据里没有摘要，而本仓库的规矩是「只发布带摘要的产物」—— 于是每个以站点为上游的
// 软件都必须在 catalog 里声明摘要来源。取不到摘要就不提供这个版本：宁可不装，也不要在
// 没有任何校验的前提下，往用户机器上落一个安装器。
type digestSource interface {
	digest(ctx context.Context, ref digestRef) (string, error)
}

// hosted 是摘要来源的可选能力：它自己会访问的域名（要写进清单的域名声明）。
type hosted interface {
	hosts() []string
}

// sha256Sidecar 从**同目录**的 .sha256 校验文件取摘要。
//
// 官方发布渠道常见的做法（LibreOffice 的每个 .msi 旁边就有一个 .msi.sha256），好处是
// 摘要与产物同源、随版本一起更新，不需要我们另外维护一张按 URL 记账的摘要表 —— 那种表
// 一旦上游换了文件就会静默失效。
//
// 注意：不是所有「.sha256 返回 200」都真的是校验文件 —— 有些站点对未知路径返回反爬页面
// （ToDesk 就是这样）。所以解析失败时报错，绝不将就。
type sha256Sidecar struct{}

func (sha256Sidecar) digest(ctx context.Context, ref digestRef) (string, error) {
	side := ref.URL + ".sha256"
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
	sum, err := findSHA256(string(body), ref.URL)
	if err != nil {
		return "", fmt.Errorf("校验文件 %s: %w", side, err)
	}
	return sum, nil
}

// sha256Sums 从上游发布的**校验清单**里取摘要（Mozilla 的 SHA256SUMS 就是这种）。
//
// 与 sha256Sidecar 的区别：清单不在产物旁边，而是每版一份，里面列着该版本所有产物的
// 摘要；地址里的 {version} 会换成站点给的版本号。
type sha256Sums struct {
	// urlTemplate 是校验清单的地址，可含 {version}。
	urlTemplate string
}

func (s sha256Sums) digest(ctx context.Context, ref digestRef) (string, error) {
	raw := s.url(ref.Version)
	resp, err := getWithRetry(ctx, raw)
	if err != nil {
		return "", fmt.Errorf("读取校验清单 %s 失败: %w", raw, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("上游没有发布校验清单 %s（HTTP %d）", raw, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("读取校验清单 %s 失败: %w", raw, err)
	}
	sum, err := findSHA256(string(body), ref.URL)
	if err != nil {
		return "", fmt.Errorf("校验清单 %s: %w", raw, err)
	}
	return sum, nil
}

// hosts 实现 hosted：Mozilla 的校验清单在 ftp 站，与产物不在同一个域名上。
func (s sha256Sums) hosts() []string {
	return dedupeHosts(softwarehub.HostOf(s.url("")))
}

// url 拼出校验清单的地址。
func (s sha256Sums) url(version string) string {
	return strings.ReplaceAll(s.urlTemplate, "{version}", version)
}

// findSHA256 在校验文件/清单里找出**属于某个产物**的那一条摘要。
//
// 两种常见写法都支持：`<hash>  <相对路径>`（Mozilla、LibreOffice）与
// `SHA256 (<文件名>) = <hash>`。前者按「整条路径后缀」精确匹配 —— 文件名里带空格时
// （`win64/zh-CN/Firefox Setup 156.0.1.msi`），按位置切字段会悄悄取到半个文件名。
func findSHA256(content, artifactURL string) (string, error) {
	want := decodedPath(artifactURL)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		// 写法一：`<hash>  <路径>`（GNU 风格会在路径前加 *）
		if len(line) > 64 && isSHA256Hex(line[:64]) {
			rest := strings.TrimLeft(line[64:], " \t*")
			if rest != "" && strings.HasSuffix(want, rest) {
				return "sha256:" + strings.ToLower(line[:64]), nil
			}
		}
		// 写法二：`SHA256 (<文件名>) = <hash>`
		if i := strings.Index(line, "= "); i >= 0 {
			if sum := strings.TrimSpace(line[i+2:]); isSHA256Hex(sum) &&
				strings.Contains(line[:i], fileNameOf(want)) {
				return "sha256:" + strings.ToLower(sum), nil
			}
		}
	}
	return "", fmt.Errorf("清单里没有 %s 的摘要（内容格式可能变了）", fileNameOf(want))
}

// decodedPath 取地址里解码后的路径，用于与校验清单里的相对路径比对。
func decodedPath(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return rawURL
	}
	return u.Path
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

// fileNameOf 取地址里的文件名（宿主用它做落盘的默认名字）；没有文件名时返回
// "package"。
func fileNameOf(rawURL string) string {
	p := strings.TrimSuffix(decodedPath(rawURL), "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if p == "" || p == "/" {
		return "package"
	}
	return p
}

// hasFileName 报告地址里是否真的带文件名。
//
// 上游常给「转发地址」（形如 https://download.mozilla.org/?product=xxx）：路径只有
// 一个 /，真正的文件名在跳转之后的地址里。这种地址没法拿来做文件名交叉校验。
func hasFileName(rawURL string) bool {
	return fileNameOf(rawURL) != "package"
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
