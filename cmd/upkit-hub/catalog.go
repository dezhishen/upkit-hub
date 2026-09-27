package main

import (
	"context"
	"runtime"

	"github.com/dezhishen/upkit-hub/internal/ucbinaries"
	"github.com/dezhishen/upkit/pkg/plugin"
)

// source 是「上游 → 一组版本」的适配器：每个软件都要有一个。
//
// 把它独立出来是因为不同软件的上游形态差别很大：多数是 GitHub Releases（挑一个资产
// 名通配就够了），ungoogled-chromium 则要读一个索引站的两个页面。把差异收在这里，
// 注册与安装配置就只剩数据。
type source interface {
	// versions 返回该软件的可选版本（新 → 旧），最多 limit 个（<=0 表示由插件决定）。
	versions(ctx context.Context, cfg plugin.AppConfig, spec appSpec, limit int) ([]plugin.Release, error)
	// hosts 报告这个上源会用到哪些域名（下载 / 插件自有），供清单里的域名声明推导。
	//
	// 它必须写清**实际**会访问的域名：宿主会拿声明去强制校验下载地址，漏一个就会
	// 让用户的下载被拒。
	hosts() (download, pluginOwn []string)
}

// appSpec 描述订阅里的一个软件。
type appSpec struct {
	id       string
	name     string
	desc     string
	homepage string
	// provides 是软身份：上游项目标识，供宿主跨来源去重（同一个软件从两个来源出现时）。
	provides []string
	// src 说明版本与产物从哪个上游来。
	src source
	// arch 是 GOARCH → 上游命名片段；没有对应项时直接用 GOARCH。
	//
	// 上游对架构的叫法不统一：fzf 用 amd64/arm64，7-Zip 用 x64/arm64，
	// ungoogled-chromium 的索引站用 x64/arm64。挑错片段会让另一个架构的机器挑不到包，
	// 或者更糟 —— 挑到别的架构的包。
	arch map[string]string

	// 以下交给宿主的四条轴（本插件是 catalog 模式，不自己安装）。
	unpack      string
	method      string
	installPath string
	entry       []string
	processes   []string
}

// archToken 返回本机架构在上游命名里的片段。
func (s appSpec) archToken() string {
	if token, ok := s.arch[runtime.GOARCH]; ok {
		return token
	}
	return runtime.GOARCH
}

// catalog 是订阅里的全部软件。加一个软件 = 在这里加一条。
//
// 四条约定：
//
//   - id 一旦发布就不能改（宿主按它记版本、算安装状态）；用下载导航站做上游的软件，
//     id 与站点上的 softwareId 保持一致，这样 `-audit-software-hub` 能直接对照两份清单；
//   - 产物必须按本机架构挑 —— 宿主只用 Artifacts[0]，挑错就是装错包；
//   - 优先便携版（zip）而不是安装器：upkit 自己负责解包、备份与回滚，安装器会绕开
//     这套机制；
//   - 产物必须带摘要，而「摘要从哪来」要么由上游直接给出，要么在 src 里明确声明
//     （见 source_softwarehub.go 的 digestSource）。
var catalog = []appSpec{
	{
		id:          "fzf",
		name:        "fzf",
		desc:        "命令行模糊查找器（绿色版）",
		homepage:    "https://github.com/junegunn/fzf",
		provides:    []string{"junegunn/fzf"},
		src:         githubReleases{repo: "junegunn/fzf", asset: "fzf-*-windows_{arch}.zip"},
		unpack:      "zip",
		method:      "portable-inplace",
		installPath: "${ROOT}/fzf",
		entry:       []string{"fzf.exe"},
		processes:   []string{"fzf.exe"},
	},
	{
		id:       "ungoogled-chromium",
		name:     "Ungoogled Chromium",
		desc:     "去 Google 化的 Chromium（Windows 便携版，二进制由社区贡献者提交）",
		homepage: "https://ungoogled-software.github.io/ungoogled-chromium-binaries/",
		provides: []string{"ungoogled-software/ungoogled-chromium"},
		// 上游是一份索引站，而不是某一个仓库：贡献者各自在自己的仓库里发布，
		// 站点把它们汇总起来，并按平台分目录。
		src: ucBinaries{platformDirs: map[string]string{
			"amd64": ucbinaries.PlatformWindows64,
			"arm64": ucbinaries.PlatformWindowsARM64,
		}},
		unpack:      "zip",
		method:      "portable-inplace",
		installPath: "${LOCALAPPDATA}/UngoogledChromium",
		entry:       []string{"chrome.exe"},
		processes:   []string{"chrome.exe"},
	},
	{
		id:       "7zip",
		name:     "7-Zip",
		desc:     "压缩工具（官方安装器，静默安装）",
		homepage: "https://github.com/ip7z/7zip",
		provides: []string{"ip7z/7zip"},
		src:      githubReleases{repo: "ip7z/7zip", asset: "7z*-{arch}.exe"},
		// 7-Zip 把 amd64 叫 x64；arm64 与 Go 的叫法一致，用缺省即可。
		arch:        map[string]string{"amd64": "x64"},
		unpack:      "raw",
		method:      "exe-installer",
		installPath: "${ROOT}/7-Zip",
		entry:       []string{"7zFM.exe"},
	},
	{
		id:          "vscode",
		name:        "Visual Studio Code",
		desc:        "代码编辑器（官方免安装版，解压即用）",
		homepage:    "https://code.visualstudio.com/",
		provides:    []string{"microsoft/vscode"},
		src:         vscodeUpdate{products: vscodeArchiveProducts},
		unpack:      "zip",
		method:      "portable-inplace",
		installPath: "${ROOT}/VSCode",
		entry:       []string{"Code.exe"},
		processes:   []string{"Code.exe"},
	},
	{
		id:       "libreoffice",
		name:     "LibreOffice",
		desc:     "开源办公套件（官方 MSI 静默安装：系统级、需要管理员权限、不能自动回滚）",
		homepage: "https://www.libreoffice.org/",
		provides: []string{"documentfoundation/libreoffice"},
		// 版本来自下载导航站（它每天跟进），但产物地址按 TDF 官方的命名规则合成：
		// 站点给的地址指向腾讯云镜像，那个镜像的路径里多一层自己的目录、也不发布
		// 校验文件。合成出来的地址带版本号（不可变），旁边就有官方的 .sha256。
		src: softwareHub{
			siteID:      "libreoffice",
			origin:      "https://download.documentfoundation.org",
			urlTemplate: "/libreoffice/stable/{version}/win/{archDir}/LibreOffice_{version}_Win_{archFile}.msi",
			arch: map[string]archNames{
				"amd64": {dir: "x86_64", file: "x86-64"},
				"arm64": {dir: "aarch64", file: "aarch64"},
			},
			digest: sha256Sidecar{},
		},
		unpack: "raw",
		method: "msiexec",
		// MSI 装到哪由安装器决定（宿主不会改它），所以这里写官方固定布局而不是 ${ROOT}；
		// 入口探测与「装没装上」的复核都按它来判断。
		installPath: "${PROGRAMFILES}/LibreOffice",
		entry:       []string{"program/soffice.exe"},
		processes:   []string{"soffice.exe", "soffice.bin"},
	},
}

// vscodeArchiveProducts 是 VS Code 官方更新接口里按架构区分的产物名。
//
// 用 `-archive`（zip）而不是 `-user`（安装器）：宿主自己解包、备份、回滚，安装器会
// 绕开这套机制；arm64 上也有官方归档，不需要回退 x64。
var vscodeArchiveProducts = map[string]string{
	"amd64": "win32-x64-archive",
	"arm64": "win32-arm64-archive",
}
