package main

import (
	"fmt"
	"sort"
	"strings"
)

// DeclaredHosts 是清单里插件条目应当声明的域名。
//
// 两类要分开写，因为宿主对它们的能力完全不同：
//
//   - Download：宿主下载产物会去的域名。宿主**强制校验** —— 插件给出的地址不在
//     这个集合里就拒绝下载，让用户重新确认订阅；
//   - Plugin：插件进程自己会访问的域名（查版本之类）。宿主拦不住，也无法授权，
//     写出来只是让用户在确认时知道会用哪些服务。
type DeclaredHosts struct {
	Download []string
	Plugin   []string
}

// Declarations 汇总 catalog 里全部软件的声明。
//
// 它由各上游的 hosts() **推导**，而不是手写：清单里的声明是要拿去强制校验下载地址
// 的，手写必然和代码漂移 —— 漏一个域名，用户那边的下载就会被拒；多写一个，就等于
// 悄悄放宽了限制。推导则从根上没有这个余地。
func Declarations() DeclaredHosts {
	download := map[string]bool{}
	plugin := map[string]bool{}
	for _, spec := range catalog {
		d, p := spec.src.hosts()
		for _, h := range d {
			download[h] = true
		}
		for _, h := range p {
			plugin[h] = true
		}
	}
	return DeclaredHosts{Download: sortedKeys(download), Plugin: sortedKeys(plugin)}
}

// Fragment 把声明渲染成两个命令行片段，供清单生成器直接接在参数后面：
//
//	--download-hosts=github.com
//	--plugin-hosts=api.github.com,ungoogled-software.github.io
//
// 用这种形态而不是 YAML，是为了让 gen-feed.sh 不必在 bash 里解析 YAML；值里也不会
// 出现空格，因此调用方直接 `... $(go run ./cmd/upkit-hub -print-declarations)` 即可。
func (d DeclaredHosts) Fragment() string {
	var b strings.Builder
	if len(d.Download) > 0 {
		fmt.Fprintf(&b, "--download-hosts=%s\n", strings.Join(d.Download, ","))
	}
	if len(d.Plugin) > 0 {
		fmt.Fprintf(&b, "--plugin-hosts=%s\n", strings.Join(d.Plugin, ","))
	}
	return b.String()
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
