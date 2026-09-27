package feedcheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	upkitplugin "github.com/dezhishen/upkit/pkg/plugin"
	"gopkg.in/yaml.v3"
)

// Package feedcheck 是本仓库的「发布前门禁」：解析并校验 upkit 订阅清单。
//
// 权威实现在 upkit 主仓库的 internal/pluginfeed（schema.go / validate.go / feed.go）。
// 那是**另一个 module 的 internal 包，Go 不允许引入**，所以这里有一份最小复刻。
// 它只解决一件事：把宿主一定会拒绝的清单在发布之前拒掉 —— 而不是自己当第二份权威。
//
// 由此推出两条自缚的规则：
//
//  1. 这里的检查只允许「与宿主一致」或「比宿主更严」，不允许比宿主宽松；
//  2. 不发明宿主不认识的字段和取值。
//
// 上游改动时下面这些对应关系必须同步（文件名按 upkit 仓库内路径给出）：
//
//	Parse               ← internal/pluginfeed/feed.go     Parse（严格模式 KnownFields）
//	Feed.Validate       ← internal/pluginfeed/schema.go   Feed.Validate / Plugin.validate / Package.validate
//	SupportedPlatforms  ← internal/pluginfeed/schema.go   SupportedPlatforms
//	TargetOS 等常数     ← internal/pluginfeed/schema.go   TargetOS / ArchAMD64 / ArchARM64
//	NormalizeSHA256     ← internal/pluginfeed/validate.go NormalizeSHA256
//	pluginVersionRe     ← internal/pluginfeed/schema.go   pluginVersionRe
//	ValidID             ← pkg/plugin/register.go          ValidID（公开包，直接转调而非复制）
//	ModeCatalog         ← pkg/plugin/constants.go         ModeCatalog
//
// 另外两个函数是宿主没有的，纯粹服务于发布流程：
// CheckArtifacts（清单与磁盘产物是否严格对应）与 VerifyURLs（清单里的地址是否真的
// 能下到那件东西）—— 它们只做比对，不改变语义。

// SchemaVersion 是本仓库能识别的最新订阅 schema（与宿主当前值一致）。
const SchemaVersion = 1

// upkit 只发行 Windows 版本，所以「宿主平台」里的系统部分是常量（与宿主同一口径）。
const (
	TargetOS  = "windows"
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// MaxFeedBytes 是订阅文件大小上限，与宿主一致（防止误把别的东西当成清单）。
const MaxFeedBytes = 4 << 20

// PlatformOf 返回指定 GOOS/GOARCH 的平台标识。
func PlatformOf(goos, goarch string) string {
	return strings.TrimSpace(goos) + "/" + strings.TrimSpace(goarch)
}

// SupportedPlatforms 返回全部受支持的平台键（amd64 在前）。
func SupportedPlatforms() []string {
	return []string{PlatformOf(TargetOS, ArchAMD64), PlatformOf(TargetOS, ArchARM64)}
}

// SupportedPlatform 报告平台键是否受支持（只认 Windows 的两个架构）。
func SupportedPlatform(platform string) bool {
	osName, arch, ok := strings.Cut(strings.TrimSpace(platform), "/")
	if !ok || osName != TargetOS {
		return false
	}
	return arch == ArchAMD64 || arch == ArchARM64
}

// Feed 是订阅文件的顶层结构。
type Feed struct {
	Schema    int       `yaml:"schema"`
	Name      string    `yaml:"name"`
	UpdatedAt time.Time `yaml:"updated_at"`
	Plugins   []Plugin  `yaml:"plugins"`
}

// Plugin 是订阅里的一个插件条目。
type Plugin struct {
	ID             string             `yaml:"id"`
	Name           string             `yaml:"name"`
	Description    string             `yaml:"description"`
	Homepage       string             `yaml:"homepage"`
	Version        string             `yaml:"version"`
	Mode           string             `yaml:"mode"`
	MinHostVersion string             `yaml:"min_host_version"`
	Packages       map[string]Package `yaml:"packages"`
}

// Package 是某个平台上的插件产物。
type Package struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	Size   int64  `yaml:"size"`
}

