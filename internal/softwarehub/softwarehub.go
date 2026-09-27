// Package softwarehub 读取「常用软件下载导航」（dezhishen/original-software-hub）
// 发布的静态 JSON。
//
// 那个站点把各软件官网的版本与下载入口抓下来，发布成一棵静态 JSON：
//
//	index.json           入口（softwareList.path 指向软件清单）
//	software-list.json   软件清单（id / 名称 / 简介 / 官网 / 每个软件的数据路径）
//	versions/<id>.json   单个软件：平台 → 架构 → 链接
//
// 对 upkit 来说它**不是一份能直接安装的清单**，而是一份「有哪些软件、现在是什么版本、
// 官方入口在哪」的索引。差的三样东西正是本包在类型里点明的：
//
//   - 链接只是「哪里能下到」，没有摘要（Digest）—— 而 upkit 必须有摘要才肯发布产物；
//   - 很多链接是网页或商店入口（看 Link.Type），根本没有可下载的文件；
//   - 版本字段经常是字面量 "latest"（意思是「这个地址永远指最新」）而不是版本号，
//     拿它判断新旧只会永远相等。
//
// 所以调用方要自己补三件事：把地址换成**不可变**的来源、另找**可信摘要**、给出可自动化
// 的安装契约。本包只负责把站点上的数据如实解析出来，不做任何猜测 —— 猜出来的安装契约
// 会让用户在「点一下就好」之后得到一个装坏的系统。
package softwarehub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultBaseURL 是站点发布数据的根地址。
	DefaultBaseURL = "https://software-hub.sdniu.top/data/json"

	// maxAttempts 与 retryDelay 只作用于传输层错误。
	maxAttempts = 2
	retryDelay  = 300 * time.Millisecond

	// LinkDirect 表示「直链」：点开就是文件本身。
	//
	// 其余取值（webpage / store / …）都只是入口页，upkit 拿不到可安装的产物 ——
	// 这正是站点上近半数软件接不进来的原因。
	LinkDirect = "direct"

	// PlatformWindows 是 Windows 平台的名称前缀；PlatformStore 标记商店版
	// （那不是可自动安装的产物，例如「Windows (Store)」）。
	PlatformWindows = "Windows"
	PlatformStore   = "(Store)"

	// 架构标签归一化之后的取值。
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
	ArchX86   = "x86"

	maxPageBytes = 4 << 20
)

// Client 是站点数据的只读客户端。
type Client struct {
	// BaseURL 留空时用 DefaultBaseURL（测试时会指向本地服务）。
	BaseURL string
	// HTTP 留空时用默认客户端（30 秒超时）。
	HTTP *http.Client
}

// Source 是站点自己记录的数据路径（相对数据根目录）。
type Source struct {
	Mode string `json:"mode"`
	Path string `json:"path"`
}

// Index 是 index.json 的内容：入口只用来找软件清单。
type Index struct {
	Meta struct {
		Version     string `json:"version"`
		GeneratedAt string `json:"generatedAt"`
		Generator   string `json:"generator"`
	} `json:"meta"`
	SoftwareList Source `json:"softwareList"`
}

// Item 是软件清单里的一条。
type Item struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Organization    string   `json:"organization"`
	OfficialWebsite string   `json:"officialWebsite"`
	Categories      []string `json:"categories"`
	Tags            []string `json:"tags"`
	Source          Source   `json:"source"`
}

// List 是软件清单。
type List struct {
	UpdatedAt string `json:"updatedAt"`
	Items     []Item `json:"items"`
}

