package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/dezhishen/upkit-hub/internal/softwarehub"
)

// pinnedDigests 是「发布前实测出来的摘要表」：给那些自己不发校验文件的上游用。
//
// 表由 scripts/gen-digests.sh 生成（把产物下载一遍、算出 sha256、连同大小与版本号写进
// pinned_digests.json），随插件一起发出去。
//
// 为什么不在用户机器上现算：那等于装之前先整份下载一次、装的时候再下一次。为什么不让
// 它空着：没有摘要就没法判断下到的到底是不是那件东西 —— 本仓库的规矩是不发布没有摘要
// 的产物。
//
// 代价说清楚：表是按**地址**记的，所以只对「地址里带版本号」的软件有效。上游发新版 →
// 地址变了 → 表里没有 → 那个版本在旧插件里看不到，直到我们重新生成表、重新发版。
// 「永远指最新」的滚动地址绝不能进这张表：同一个地址明天就是另一个文件，表里的摘要在
// 用户那边会直接校验失败。
//
//go:embed pinned_digests.json
var pinnedDigestsJSON []byte

// pinnedEntry 是表里的一条。
type pinnedEntry struct {
	// SHA256 是发布前实测出来的摘要（小写十六进制）。
	SHA256 string `json:"sha256"`
	// Size 是字节数（下载时顺手记下，界面上就能显示「要下多大」）。
	Size int64 `json:"size"`
	// Version 是算这条摘要时上游的版本号，供人核对。
	Version string `json:"version"`
	// GeneratedAt 是算这条摘要的时间。
	GeneratedAt string `json:"generatedAt,omitempty"`
}

var (
	pinnedOnce sync.Once
	pinnedData map[string]pinnedEntry
	pinnedErr  error
)

// pinnedTable 读取并缓存摘要表。
func pinnedTable() (map[string]pinnedEntry, error) {
	pinnedOnce.Do(func() {
		pinnedData = map[string]pinnedEntry{}
		if len(strings.TrimSpace(string(pinnedDigestsJSON))) == 0 {
			pinnedErr = fmt.Errorf("构建期摘要表是空的（pinned_digests.json 没生成？）")
			return
		}
		if err := json.Unmarshal(pinnedDigestsJSON, &pinnedData); err != nil {
			pinnedErr = fmt.Errorf("解析构建期摘要表失败: %w", err)
		}
	})
	return pinnedData, pinnedErr
}

// pinnedDigest 从构建期摘要表里取摘要。
//
// 表里没有这个地址就是不提供这个版本：地址带版本号，地址变了说明上游发了新版，而新版
// 的摘要我们还没实测过。此时报错并说清原因，比给一个没有校验的下载要好。
type pinnedDigest struct{}

func (pinnedDigest) digest(_ context.Context, ref digestRef) (string, error) {
	table, err := pinnedTable()
	if err != nil {
		return "", err
	}
	entry, ok := table[ref.URL]
	if !ok {
		return "", fmt.Errorf(
			"构建期摘要表里没有这个地址（上游可能刚发新版）：需要重新生成摘要表并发布新版插件；%s", ref.URL)
	}
	if !isSHA256Hex(entry.SHA256) {
		return "", fmt.Errorf("构建期摘要表里 %s 的摘要不合法: %q", ref.URL, entry.SHA256)
	}
	return "sha256:" + strings.ToLower(entry.SHA256), nil
}

// size 实现 sized：表是下载产物时算出来的，所以大小是现成的。
func (pinnedDigest) size(rawURL string) (int64, bool) {
	table, err := pinnedTable()
	if err != nil {
		return 0, false
	}
	entry, ok := table[rawURL]
	if !ok || entry.Size <= 0 {
		return 0, false
	}
	return entry.Size, true
}

// sized 是摘要来源的可选能力：顺带知道产物大小（构建期摘要表就知道，因为它下载过）。
// 不知道的，调用方退回 HEAD 去问。
type sized interface {
	size(rawURL string) (int64, bool)
}

// listPinnedArtifacts 打印「需要在构建期算摘要的产物」，每行一条，供
// scripts/gen-digests.sh 直接消费：
//
//	<地址>\t<版本>\t<软件 id>
//
// 只列站点**当前**版本：摘要表就是为这一刻的产物准备的。
func listPinnedArtifacts(w io.Writer, baseURL string) error {
	ctx := context.Background()
	client := &softwarehub.Client{BaseURL: baseURL}
	planned := 0
	for _, spec := range catalog {
		src, ok := spec.src.(softwareHub)
		if !ok {
			continue
		}
		if _, ok := src.digest.(pinnedDigest); !ok {
			continue
		}
		payload, err := client.Versions(ctx, src.siteID)
		if err != nil {
			return fmt.Errorf("%s: %w", spec.id, err)
		}
		arches := make([]string, 0, len(src.arch))
		for arch := range src.arch {
			arches = append(arches, arch)
		}
		sort.Strings(arches)
		for _, arch := range arches {
			pick, err := payload.Pick(arch)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", spec.id, arch, err)
			}
			if softwarehub.PlaceholderVersion(pick.Version) {
				return fmt.Errorf("%s/%s: 站点没有可比较的版本号（%q）", spec.id, arch, pick.Version)
			}
			names, _, ok := src.pickArch(arch)
			if !ok {
				return fmt.Errorf("%s/%s: catalog 里没有这个架构的命名片段", spec.id, arch)
			}
			// artifactURL 会顺手把「站点地址落在声明的域名上」校验一遍，
			// 以及模板合成地址与站点文件名的交叉校验。
			rawURL, err := src.artifactURL(pick, names)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", spec.id, arch, err)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", rawURL, pick.Version, spec.id)
			planned++
		}
	}
	if planned == 0 {
		return fmt.Errorf("catalog 里没有软件需要构建期摘要（没人用 pinnedDigest？）")
	}
	return nil
}

// 编译期锚点：确保 pinnedDigest 满足 digestSource。
var _ digestSource = pinnedDigest{}