// Platforms 返回该插件声明的平台键（字典序，便于输出稳定）。
func (p Plugin) Platforms() []string {
	out := make([]string, 0, len(p.Packages))
	for k := range p.Packages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Parse 解析订阅内容（严格模式：字段名写错会直接报错）。
//
// 宿主用 KnownFields(true) 解析，理由同样是「订阅等于远程代码执行授权」，静默忽略
// 拼写错误会让人误以为某个限制生效了。这里必须保持一致。
func Parse(data []byte) (*Feed, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("订阅内容为空")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f Feed
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("解析订阅失败（严格模式：字段名必须与 schema 完全一致）: %w", err)
	}
	return &f, nil
}

// Load 读取并解析清单文件（限制大小，避免误传大文件）。
func Load(path string) (*Feed, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("读取订阅 %s: %w", path, err)
	}
	if info.Size() > MaxFeedBytes {
		return nil, fmt.Errorf("订阅 %s 超过 %d 字节上限", path, MaxFeedBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取订阅 %s: %w", path, err)
	}
	return Parse(raw)
}

// Validate 检查订阅的完整性与自洽性（hostPlatform 为空时按当前架构）。
func (f *Feed) Validate(hostVersion, hostPlatform string) error {
	if f == nil {
		return fmt.Errorf("订阅为空")
	}
	if f.Schema <= 0 {
		return fmt.Errorf("订阅缺少 schema 版本")
	}
	if f.Schema > SchemaVersion {
		return fmt.Errorf("订阅 schema 版本为 %d，当前宿主只支持 %d（请升级 upkit）", f.Schema, SchemaVersion)
	}
	if len(f.Plugins) == 0 {
		return fmt.Errorf("订阅里没有任何插件")
	}
	if hostPlatform == "" {
		hostPlatform = PlatformOf(TargetOS, runtime.GOARCH)
	}
	if !SupportedPlatform(hostPlatform) {
		return fmt.Errorf("不支持的平台 %q：upkit 只支持 %s",
			hostPlatform, strings.Join(SupportedPlatforms(), "、"))
	}

	seen := map[string]int{}
	for i, p := range f.Plugins {
		if err := p.validate(hostVersion, hostPlatform); err != nil {
			return fmt.Errorf("plugins[%d]: %w", i, err)
		}
		if prev, dup := seen[p.ID]; dup {
			return fmt.Errorf("plugins[%d]: 插件 id %q 重复（与 plugins[%d] 冲突）", i, p.ID, prev)
		}
		seen[p.ID] = i
	}
	return nil
}

// ValidateAllPlatforms 在全部受支持平台上校验。
//
// 发布门禁必须走这条：每个插件都要覆盖全部架构，漏一个架构，那个架构的用户会在
// 「校验订阅」这一步失败 —— 而这本可以在发布前发现。
func (f *Feed) ValidateAllPlatforms(hostVersion string) error {
	for _, platform := range SupportedPlatforms() {
		if err := f.Validate(hostVersion, platform); err != nil {
			return fmt.Errorf("%s: %w", platform, err)
		}
	}
	return nil
}

// pluginVersionRe 限制版本号的字符集（与宿主一致）。
//
// 版本号会被拼进缓存文件名，放任任意字符等于把 filepath.Join 的越界能力交给订阅方。
var pluginVersionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

func (p Plugin) validate(hostVersion, hostPlatform string) error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("缺少 id")
	}
	if !ValidID(p.ID) {
		return fmt.Errorf("id %q 不合法（要求 ^[a-z0-9][a-z0-9._-]{0,63}$，且不得为 Windows 保留名）", p.ID)
	}
	if v := strings.TrimSpace(p.Version); v != "" && !pluginVersionRe.MatchString(v) {
		return fmt.Errorf("version %q 不合法（只允许字母、数字与 . _ + -，且不以符号开头）", p.Version)
	}
	if len(p.Packages) == 0 {
		return fmt.Errorf("插件 %s 没有任何平台的包", p.ID)
	}
	if _, ok := p.Packages[hostPlatform]; !ok {
		return fmt.Errorf("插件 %s 没有 %s 平台的包（可选：%v）", p.ID, hostPlatform, p.Platforms())
	}
	// 其它平台的包不报错也不采纳：它可能同时服务别的工具，upkit 只挑 Windows 那份。
	for _, platform := range p.Platforms() {
		if err := p.Packages[platform].validate(); err != nil {
			return fmt.Errorf("插件 %s 的 %s 包: %w", p.ID, platform, err)
		}
	}
	if p.MinHostVersion != "" && hostVersion != "" && CompareVersions(hostVersion, p.MinHostVersion) < 0 {
		return fmt.Errorf("插件 %s 要求宿主版本 >= %s，当前为 %s", p.ID, p.MinHostVersion, hostVersion)
	}
	return nil
}

