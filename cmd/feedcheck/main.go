package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/dezhishen/upkit-hub/internal/feedcheck"
)

// Command feedcheck 是 upkit-hub 的发布前门禁：把宿主会拒绝的清单在发布之前拒掉。
//
// 它有三个层次，按需组合：
//   - 解析 + 校验：严格模式解析、schema、平台覆盖、id 与摘要格式（宿主同一套规则）；
//   - -artifacts：清单里的文件名 / sha256 / size 必须与磁盘上的产物一致；
//   - -check-urls：逐个下载清单里的包，核对摘要（发布后或本地起服务后验证用）。
//
// 之所以不只用 `go test`：CI 里生成完 feed.yaml 之后需要对着**真实的**产物与地址
// 再验一次 —— 那是测试夹具覆盖不到的一段。

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	feedPath    string
	artifacts   string
	feedURL     string
	hostVersion string
	checkURLs   bool
	insecure    bool
	timeout     time.Duration
}

func run() error {
	var opt options
	flag.StringVar(&opt.feedPath, "feed", "dist/release/feed.yaml", "订阅清单路径")
	flag.StringVar(&opt.artifacts, "artifacts", "", "产物目录；给了就核对清单里的文件名与摘要")
	flag.StringVar(&opt.feedURL, "feed-url", "", "清单自己的地址；清单里写了相对产物地址（./<文件>）时用它解析")
	flag.StringVar(&opt.hostVersion, "host-version", "", "模拟的宿主版本，用于校验 min_host_version（留空表示不校验）")
	flag.BoolVar(&opt.checkURLs, "check-urls", false, "逐个下载清单里的包并核对摘要（需要网络）")
	flag.BoolVar(&opt.insecure, "insecure", false, "下载时不校验证书（仅用于本地自签 https 调试）")
	flag.DurationVar(&opt.timeout, "timeout", 60*time.Second, "整体超时")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: go run ./cmd/feedcheck -feed <feed.yaml> [选项]\n\n选项:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), opt.timeout)
	defer cancel()

	feed, err := feedcheck.Load(opt.feedPath)
	if err != nil {
		return err
	}
	fmt.Printf("==> 清单 %s\n", opt.feedPath)
	fmt.Printf("    schema=%d  name=%s  updated_at=%s\n",
		feed.Schema, feed.Name, feed.UpdatedAt.UTC().Format(time.RFC3339))
	if err := feed.ValidateAllPlatforms(opt.hostVersion); err != nil {
		return fmt.Errorf("平台覆盖或 schema 校验失败: %w", err)
	}
	for _, e := range feed.Entries() {
		fmt.Printf("    %-24s %-14s v%s  %s\n", e.Plugin.ID, e.Platform, e.Plugin.Version,
			summarize(e.Package.SHA256))
	}
	fmt.Printf("==> 校验通过：%d 个插件，%d 条平台条目，全部覆盖 %v\n",
		len(feed.Plugins), len(feed.Entries()), feedcheck.SupportedPlatforms())

	if opt.artifacts != "" {
		if err := feed.CheckArtifacts(opt.artifacts); err != nil {
			return fmt.Errorf("产物核对失败: %w", err)
		}
		fmt.Printf("==> 产物核对通过：清单里的文件名、sha256、size 与 %s 下的文件一致\n", opt.artifacts)
	}

	if opt.checkURLs {
		client := &http.Client{Timeout: opt.timeout}
		if opt.insecure {
			// 仅用于本地自签证书：默认的校验链在开发机上没有可信根。
			client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
			fmt.Println("    （已关闭证书校验，仅用于本地自签 https）")
		}
		if err := feed.VerifyURLs(ctx, client, opt.feedURL); err != nil {
			return fmt.Errorf("地址核对失败: %w", err)
		}
		fmt.Println("==> 地址核对通过：清单里的每个包都能下到，且内容与摘要一致")
	}

	return nil
}

func summarize(digest string) string {
	d := feedcheck.NormalizeSHA256(digest)
	if len(d) > 16 {
		return d[:16] + "…"
	}
	return d
}
