package main

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/dezhishen/upkit-hub/internal/ucbinaries"
	"github.com/dezhishen/upkit/pkg/plugin"
)

// ucBinaries 用 ungoogled-chromium 的官方二进制索引站作为上游。
//
// 那个站点（https://ungoogled-software.github.io/ungoogled-chromium-binaries/）是
// 社区贡献者提交二进制的汇聚点：同一个版本可能来自官方 Actions，也可能来自某位贡献者
// 自己的仓库。之所以以站点为准而不是钉死一个 GitHub 仓库：
//
//   - 索引把各贡献者的版本汇总在一起，写死仓库会漏掉别人提交的版本；
//   - 索引页给每个文件都列了 SHA256，而 GitHub API 的 digest 只有较新资产才有。
//
// 站点自己的首页写明这些二进制**不是官方构建**、也无法保证可复现，所以版本说明里
// 带上这句话与贡献者名字 —— 用户在按下安装之前应当知道。
type ucBinaries struct {
	// platformDirs 是 GOARCH → 站点上的平台目录名。
	platformDirs map[string]string
	// baseURL 与 httpClient 留空用默认值（测试时指向本地服务）。
	baseURL    string
	httpClient *http.Client
}

func (s ucBinaries) versions(ctx context.Context, cfg plugin.AppConfig, spec appSpec, limit int) ([]plugin.Release, error) {
	goarch := runtime.GOARCH
	dir, ok := s.platformDirs[goarch]
	if !ok {
		// 站点上没有这个架构的目录：这台机器就是装不了，明确说清楚。
		return nil, fmt.Errorf("%w: 索引站没有 Windows %s 的目录", plugin.ErrNotFound, goarch)
	}

	client := &ucbinaries.Client{BaseURL: s.baseURL, HTTP: s.httpClient}
	picked, err := client.Releases(ctx, dir, limit, func(rel ucbinaries.Release) (ucbinaries.File, bool) {
		return rel.PickPortable(goarch)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %s", plugin.ErrNotFound, err)
	}

	out := make([]plugin.Release, 0, len(picked))
	for _, p := range picked {
		out = append(out, plugin.Release{
			Version: p.Release.Version,
			Tag:     p.Release.Version,
			Channel: "stable",
			// 站点没给文件大小，留 0（宿主会显示为未知）。
			PublishedAt: p.Release.PublishedAt,
			Notes:       ucNotes(p.Release),
			Artifacts: []plugin.Artifact{{
				Name:   p.File.Name,
				URL:    p.File.URL,
				Digest: "sha256:" + strings.ToLower(p.File.SHA256),
			}},
		})
	}
	cfg.Log.Info("查询完成", "app", spec.id, "releases", len(out), "latest", out[0].Version)
	return out, nil
}

// ucNotes 说明这批二进制的信任边界：谁构建的、是不是官方构建。
func ucNotes(rel ucbinaries.Release) string {
	author := strings.TrimSpace(rel.Author)
	if author == "" {
		author = "未署名"
	}
	return fmt.Sprintf("二进制由社区贡献者提交（%s），非官方构建、无法保证可复现", author)
}
