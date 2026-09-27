#!/usr/bin/env bash
#
# 生成「构建期摘要表」（cmd/upkit-hub/pinned_digests.json）。
#
# 有些上游（微信、QQ 这类）自己不发校验文件，而本仓库的规矩是「没有摘要就不发布产物」。
# 这个脚本在**发布前**把它们的产物下载一遍、算出 sha256 与大小，随插件一起发出去 ——
# 用户在装的时候仍然逐字节校验，只是摘要由我们在发布时实测。
#
#   bash scripts/gen-digests.sh [输出文件]
#
# 产物清单由插件自己给出（go run ./cmd/upkit-hub -list-pinned-artifacts）：哪个软件
# 需要构建期摘要、地址怎么拼、当前是哪个版本，都写在 catalog 里 —— 这个脚本不复制
# 那份知识，只负责下载与算摘要。
#
# 已下载过的产物按「地址 + 版本」缓存（默认 ~/.cache/upkit-hub/digests），版本没变就
# 不重复下载；CI 可以缓存这个目录。
#
# 注意：只有**地址里带版本号**的产物才该进这张表。滚动地址（同一个 URL 永远指最新）
# 绝不能进来 —— 表里的摘要明天就会失效，用户的安装会直接校验失败。
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${1:-"$repo_root/cmd/upkit-hub/pinned_digests.json"}
cache_dir=${UPKIT_HUB_DIGEST_CACHE:-"$HOME/.cache/upkit-hub/digests"}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "==> 列出需要在构建期算摘要的产物"
list_file="$work/list.tsv"
(cd "$repo_root" && go run ./cmd/upkit-hub -list-pinned-artifacts) >"$list_file"
count=$(grep -c . "$list_file" || true)
if [ "$count" -eq 0 ]; then
	echo "==> 没有软件需要构建期摘要，写出空表"
	printf '{}\n' >"$out"
	exit 0
fi
echo "    共 $count 个产物"

mkdir -p "$cache_dir" "$(dirname "$out")"
generated=$(date -u +%Y-%m-%dT%H:%M:%SZ)
failed_file="$work/failed"

# 下载一个产物（带重试），返回落盘路径。
#
# 这些安装包动辄几百 MB，所以要防「连接活着但不再传数据」的假死：--speed-limit /
# --speed-time 让 curl 在速率掉下去时限时放弃、交给外层重试，而不是把发布卡到
# job 超时。
fetch() { # url dest
	local url="$1" dest="$2"
	local i
	for i in 1 2 3 4 5; do
		if curl -fsSL --retry 3 --retry-all-errors \
			--connect-timeout 20 --speed-limit 20480 --speed-time 60 \
			-o "$dest.part" "$url"; then
			mv "$dest.part" "$dest"
			return 0
		fi
		echo "    重试（第 $i 次）：$url" >&2
		sleep 2
	done
	rm -f "$dest.part"
	return 1
}

{
	printf '{\n'
	first=1
	while IFS=$'\t' read -r url version appid; do
		[ -n "${url:-}" ] || continue
		key=$(printf '%s' "$url" | sha256sum | cut -c1-16)
		blob="$cache_dir/$key"
		keyver="$cache_dir/$key.version"

		if [ -f "$blob" ] && [ -f "$keyver" ] && [ "$(cat "$keyver")" = "$version" ]; then
			echo "    复用缓存：$appid v$version" >&2
		else
			echo "    下载：$appid v$version（$url）" >&2
			if ! fetch "$url" "$blob"; then
				printf '%s' "$url" >"$failed_file"
				break
			fi
			printf '%s' "$version" >"$keyver"
		fi

		sha=$(sha256sum "$blob" | cut -d' ' -f1)
		size=$(stat -c%s "$blob" 2>/dev/null || stat -f%z "$blob")
		[ "$first" = 1 ] || printf ',\n'
		first=0
		printf '  "%s": {"sha256": "%s", "size": %s, "version": "%s", "generatedAt": "%s"}' \
			"$url" "$sha" "$size" "$version" "$generated"
	done <"$list_file"
	printf '\n}\n'
} >"$out.tmp"

if [ -f "$failed_file" ]; then
	echo "生成失败：$(cat "$failed_file") 下载不下来（摘要表宁可不发，也不能少一条）" >&2
	rm -f "$out.tmp"
	exit 1
fi
if [ ! -s "$out.tmp" ]; then
	echo "生成失败：摘要表为空" >&2
	rm -f "$out.tmp"
	exit 1
fi
mv "$out.tmp" "$out"
echo "==> 已写出 $out（$count 条）"