func (pkg Package) validate() error {
	if strings.TrimSpace(pkg.URL) == "" {
		return fmt.Errorf("缺少 url")
	}
	// 协议相对地址（//host/x）长得像相对路径，解析后却会换域 —— 等于把下载指向别处。
	// 宿主明确拒绝这种写法，这里也把它拦在发布之前。
	if strings.HasPrefix(strings.TrimSpace(pkg.URL), "//") {
		return fmt.Errorf("url 不能写成协议相对地址（%s）：解析后会换域，应写成绝对地址或 ./<文件>", pkg.URL)
	}
	// sha256 是强制的：订阅等于远程代码执行授权，没有哈希就无法判断下载到的东西。
	if !ValidSHA256(pkg.SHA256) {
		return fmt.Errorf("缺少合法的 sha256（插件包必须提供 64 位十六进制摘要）")
	}
	if pkg.Size < 0 {
		return fmt.Errorf("size 不能为负")
	}
	return nil
}

// ValidID 报告插件 id 是否合法。
//
// 直接转调 SDK 的公开实现，而不是复制一份正则：这条规则跨进程存在，必须只有一个来源。
func ValidID(id string) bool { return upkitplugin.ValidID(id) }

// ValidSHA256 报告摘要是否是合法的 sha256（允许 "sha256:" 前缀）。
func ValidSHA256(digest string) bool {
	d := NormalizeSHA256(digest)
	if len(d) != 64 {
		return false
	}
	_, err := hex.DecodeString(d)
	return err == nil
}

// NormalizeSHA256 去掉可选的 "sha256:" 前缀并统一为小写。
func NormalizeSHA256(digest string) string {
	d := strings.ToLower(strings.TrimSpace(digest))
	return strings.TrimPrefix(d, "sha256:")
}

// CompareVersions 比较两个版本号，返回 -1 / 0 / 1（与宿主同一套规则）。
//
// 只用于判断 min_host_version 是否满足，因此不需要完整的 semver：按 . - + _ 切段，
// 数字段按数值比较，数字段大于非数字段（1.0 > 1.0-rc1），其余按字典序。
func CompareVersions(a, b string) int {
	as, bs := splitVersion(a), splitVersion(b)
	n := max(len(as), len(bs))
	for i := range n {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if c := compareSegment(x, y); c != 0 {
			return c
		}
	}
	return 0
}

func splitVersion(v string) []string {
	s := strings.TrimSpace(v)
	// 统一剥掉 v 前缀：v1.2.0 与 1.2.0 是同一个版本。
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '.' || r == '-' || r == '+' || r == '_'
	})
}

func compareSegment(x, y string) int {
	xn, xerr := parseUint(x)
	yn, yerr := parseUint(y)
	switch {
	case x == "" && y == "":
		return 0
	case x == "":
		// 段用尽：对方是预发布标识时我们用尽的一方更大（1.0.0 > 1.0.0-rc1），
		// 对方是数字段时更小（1.0 < 1.0.0）。
		if yerr != nil {
			return 1
		}
		return -1
	case y == "":
		if xerr != nil {
			return -1
		}
		return 1
	case xerr == nil && yerr == nil:
		switch {
		case xn < yn:
			return -1
		case xn > yn:
			return 1
		default:
			return 0
		}
	case xerr == nil:
		return 1 // 数字段 > 非数字段
	case yerr == nil:
		return -1
	default:
		return comparePrerelease(x, y)
	}
}

