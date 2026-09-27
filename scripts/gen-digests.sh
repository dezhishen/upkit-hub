#!/usr/bin/env bash
#
# 生成「构建期摘要表」（cmd/upkit-hub/pinned_digests.json）。
#
# 有些上游（微信、百度网盘这类）自己不发校验文件，而本仓库的规矩是「没有摘要就不发布
# 产物」。这个脚本把它们的产物下载一遍、实测 sha256 与大小，随插件一起发出去 —— 用户
# 在装的时候仍然逐字节校验，只是摘要由我们在发布时实测。
#
#   bash scripts/gen-digests.sh [输出文件] [--strict]
#
# 产物清单由插件自己给出（go run ./cmd/upkit-hub -list-pinned-artifacts）：哪个软件
# 需要构建期摘要、地址怎么拼、当前是哪个版本，都写在 catalog 里 —— 这个脚本不复制
# 那份知识，只负责下载与算摘要。
#
# 两条重要行为：
#
#   1. **能复用就不下载**。表里已经有一条同地址、同版本的记录时直接用（这就是提交进
#      仓库的那份表）。CI 上因此完全不需要访问这些 CDN；
#   2. **取不到不等于发布失败**（除非给 --strict）。这些安装包在几百 MB，而且厂商 CDN
#      对海外 IP 常常直接拒绝（实测从 GitHub 的 runner 上就是 403）。取不到的那一条
#      不写进表：那个软件在用户那边会「查不到可用版本」，而不是「装到没有校验的包」——
#      这是有意的降级方向。
#
# 所以：**上游发新版之后，要在能访问这些 CDN 的机器上跑一次并提交结果**，否则那几个
# 软件会一直停在旧版本（见 README）。已下载过的产物按「地址 + 版本」缓存在
# ~/.cache/upkit-hub/digests，版本没变就不重复下载。
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=""
strict=0
for arg in "$@"; do
	case "$arg" in
	--strict) strict=1 ;;
	*) out="$arg" ;;
	esac
done
out=${out:-"$repo_root/cmd/upkit-hub/pinned_digests.json"}
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

# 现有表：先当作「可复用的来源」。它可能不存在（第一次生成）。
existing="$work/existing.json"
if [ -f "$out" ] && [ -s "$out" ]; then
	cp "$out" "$existing"
else
	printf '{}\n' >"$existing"
fi

# entry_of 从现有表里取一条记录，按空格分隔打印：<sha256> <size> <version> <generatedAt>。
# 表是本脚本生成的固定形状，所以这里用 grep/sed 就够，不必引入 jq 或 python。
entry_of() { # url table
	local url="$1" table="$2" line
	line=$(grep -o "\"$url\": {[^}]*}" "$table" 2>/dev/null | head -1) || return 1
	[ -n "$line" ] || return 1
	local sha size version at
	sha=$(printf '%s' "$line" | sed -n 's/.*"sha256": "\([0-9a-f]*\)".*/\1/p')
	size=$(printf '%s' "$line" | sed -n 's/.*"size": \([0-9]*\).*/\1/p')
	version=$(printf '%s' "$line" | sed -n 's/.*"version": "\([^"]*\)".*/\1/p')
	at=$(printf '%s' "$line" | sed -n 's/.*"generatedAt": "\([^"]*\)".*/\1/p')
	[ -n "$sha" ] && [ -n "$size" ] && [ -n "$version" ] || return 1
	printf '%s %s %s %s' "$sha" "$size" "$version" "$at"
}

# 下载一个产物（带重试），返回落盘路径。
#
# 这些安装包动辄几百 MB，所以要防「连接活着但不再传数据」的假死：--speed-limit /
# --speed-time 让 curl 在速率掉下去时限时放弃、交给外层重试，而不是把发布卡到
# job 超时。
fetch() { # url dest
	local url="$1" dest="$2"
	local i
	for i in 1 2 3; do
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

reused=0
fetched=0
skipped="$work/skipped.tsv"
: >"$skipped"

{
	printf '{\n'
	first=1
	emit() { # url sha size version [generatedAt]
		[ "$first" = 1 ] || printf ',\n'
		first=0
		printf '  "%s": {"sha256": "%s", "size": %s, "version": "%s", "generatedAt": "%s"}' \
			"$1" "$2" "$3" "$4" "${5:-$generated}"
	}
	while IFS=$'\t' read -r url version appid; do
		[ -n "${url:-}" ] || continue

		# 1) 现有表里已有同版本记录：直接用，一个字节都不下载。
		if record=$(entry_of "$url" "$existing") && [ "$(printf '%s' "$record" | cut -d' ' -f3)" = "$version" ]; then
			# shellcheck disable=SC2086
			set -- $record
			echo "    复用表中记录：$appid v$version" >&2
			# 原样沿用旧的 generatedAt：复用不是重新生成，改时间戳只会制造无谓的 diff。
			emit "$url" "$1" "$2" "$3" "$4"
			reused=$((reused + 1))
			continue
		fi

		# 2) 本地缓存命中（地址哈希 + 版本号）同样不下载。
		key=$(printf '%s' "$url" | sha256sum | cut -c1-16)
		blob="$cache_dir/$key"
		keyver="$cache_dir/$key.version"
		if [ -f "$blob" ] && [ -f "$keyver" ] && [ "$(cat "$keyver")" = "$version" ]; then
			echo "    复用缓存：$appid v$version" >&2
		else
			echo "    下载：$appid v$version（$url）" >&2
			if ! fetch "$url" "$blob"; then
				echo "    取不到（跳过这一条）：$appid v$version" >&2
				printf '%s\t%s\t%s\n' "$url" "$version" "$appid" >>"$skipped"
				continue
			fi
			printf '%s' "$version" >"$keyver"
		fi

		sha=$(sha256sum "$blob" | cut -d' ' -f1)
		size=$(stat -c%s "$blob" 2>/dev/null || stat -f%z "$blob")
		emit "$url" "$sha" "$size" "$version"
		fetched=$((fetched + 1))
	done <"$list_file"
	printf '\n}\n'
} >"$out.tmp"

skip_count=$(grep -c . "$skipped" || true)
if [ "$skip_count" -gt 0 ]; then
	echo "==> 有 $skip_count 个产物取不到，未写入摘要表：" >&2
	sed 's/^/    /' "$skipped" >&2
	if [ "$strict" = 1 ]; then
		echo "==> --strict：取不到就算失败（本地刷新摘要表时用）" >&2
		rm -f "$out.tmp"
		exit 1
	fi
	echo "    这些软件在用户那边会「查不到可用版本」；要在能访问这些 CDN 的机器上重新生成。" >&2
fi

if [ ! -s "$out.tmp" ]; then
	echo "生成失败：摘要表为空" >&2
	rm -f "$out.tmp"
	exit 1
fi
mv "$out.tmp" "$out"
echo "==> 已写出 $out（复用 $reused 条，新下载 $fetched 条，跳过 $skip_count 条）"
