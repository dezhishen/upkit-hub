package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/dezhishen/upkit/pkg/plugin"
)

// githubReleases 从 GitHub Releases 里取版本与产物。
//
// asset 是资产名通配（path.Match 语法），其中 {arch} 会替换成本机架构在上游命名里的
// 片段。上游大多按架构分文件（windows_amd64 / windows_arm64 / x64），把架构写死会让
// 另一个架构的机器挑不到包 —— 而宿主只用 Artifacts[0]，挑错了没有第二次机会。
type githubReleases struct {
	repo  string // owner/name
	asset string
	// stableOnly 为真时丢掉预发布：有些仓库（Git for Windows）的 RC 比正式版还
	// 频繁，而宿主默认拿列表里的第一条 —— 不挡掉，用户点下去装的就是 RC。
	stableOnly bool
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// listReleases 查询某个仓库最近的发布，并挑出匹配当前平台的产物。
func (s githubReleases) versions(ctx context.Context, cfg plugin.AppConfig, spec appSpec, limit int) ([]plugin.Release, error) {
	if limit <= 0 || limit > 30 {
		limit = 10
	}
	// 只要正式版时多要一些：Git for Windows 的 RC 比正式版还密集，按 limit 条去问，
	// 很可能一条正式版都翻不到（结果就是「查不到版本」）。
	fetch := limit
	if s.stableOnly {
		fetch = limit * 4
		if fetch > 60 {
			fetch = 60
		}
	}
	pattern := assetPattern(s.asset, spec.archToken())
	api := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", s.repo, fetch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "upkit-hub")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 %s 的发布失败: %w", s.repo, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: 仓库 %s 不存在", plugin.ErrNotFound, s.repo)
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: GitHub 限流，请稍后再试或配置令牌", plugin.ErrRateLimited)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}

	var raw []struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Body        string    `json:"body"`
		Prerelease  bool      `json:"prerelease"`
		Draft       bool      `json:"draft"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析 GitHub 应答: %w", err)
	}

	out := make([]plugin.Release, 0, len(raw))
	for _, r := range raw {
		if r.Draft {
			continue
		}
		if s.stableOnly && r.Prerelease {
			continue
		}
		arts := make([]plugin.Artifact, 0, len(r.Assets))
		for _, a := range r.Assets {
			if !strings.HasSuffix(strings.ToLower(a.Name), ".zip") && !strings.HasSuffix(strings.ToLower(a.Name), ".exe") {
				continue
			}
			if !matchAsset(pattern, a.Name) {
				continue
			}
			arts = append(arts, plugin.Artifact{
				Name:   a.Name,
				URL:    a.URL,
				Size:   a.Size,
				Digest: normalizeDigest(a.Digest),
			})
		}
		if len(arts) == 0 {
			continue // 这个版本没有当前平台的产物
		}
		out = append(out, plugin.Release{
			Version:     strings.TrimPrefix(r.TagName, "v"),
			Tag:         r.TagName,
			Channel:     channelOf(r.Prerelease),
			PublishedAt: r.PublishedAt,
			Notes:       truncate(r.Body, 400),
			Artifacts:   arts,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s 最近的发布里没有匹配 %q 的产物（检查资产名通配或平台）",
			plugin.ErrNotFound, s.repo, pattern)
	}
	// 多要的那几条只是为了穿过预发布；按调用方要的条数返回。
	if len(out) > limit {
		out = out[:limit]
	}
	cfg.Log.Info("查询完成", "app", spec.id, "releases", len(out), "latest", out[0].Version)
	return out, nil
}

// hosts 报告这个上游用到的域名。
//
//   - 下载：GitHub 的 Release 资产始终从 github.com 下发（会 302 到 CDN，但重定向
//     目标不写进声明 —— 那是 GitHub 的实现细节，历史上变过，写死只会跟着过期；
//     跳转后的字节由 sha256 兜底）。
//   - 插件自有：版本查询走 api.github.com，由插件进程自己发起，宿主拦不住。
func (s githubReleases) hosts() (download, pluginOwn []string) {
	return []string{"github.com"}, []string{"api.github.com"}
}

// assetPattern 把资产名通配里的 {arch} 展开成上游命名里的架构片段。
func assetPattern(pattern, token string) string {
	return strings.ReplaceAll(pattern, "{arch}", token)
}

// matchAsset 用 path.Match 的通配语法匹配资产名；空模式表示匹配任意。
func matchAsset(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	ok, err := path.Match(pattern, name)
	if err == nil && ok {
		return true
	}
	// 资产名里常带斜杠以外的特殊字符，退化成"逐段包含"更实用。
	return containsFold(name, strings.ReplaceAll(pattern, "*", ""))
}

func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// channelOf 把预发布标记转成渠道名。
func channelOf(prerelease bool) string {
	if prerelease {
		return "beta"
	}
	return "stable"
}

// normalizeDigest 统一成宿主认识的 "sha256:..." 形式。
func normalizeDigest(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return ""
	}
	if strings.Contains(d, ":") {
		return d
	}
	return "sha256:" + d
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