// Link 是一个下载入口。
type Link struct {
	Type  string `json:"type"` // direct / webpage / store …
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Package 是某个架构下的产物集合。
//
// Architecture 是站点上的自由文本（"x64"、"arm64"、"x64 (appimage)"、"x64/x86"…），
// 必须经 NormalizeArch 归一化之后再比较 —— 直接字符串比较一定会在某个软件上挑错包。
type Package struct {
	Architecture string `json:"architecture"`
	Links        []Link `json:"links"`
}

// Platform 是一个平台（Windows / macOS / Android …）的当前版本。
type Platform struct {
	Platform    string    `json:"platform"`
	Version     string    `json:"version"`
	ReleaseDate string    `json:"releaseDate"`
	OfficialURL string    `json:"officialUrl"`
	Packages    []Package `json:"packages"`
}

// Payload 是 versions/<id>.json 的内容。
//
// 注意它只有**当前**版本：站点不做历史归档，所以从这里接进来的软件只有一个可选版本，
// 装不了旧版。这是站点形态带来的硬限制，只能如实告诉用户。
type Payload struct {
	SoftwareID string     `json:"softwareId"`
	UpdatedAt  string     `json:"updatedAt"`
	Platforms  []Platform `json:"platforms"`
}

// Pick 是从站点数据里挑出的一份产物。
type Pick struct {
	// Version 是站点记的版本号，可能是占位值（见 PlaceholderVersion）。
	Version string
	// Date 是发布日期（站点没给时为零值）。
	Date time.Time
	// URL 是下载地址（站点给的原文，未经改写）。
	URL string
	// Label 是站点给这个链接的展示文本。
	Label string
	// Arch 是归一化后的架构（ArchAMD64 / ArchARM64 / ArchX86）。
	Arch string
	// Note 说明这次挑选的特殊之处：版本是占位值、或走了架构回退。
	Note string
}

// Base 返回实际使用的数据根地址。
func (c *Client) Base() string {
	if base := strings.TrimSpace(c.BaseURL); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return DefaultBaseURL
}

// URL 把站点上的相对路径拼成完整地址。
func (c *Client) URL(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return c.Base()
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	return c.Base() + "/" + strings.TrimPrefix(p, "./")
}

// Index 读取入口文件。
func (c *Client) Index(ctx context.Context) (Index, error) {
	var out Index
	err := c.decode(ctx, c.URL("index.json"), &out)
	return out, err
}

// List 读取软件清单（先读入口，再按它指的路径去取）。
func (c *Client) List(ctx context.Context) (List, error) {
	idx, err := c.Index(ctx)
	if err != nil {
		return List{}, err
	}
	if strings.TrimSpace(idx.SoftwareList.Path) == "" {
		return List{}, fmt.Errorf("站点的 index.json 没有给出 softwareList.path（站点结构可能变了）")
	}
	var out List
	if err := c.decode(ctx, c.URL(idx.SoftwareList.Path), &out); err != nil {
		return List{}, err
	}
	if len(out.Items) == 0 {
		return List{}, fmt.Errorf("站点的软件清单是空的（站点结构可能变了）")
	}
	return out, nil
}

// Versions 读取某个软件的平台数据。
func (c *Client) Versions(ctx context.Context, siteID string) (Payload, error) {
	return c.VersionsAt(ctx, "versions/"+strings.TrimSpace(siteID)+".json")
}

// VersionsAt 按站点上记录的路径读取平台数据（软件清单里的 source.path）。
func (c *Client) VersionsAt(ctx context.Context, path string) (Payload, error) {
	var out Payload
	if err := c.decode(ctx, c.URL(path), &out); err != nil {
		return Payload{}, err
	}
	if len(out.Platforms) == 0 {
		return Payload{}, fmt.Errorf("软件 %s 没有任何平台数据（站点结构可能变了）", out.SoftwareID)
	}
	return out, nil
}

// decode 取一个 JSON 文件并解析。
func (c *Client) decode(ctx context.Context, rawURL string, out any) error {
	body, err := c.fetch(ctx, rawURL)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", rawURL, err)
	}
	return nil
}

