package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/dezhishen/upkit/pkg/plugin"
)

// Command upkit-hub 是官方订阅里的那个插件：把一批常用软件放进一条订阅分发。
//
// 它是 catalog 模式 —— 只回答「有哪些软件、有哪些版本、下载什么」，下载 / 解包 /
// 落地 / 探测全部复用宿主内置的四条轴，所以这里没有任何安装逻辑。
//
// 软件的清单在 catalog.go：一条 appSpec = 一个软件，src 说明版本与产物从哪个上游来。
// 插件本身按架构分别构建（订阅里同时有 windows/amd64 与 windows/arm64 两份），
// 所以 runtime.GOARCH 就是宿主的架构 —— 挑产物时必须用它。

// version 是插件自身的版本号，由构建脚本用 -ldflags -X main.version 注入；
// 直接 go run / go build 时为 dev。发布流水线会把它设成与订阅 tag 同一个版本。
var version = "dev"

// hubApp 把 appSpec 适配成 plugin.App（catalog 模式只需要 Versions）。
type hubApp struct {
	cfg  plugin.AppConfig
	spec appSpec
}

func (a *hubApp) Versions(ctx context.Context, req plugin.VersionsRequest) ([]plugin.Release, error) {
	return a.spec.src.versions(ctx, a.cfg, a.spec, req.Limit)
}

// optionsOf 把目录里的选项表转成宿主认的 Options。
//
// 键先排序：同一份目录每次注册出来的选项顺序要一致，否则宿主侧的配置比对会看到无谓的
// 顺序差异。
func optionsOf(m map[string]string) plugin.Options {
	if len(m) == 0 {
		return plugin.Options{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kv := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		kv = append(kv, k, m[k])
	}
	return plugin.NewOptions(kv...)
}

func main() {
	// 清单生成需要知道「这个插件声明哪些域名」，而声明的唯一事实来源是 catalog。
	// 所以让插件自己把它打印出来，供 scripts/gen-feed.sh 直接接在参数后面（见
	// declarations.go 的 Fragment）。带这个参数时不进 Serve，也不会与宿主握手。
	if len(os.Args) > 1 && (os.Args[1] == "-print-declarations" || os.Args[1] == "--print-declarations") {
		fmt.Print(Declarations().Fragment())
		return
	}

	// 盘点「下载导航站上哪些软件够得着接入条件」：这一步只读站点数据，不进 Serve，
	// 也不会与宿主握手。加软件之前先跑它，能省掉一轮一轮重新抓数据的功夫。
	if len(os.Args) > 1 && (os.Args[1] == "-audit-software-hub" || os.Args[1] == "--audit-software-hub") {
		if err := auditSoftwareHub(os.Stdout, ""); err != nil {
			fmt.Fprintln(os.Stderr, "盘点失败:", err)
			os.Exit(1)
		}
		return
	}

	// 导出「需要在构建期算摘要的产物」：scripts/gen-digests.sh 拿它去下载并实测 sha256。
	// 自己不发校验文件的上游（微信、QQ 这类）全靠这张表。
	if len(os.Args) > 1 && (os.Args[1] == "-list-pinned-artifacts" || os.Args[1] == "--list-pinned-artifacts") {
		if err := listPinnedArtifacts(os.Stdout, ""); err != nil {
			fmt.Fprintln(os.Stderr, "导出失败:", err)
			os.Exit(1)
		}
		return
	}

	regs := make([]plugin.Registration, 0, len(catalog))
	for _, item := range catalog {
		spec := item
		regs = append(regs, plugin.Register(spec.id, func(cfg plugin.AppConfig) (plugin.App, error) {
			return &hubApp{cfg: cfg, spec: spec}, nil
		},
			plugin.WithName(spec.name),
			plugin.WithDescription(spec.desc),
			plugin.WithHomepage(spec.homepage),
			// 软身份：用上游项目标识，便于宿主跨来源去重。
			plugin.WithProvides(spec.provides...),
			plugin.WithDefaults(plugin.Defaults{
				Method:        spec.method,
				Unpack:        spec.unpack,
				MethodOptions: optionsOf(spec.methodOpts),
				UnpackOptions: optionsOf(spec.unpackOpts),
				SourceOptions: optionsOf(spec.sourceOpts),
				Install: plugin.InstallDefaults{
					Path:        spec.installPath,
					Entrypoints: spec.entry,
					Processes:   spec.processes,
				},
			}),
		))
	}

	plugin.Serve(plugin.Info{
		ID:          "upkit-hub",
		Name:        "upkit 官方插件集",
		Version:     version,
		Vendor:      "upkit",
		Homepage:    "https://github.com/dezhishen/upkit-hub",
		Description: "upkit 官方源里的软件（catalog 模式，版本从各上游动态查询）",
	}, regs...)
}
