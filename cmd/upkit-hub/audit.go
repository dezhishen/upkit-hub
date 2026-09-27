package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/dezhishen/upkit-hub/internal/softwarehub"
)

// auditSoftwareHub 把下载导航站上的软件全部扫一遍，逐条列出「具备什么、还缺什么」。
//
// 存在的理由：那个站上有七十多个软件，但它是一份**导航**清单，不是安装清单。要判断
// 某个软件能不能接进 upkit，得看三件事 —— 产物地址是否不可变、摘要从哪来、安装契约
// （静默参数 / 安装目录 / 入口 / 进程名）能不能写死。前两件能从数据里看出来，第三件
// 只能靠人补。这个命令把能看出来的部分摊开，避免每次讨论「接哪个」都要重新抓一遍。
//
// 它只陈述事实、不下结论：一个软件被标成「待补安装契约」，意思是数据看起来是通的，
// 但接入前仍要像 LibreOffice 那样把地址与摘要逐条核实。
func auditSoftwareHub(w io.Writer, baseURL string) error {
	ctx := context.Background()
	client := &softwarehub.Client{BaseURL: baseURL}

	idx, err := client.Index(ctx)
	if err != nil {
		return err
	}
	list, err := client.List(ctx)
	if err != nil {
		return err
	}

	integrated := map[string]bool{}
	for _, spec := range catalog {
		integrated[spec.id] = true
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "软件 id\t版本\tWindows 架构\t直链\t网页/商店\t状态")
	fmt.Fprintln(tw, "-------\t----\t-----------\t----\t---------\t----")

	var (
		pageOnly, placeholder, candidates, done int
		failed                                  []string
	)
	for _, item := range list.Items {
		payload, err := client.VersionsAt(ctx, item.Source.Path)
		if err != nil {
			// 单个软件取不到不该打断整张表，但要在末尾汇总出来。
			failed = append(failed, fmt.Sprintf("%s（%v）", item.ID, err))
			fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t%s\n", item.ID, "取数据失败")
			continue
		}
		f := payload.Facts()
		status := classify(f)
		switch status {
		case statusPageOnly:
			pageOnly++
		case statusPlaceholder:
			placeholder++
		case statusCandidate:
			candidates++
		}
		version := "-"
		if len(f.Versions) > 0 {
			version = strings.Join(f.Versions, "/")
		}
		if integrated[item.ID] {
			status = "已接入"
			done++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n",
			item.ID, version, strings.Join(f.Arches, ","), f.DirectLinks, f.PageOnlyLinks, status)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\n共 %d 个软件（站点数据生成于 %s）：已接入 %d，待补安装契约 %d，版本不可比较 %d，只有网页/商店入口 %d\n",
		len(list.Items), idx.Meta.GeneratedAt, done, candidates, placeholder, pageOnly)
	fmt.Fprint(w, `
接入一个软件需要同时满足三条：
  1. 产物地址不可变 —— 地址里必须带版本号（"永远指最新"的转发地址上没法固定摘要）；
  2. 摘要可得 —— 上游官方渠道给出的 sha256（同目录 .sha256 校验文件、接口字段…），
     没有摘要就不发布产物；
  3. 安装契约写得死 —— 静默参数、安装目录、入口可执行文件、升级前要关的进程名。
     这三条里只有第 1、2 条能从这张表看出来，第 3 条必须逐个软件核实。
`)
	if len(failed) > 0 {
		fmt.Fprintf(w, "\n以下软件的数据没取到（不影响其余统计）：\n  %s\n", strings.Join(failed, "\n  "))
	}
	return nil
}

// 审计结论的取值。
const (
	statusPageOnly    = "只有网页/商店入口"
	statusPlaceholder = "版本是占位值"
	statusCandidate   = "待补安装契约"
)

// classify 按数据本身能看出来的事实给一个状态。
func classify(f softwarehub.Facts) string {
	switch {
	case f.DirectLinks == 0:
		return statusPageOnly
	case f.AllPlaceholderVersions():
		return statusPlaceholder
	default:
		return statusCandidate
	}
}