// fetch 取一个文件的内容，对**传输层**错误重试。
//
// 站点在海外，链路偶发 EOF 是常事（同一个地址连打三次也会有一次断）——不重试的话，
// 用户看到的是「查不到版本」，而实际上再试一次就好了。状态码错误不重试：404 就是没有，
// 重试只会拖慢报错。
func (c *Client) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("读取 %s 失败: %w", rawURL, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("读取 %s 失败: HTTP %d", rawURL, resp.StatusCode)
		}
		if readErr != nil {
			lastErr = fmt.Errorf("读取 %s 失败: %w", rawURL, readErr)
			continue
		}
		if len(body) > maxPageBytes {
			return nil, fmt.Errorf("读取 %s 失败: 内容超过 %d 字节上限", rawURL, maxPageBytes)
		}
		return body, nil
	}
	return nil, lastErr
}

// Windows 返回 Windows 桌面平台的条目（不含商店版）。
func (p Payload) Windows() []Platform {
	out := make([]Platform, 0, len(p.Platforms))
	for _, pl := range p.Platforms {
		name := strings.TrimSpace(pl.Platform)
		if !strings.HasPrefix(name, PlatformWindows) {
			continue
		}
		if strings.Contains(name, PlatformStore) {
			continue // 商店版没有可下载的产物
		}
		out = append(out, pl)
	}
	return out
}

// NormalizeArch 把站点的架构标签归一到 ArchAMD64 / ArchARM64 / ArchX86。
//
// 站点上的写法很杂（x64、x86_64、64bit、x64/x86、x64 (deb)、aarch64…），所以只取
// 第一个分隔符之前的那一段再匹配。认不出来时报 false —— 调用方应当**跳过**这个包，
// 而不是随便挑一个（挑错架构就是给用户装错包）。
func NormalizeArch(label string) (string, bool) {
	token := strings.TrimSpace(strings.ToLower(label))
	if token == "" {
		return "", false
	}
	// 取第一段：空格、括号、斜杠、逗号都会把「x64 (deb)」「x64/x86」这类写法切成主架构。
	token = strings.FieldsFunc(token, func(r rune) bool {
		return r == ' ' || r == '(' || r == '/' || r == ',' || r == '+'
	})[0]

	switch token {
	case "x64", "amd64", "x86_64", "x86-64", "64bit", "64-bit":
		return ArchAMD64, true
	case "arm64", "aarch64":
		return ArchARM64, true
	case "x86", "win32", "i386", "i686", "386", "32bit", "32-bit":
		return ArchX86, true
	}
	return "", false
}

// fallbackChain 返回某个架构可以接受的产物架构，按优先级排列。
//
// 允许回退的理由是 Windows 的兼容性：amd64 上跑得动 x86 的程序，arm64 上跑得动 x64
// （Windows 11 ARM 自带 x64 模拟）。回退**必须**在 Note 里说明，否则用户会以为自己拿到
// 的是原生版本。
func fallbackChain(goarch string) []string {
	switch goarch {
	case ArchAMD64:
		return []string{ArchAMD64, ArchX86}
	case ArchARM64:
		return []string{ArchARM64, ArchAMD64, ArchX86}
	default:
		return []string{goarch}
	}
}

// PlaceholderVersion 报告版本字段是否是占位值。
//
// 站点对「官方只提供一个永远指最新的地址」的软件会写 "latest" / "Latest" / 空 ——
// 那不是版本号：拿它当版本，升级判断会永远相等，装了旧版也升不上去。
func PlaceholderVersion(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "latest", "latest-ssl", "current", "stable", "最新", "最新版":
		return true
	}
	return false
}

