package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/dezhishen/upkit/pkg/plugin"
)

// vscodeUpdate 用微软官方的更新接口做上游。
//
// 为什么不用下载导航站里那条 vscode：站点给的地址是
// `.../sha/download?build=stable&os=win32-x64-user`（永远指最新，内容随时会变），
// 而且它没有摘要。官方的更新接口一次就给出三样东西 —— 版本号、内容摘要、以及一个
// 由版本号决定的**不可变**地址，正好是 upkit 需要的全部。
//
// 地址用 `/{版本}/{产物}/stable`（会转发到 CDN），而不是接口返回的 CDN 直链：CDN 主机
// 是微软的实现细节，历史上换过，写进域名声明只会跟着过期；跳转后的字节由摘要兜底。
type vscodeUpdate struct {
	// products 是 GOARCH → 接口里的产物名。
	//
	// `-archive` 是免安装的 zip：upkit 自己解包、备份、回滚，而 `-user` 是安装器，
	// 会绕开这套机制（和 catalog 里「优先便携版」的约定一致）。
	products map[string]string
	// baseURL 与 httpClient 留空用默认值（测试时指向本地服务）。
	baseURL    string
	httpClient *http.Client
}

const vscodeDefaultBaseURL = "https://update.code.visualstudio.com"

func (s vscodeUpdate) versions(ctx context.Context, cfg plugin.AppConfig, spec appSpec, limit int) ([]plugin.Release, error) {
	product, ok := s.products[runtime.GOARCH]
	if !ok {
		return nil, fmt.Errorf("%w: 官方更新接口没有 Windows %s 的产物", plugin.ErrNotFound, runtime.GOARCH)
	}

	api := fmt.Sprintf("%s/api/update/%s/stable/latest", strings.TrimSuffix(s.base(), "/"), product)
	resp, err := getWithRetry(ctx, api)
	if err != nil {
		return nil, fmt.Errorf("查询 %s 的发布失败: %w", spec.id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("更新接口返回 HTTP %d（%s）", resp.StatusCode, api)
	}

	var raw struct {
		ProductVersion string `json:"productVersion"`
		Name           string `json:"name"`
		SHA256Hash     string `json:"sha256hash"`
		URL            string `json:"url"`
		Timestamp      int64  `json:"timestamp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析更新接口应答: %w", err)
	}
	version := strings.TrimSpace(raw.ProductVersion)
	if version == "" {
		return nil, fmt.Errorf("%w: 更新接口没有给出版本号（%s）", plugin.ErrNotFound, api)
	}
	if !isSHA256Hex(strings.TrimSpace(raw.SHA256Hash)) {
		return nil, fmt.Errorf("%w: 更新接口没有给出合法的 sha256（%s）", plugin.ErrNotFound, api)
	}

	// 落盘名沿用官方的 CDN 文件名（VSCode-win32-x64-1.139.1.zip）；接口没给时自己拼一个。
	name := fileNameOf(raw.URL)
	if name == "package" {
		name = fmt.Sprintf("VSCode-%s-%s.zip", product, version)
	}
	// 只有最新一版：接口的 /latest 只回答当前版本，历史版本要按提交号查，
	// 而提交号与版本号之间没有稳定的公开映射，所以这里不假装能列历史版本。
	published := time.Time{}
	if raw.Timestamp > 0 {
		published = time.UnixMilli(raw.Timestamp)
	}
	cfg.Log.Info("查询完成", "app", spec.id, "version", version, "product", product)
	artifactURL := fmt.Sprintf("%s/%s/%s/stable", strings.TrimSuffix(s.base(), "/"), version, product)

	return []plugin.Release{{
		Version:     version,
		Tag:         version,
		Channel:     "stable",
		PublishedAt: published,
		Notes:       "官方免安装 zip（解压即用）；只提供当前最新版，装不了旧版",
		Artifacts: []plugin.Artifact{{
			Name:   name,
			URL:    artifactURL,
			Size:   contentLength(ctx, artifactURL),
			Digest: "sha256:" + strings.ToLower(strings.TrimSpace(raw.SHA256Hash)),
		}},
	}}, nil
}

// hosts 报告这个上游用到的域名。
//
// 下载与版本查询都走同一个入口：产物地址是它上面那个带版本号的固定地址（转发到 CDN，
// 跳转目标不写进声明，由摘要兜底）。
func (s vscodeUpdate) hosts() (download, pluginOwn []string) {
	host := hostOf(s.base())
	return []string{host}, []string{host}
}

// base 返回实际使用的接口根地址。
func (s vscodeUpdate) base() string {
	if base := strings.TrimSpace(s.baseURL); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return vscodeDefaultBaseURL
}
