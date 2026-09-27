package feedcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxPackageBytes 是单个插件包的大小上限，与宿主一致。
const MaxPackageBytes = 256 << 20

// VerifyURLs 逐个下载清单里的包并核对摘要与大小。
//
// 它把「清单声明的东西真的存在、且内容与摘要一致」也验掉：release 附件名写错、
// 漏传文件、--base-url 拼错、清单与产物来自两次发布 —— 这些都会在这里现形，
// 而不是等用户点「安装」时才失败。
//
// 只校验受支持平台（Windows 的那两个）；其它平台的包不下载也不报错。
func (f *Feed) VerifyURLs(ctx context.Context, client *http.Client) error {
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
		if err := verifyOneURL(ctx, client, e); err != nil {
			return err
		}
	}
	return nil
}

func verifyOneURL(ctx context.Context, client *http.Client, e Entry) error {
	raw := strings.TrimSpace(e.Package.URL)
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("插件 %s 的 %s 包地址不是 http(s)：%s", e.Plugin.ID, e.Platform, raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return fmt.Errorf("构造请求失败（%s）: %w", raw, err)
	}
	req.Header.Set("User-Agent", "upkit-hub/feedcheck")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载 %s 失败: %w", raw, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 %s 失败: HTTP %d", raw, resp.StatusCode)
	}

	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, MaxPackageBytes+1))
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", raw, err)
	}
	if n > MaxPackageBytes {
		return fmt.Errorf("包 %s 超过 %d 字节上限", raw, MaxPackageBytes)
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
