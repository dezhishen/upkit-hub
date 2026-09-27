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

// Command upkit-hub 是一个示例插件：把若干常用软件放进一条订阅里分发。
//
// 它是 catalog 模式：只回答「有哪些软件、有哪些版本、下载什么」，
// 下载 / 解包 / 落地 / 探测全部复用宿主内置的四条轴，因此这里不需要任何安装逻辑。
//
// 它也是可运行的开发样例：下面的 catalog 里每条都是真实可用的软件定义，
// 想看更小的起步例子见 cmd/upkit-plugin-example，想看 full 模式见它的 full.go。
// 版本从各自的 GitHub Releases 动态查询；想加一个软件，往 catalog 里加一条即可。

// version 是插件自身的版本号，由构建脚本用 -ldflags -X main.version 注入；
// 直接 go run / go build 时为 dev。发布流水线会把它设成与宿主同一个 tag 版本。
var version = "dev"

// appSpec 是示例里的一个软件条目。
type appSpec struct {
	id       string
	name     string
	desc     string
	homepage string
	repo     string // GitHub owner/name
	// asset 是资产名通配（path.Match 语法），用于在 release 里挑出当前平台的产物。
	asset string
	// unpack / method 交给宿主的四条轴。
	unpack string
	method string
	// installPath 支持 ${ROOT} 等宿主变量；entry 是复核安装结果用的可执行文件。
	installPath string
	entry       []string
	processes   []string
}

var catalog = []appSpec{
	{
		id:          "fzf",
		name:        "fzf",
		desc:        "命令行模糊查找器（绿色版）",
		homepage:    "https://github.com/junegunn/fzf",
		repo:        "junegunn/fzf",
		asset:       "fzf-*-windows_amd64.zip",
		unpack:      "zip",
		method:      "portable-inplace",
		installPath: "${ROOT}/fzf",
		entry:       []string{"fzf.exe"},
		processes:   []string{"fzf.exe"},
	},
	{
		id:          "ungoogled-chromium",
		name:        "Ungoogled Chromium",
		desc:        "去 Google 化的 Chromium（Windows 便携版）",
		homepage:    "https://github.com/ungoogled-software/ungoogled-chromium-windows",
		repo:        "ungoogled-software/ungoogled-chromium-windows",
		asset:       "*_windows_x64.zip",
		unpack:      "zip",
		method:      "portable-inplace",
		installPath: "${LOCALAPPDATA}/UngoogledChromium",
		entry:       []string{"chrome.exe"},
		processes:   []string{"chrome.exe"},
	},
	{
		id:          "7zip",
		name:        "7-Zip",
		desc:        "压缩工具（官方安装器，静默安装）",
		homepage:    "https://github.com/ip7z/7zip",
		repo:        "ip7z/7zip",
		asset:       "7z*-x64.exe",
		unpack:      "raw",
		method:      "exe-installer",
		installPath: "${ROOT}/7-Zip",
		entry:       []string{"7zFM.exe"},
	},
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// hubApp 把 appSpec 适配成 plugin.App（catalog 模式只需要 Versions）。
type hubApp struct {
	cfg  plugin.AppConfig
	spec appSpec
}

func (a *hubApp) Versions(ctx context.Context, req plugin.VersionsRequest) ([]plugin.Release, error) {
	return listReleases(ctx, a.cfg, a.spec, req.Limit)
}

// listReleases 查询某个仓库最近的发布，并挑出匹配当前平台的产物。
func listReleases(ctx context.Context, cfg plugin.AppConfig, spec appSpec, limit int) ([]plugin.Release, error) {
	if limit <= 0 || limit > 30 {
		limit = 10
	}
	api := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", spec.repo, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "upkit-hub")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 %s 的发布失败: %w", spec.repo, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: 仓库 %s 不存在", plugin.ErrNotFound, spec.repo)
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
		arts := make([]plugin.Artifact, 0, len(r.Assets))
		for _, a := range r.Assets {
			if !strings.HasSuffix(strings.ToLower(a.Name), ".zip") && !strings.HasSuffix(strings.ToLower(a.Name), ".exe") {
				continue
			}
			if !matchAsset(spec.asset, a.Name) {
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
		return nil, fmt.Errorf("%w: %s 最近的发布里没有匹配 %q 的产物（检查 asset 通配或平台）", plugin.ErrNotFound, spec.repo, spec.asset)
	}
	cfg.Log.Info("查询完成", "app", spec.id, "releases", len(out), "latest", out[0].Version)
	return out, nil
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

func main() {
	regs := make([]plugin.Registration, 0, len(catalog))
	for _, item := range catalog {
		spec := item
		regs = append(regs, plugin.Register(spec.id, func(cfg plugin.AppConfig) (plugin.App, error) {
			return &hubApp{cfg: cfg, spec: spec}, nil
		},
			plugin.WithName(spec.name),
			plugin.WithDescription(spec.desc),
			plugin.WithHomepage(spec.homepage),
			// 软身份：用上游仓库标识，便于跨来源去重。
			plugin.WithProvides(spec.repo),
			plugin.WithDefaults(plugin.Defaults{
				Method: spec.method,
				Unpack: spec.unpack,
				Install: plugin.InstallDefaults{
					Path:        spec.installPath,
					Entrypoints: spec.entry,
					Processes:   spec.processes,
				},
				SourceOptions: plugin.NewOptions("asset", spec.asset),
			}),
		))
	}

	plugin.Serve(plugin.Info{
		ID:          "upkit-hub",
		Name:        "upkit 示例插件集",
		Version:     version,
		Vendor:      "upkit",
		Homepage:    "https://github.com/dezhishen/upkit",
		Description: "演示一条订阅分发多个软件（catalog 模式）",
	}, regs...)
}
