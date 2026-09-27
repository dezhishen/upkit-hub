package main

import (
	"context"

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
			// 软身份：用上游项目标识，便于宿主跨来源去重。
			plugin.WithProvides(spec.provides...),
			plugin.WithDefaults(plugin.Defaults{
				Method: spec.method,
				Unpack: spec.unpack,
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
