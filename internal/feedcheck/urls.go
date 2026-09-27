package feedcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// MaxPackageBytes 是单个插件包的大小上限，与宿主一致。
const MaxPackageBytes = 256 << 20

// VerifyURLs 逐个下载清单里的包并核对摘要与大小。
//
// 它把「清单声明的东西真的存在、且内容与摘要一致」也验掉：release 附件名写错、
// 漏传文件、地址拼错、清单与产物来自两次发布 —— 这些都会在这里现形，而不是等用户
// 点「安装」时才失败。
//
// feedURL 是清单自己的地址，用来解析相对地址（./<文件>）：相对地址一律相对清单位置
// 解析，这与宿主同一套规则。整份清单都是相对地址时不给 feedURL 就无法核对，此时
// 会明确报错而不是默默跳过。
//
// 只校验受支持平台（Windows 的那两个）；其它平台的包不下载也不报错。
func (f *Feed) VerifyURLs(ctx context.Context, client *http.Client, feedURL string) error {
	if f == nil {
		return fmt.Errorf("订阅为空")
	}
	if client == nil {
		client = http.DefaultClient
	}
	for _, e := range f.Entries() {
		if !SupportedPlatform(e.Platform) {
			continue
		}
		abs, err := ResolveURL(feedURL, e.Package.URL)
		if err != nil {
			return fmt.Errorf("插件 %s 的 %s 包: %w", e.Plugin.ID, e.Platform, err)
		}
		if err := verifyOneURL(ctx, client, e, abs); err != nil {
			return err
		}
	}
	return nil
}

// ResolveURL 把清单里的包地址解析成绝对地址。
//
// 绝对地址原样返回（宿主会按跨域规则单独向用户确认域名）；相对地址相对清单地址解析，
// 并且**强制同源** —— 解析后换了域就直接拒绝，否则一份被篡改的清单就能把下载指向
// 任意域名。这两条都与宿主 internal/pluginfeed/resolve.go 保持一致。
func ResolveURL(feedURL, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("包地址为空")
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("解析地址 %q: %w", raw, err)
	}
	if ref.IsAbs() {
		if ref.Scheme != "http" && ref.Scheme != "https" {
			return "", fmt.Errorf("不支持的地址协议 %s（只允许 http/https）", ref.Scheme)
		}
		return ref.String(), nil
	}

	base, err := url.Parse(strings.TrimSpace(feedURL))
	if err != nil || base.Host == "" {
		return "", fmt.Errorf("相对地址 %q 需要知道清单位置才能核对（给 -feed-url 指定清单地址）", raw)
	}
	joined := base.ResolveReference(ref)
	if !strings.EqualFold(joined.Host, base.Host) || joined.Scheme != base.Scheme {
		return "", fmt.Errorf("相对地址 %q 解析后跳到了 %s，与订阅不同源", raw, joined.Host)
	}
	return joined.String(), nil
}

func verifyOneURL(ctx context.Context, client *http.Client, e Entry, absURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, absURL, nil)
	if err != nil {
		return fmt.Errorf("构造请求失败（%s）: %w", absURL, err)
	}
	req.Header.Set("User-Agent", "upkit-hub/feedcheck")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载 %s 失败: %w", absURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 %s 失败: HTTP %d", absURL, resp.StatusCode)
	}

	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, MaxPackageBytes+1))
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", absURL, err)
	}
	if n > MaxPackageBytes {
		return fmt.Errorf("包 %s 超过 %d 字节上限", absURL, MaxPackageBytes)
	}
	if got, want := hex.EncodeToString(h.Sum(nil)), NormalizeSHA256(e.Package.SHA256); got != want {
		return fmt.Errorf("插件 %s 的 %s 包内容与清单摘要不符: 清单 %s，实际 %s",
			e.Plugin.ID, e.Platform, want, got)
	}
	if e.Package.Size != 0 && e.Package.Size != n {
		return fmt.Errorf("插件 %s 的 %s 包大小与清单不符: 清单 %d，实际 %d",
			e.Plugin.ID, e.Platform, e.Package.Size, n)
	}
	return nil
}