// comparePrerelease 按「公共前缀 + 末尾数字的数值」比较，因此 rc1 < rc2 < rc10。
func comparePrerelease(x, y string) int {
	xp, xn, xok := splitTrailingNumber(x)
	yp, yn, yok := splitTrailingNumber(y)
	if c := strings.Compare(xp, yp); c != 0 {
		return c
	}
	switch {
	case !xok && !yok:
		return 0
	case !xok:
		return -1
	case !yok:
		return 1
	case xn < yn:
		return -1
	case xn > yn:
		return 1
	default:
		return 0
	}
}

func splitTrailingNumber(s string) (string, int, bool) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, 0, false
	}
	n, err := parseUint(s[i:])
	if err != nil {
		return s, 0, false
	}
	return s[:i], n, true
}

func parseUint(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("空")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("非数字")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// CheckArtifacts 校验清单与磁盘上的产物严格对应。
//
// 它补的是脚本管不到的那一半：gen-feed.sh 现算摘要，只能保证「清单与它当时看到的
// 文件一致」，而 artifact 上传、下载、复制这些环节出错是看不到的。发布前拿真实文件
// 再比一次，才能保证用户拿到的摘要与产物匹配。
func (f *Feed) CheckArtifacts(dir string) error {
	if f == nil {
		return fmt.Errorf("订阅为空")
	}
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("产物目录为空")
	}
	for _, p := range sortedPlugins(f.Plugins) {
		for _, platform := range p.Platforms() {
			pkg := p.Packages[platform]
			if !SupportedPlatform(platform) {
				// 不支持的平台（例如同时服务别的工具）不做本地比对，也不报错。
				continue
			}
			if err := checkPackageArtifact(dir, p.ID, platform, pkg); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkPackageArtifact(dir, id, platform string, pkg Package) error {
	want := ArtifactName(id, platform)
	got := path.Base(strings.TrimSpace(pkg.URL))
	if got != want {
		return fmt.Errorf("插件 %s 的 %s 包地址指向 %s，按约定应为 %s（清单可能被手改过）",
			id, platform, got, want)
	}
	file := filepath.Join(dir, want)
	sum, size, err := FileSHA256(file)
	if err != nil {
		return fmt.Errorf("插件 %s 的 %s 包: %w", id, platform, err)
	}
	if want := NormalizeSHA256(pkg.SHA256); sum != want {
		return fmt.Errorf("插件 %s 的 %s 包摘要与产物不符: 清单 %s，实际 %s", id, platform, want, sum)
	}
	if pkg.Size != 0 && pkg.Size != size {
		return fmt.Errorf("插件 %s 的 %s 包大小与产物不符: 清单 %d，实际 %d", id, platform, pkg.Size, size)
	}
	return nil
}

// ArtifactName 返回某插件在某平台上的产物文件名约定：<id>-<os>-<arch>[.exe]。
//
// 这是 gen-feed.sh 识别 id 与平台的依据，也是 release 附件的唯一合法命名。
func ArtifactName(id, platform string) string {
	osName, arch, _ := strings.Cut(platform, "/")
	name := fmt.Sprintf("%s-%s-%s", id, osName, arch)
	if osName == TargetOS {
		name += ".exe"
	}
	return name
}

// FileSHA256 返回文件的 sha256（小写十六进制）与字节数。
func FileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("打开产物失败: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("读取产物失败: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Entries 返回清单里的全部「插件 × 平台」条目（按 id 与平台排序，输出稳定）。
func (f *Feed) Entries() []Entry {
	if f == nil {
		return nil
	}
	out := make([]Entry, 0, len(f.Plugins))
	for _, p := range sortedPlugins(f.Plugins) {
		for _, platform := range p.Platforms() {
			out = append(out, Entry{Plugin: p, Platform: platform, Package: p.Packages[platform]})
		}
	}
	return out
}

// Entry 是清单里的一条「插件 × 平台」。
type Entry struct {
	Plugin   Plugin
	Platform string
	Package  Package
}

func sortedPlugins(plugins []Plugin) []Plugin {
	out := make([]Plugin, len(plugins))
	copy(out, plugins)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