// Pick 从站点数据里挑出本机架构能用的直链产物。
//
// 挑选顺序：先精确匹配本机架构，再按 fallbackChain 回退。任何一步都只认 LinkDirect ——
// 网页与商店入口不是产物。
func (p Payload) Pick(goarch string) (Pick, error) {
	windows := p.Windows()
	if len(windows) == 0 {
		return Pick{}, fmt.Errorf("软件 %s 在站点上没有 Windows 平台的条目", p.SoftwareID)
	}
	for _, want := range fallbackChain(goarch) {
		for _, pl := range windows {
			for _, pkg := range pl.Packages {
				arch, ok := NormalizeArch(pkg.Architecture)
				if !ok || arch != want {
					continue
				}
				for _, link := range pkg.Links {
					if link.Type != LinkDirect || strings.TrimSpace(link.URL) == "" {
						continue
					}
					return Pick{
						Version: pl.Version,
						Date:    parseDate(pl.ReleaseDate),
						URL:     link.URL,
						Label:   link.Label,
						Arch:    arch,
						Note:    pickNote(goarch, arch, pl.Version),
					}, nil
				}
			}
		}
	}
	return Pick{}, fmt.Errorf("软件 %s 在 Windows %s 上没有直链安装包（站点的入口多半只是网页或商店）",
		p.SoftwareID, goarch)
}

// pickNote 说明这次挑选需要提醒用户的地方。
func pickNote(goarch, arch, version string) string {
	notes := make([]string, 0, 2)
	if arch != goarch {
		notes = append(notes, fmt.Sprintf("上游没有 Windows %s 的原生包，这里回退到 %s 的安装包（由系统兼容层运行）", goarch, arch))
	}
	if PlaceholderVersion(version) {
		notes = append(notes, fmt.Sprintf("站点上的版本字段是 %q：它只表示地址永远指向最新，不能用来判断新旧", version))
	}
	return strings.Join(notes, "；")
}

// parseDate 解析站点的发布日期；解析不出来时返回零值。
func parseDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}

// Facts 是审计用的统计：一个软件在站点上「具备什么、缺什么」。
//
// 它只陈述数据里的事实，不下结论 —— 「能不能接」还取决于摘要与安装契约，那两样站点
// 里没有，必须由人补齐。
type Facts struct {
	// Platforms 是条目覆盖的平台数。
	Platforms int
	// Windows 是 Windows 桌面平台数（不含商店版）。
	Windows int
	// DirectLinks 是 Windows 上的直链数。
	DirectLinks int
	// PageOnlyLinks 是 Windows 上的网页/商店入口数。
	PageOnlyLinks int
	// Arches 是 Windows 上归一化后的架构（去重、排序）。
	Arches []string
	// UnmappedArches 是认不出来的架构写法（去重、排序）——它们会被挑选逻辑跳过。
	UnmappedArches []string
	// Versions 是 Windows 上出现过的版本字段（去重、排序）。
	Versions []string
}

// Facts 统计这个软件在站点上的形态。
func (p Payload) Facts() Facts {
	f := Facts{Platforms: len(p.Platforms)}
	archSet := map[string]bool{}
	unmapped := map[string]bool{}
	versionSet := map[string]bool{}
	for _, pl := range p.Windows() {
		f.Windows++
		versionSet[pl.Version] = true
		for _, pkg := range pl.Packages {
			if arch, ok := NormalizeArch(pkg.Architecture); ok {
				archSet[arch] = true
			} else {
				unmapped[strings.TrimSpace(pkg.Architecture)] = true
			}
			for _, link := range pkg.Links {
				if link.Type == LinkDirect && strings.TrimSpace(link.URL) != "" {
					f.DirectLinks++
				} else {
					f.PageOnlyLinks++
				}
			}
		}
	}
	f.Arches = sortedKeys(archSet)
	f.UnmappedArches = sortedKeys(unmapped)
	f.Versions = sortedKeys(versionSet)
	return f
}

// AllPlaceholderVersions 报告 Windows 平台的版本字段是否全是占位值。
func (f Facts) AllPlaceholderVersions() bool {
	if len(f.Versions) == 0 {
		return true
	}
	for _, v := range f.Versions {
		if !PlaceholderVersion(v) {
			return false
		}
	}
	return true
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		if strings.TrimSpace(k) == "" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HostOf 返回地址的主机名；解析不出来时返回空串。
func HostOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return u.Hostname()
}
