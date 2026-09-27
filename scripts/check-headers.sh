#!/usr/bin/env bash
#
# 检查 Go 文件的文件头是否合法：
#   1. package 声明必须恰好出现一次；
#   2. //go:build 必须位于第 1 行。
#
# 为什么值得单独检查这两条：
#
#   · 重复的 package 行会让编译直接失败，好抓；
#   · 而 //go:build 一旦不在第 1 行，Go 会**静默忽略**它 —— 那个文件就被无条件
#     编译进去。Linux 专用的实现可能被打进 Windows 二进制，只要两边没有符号冲突
#     就一声不响，直到运行时行为诡异才发现。
#
# 这类损坏的常见成因：编辑器或工具往 //go:build 前面补了一行 `package <目录名>`。
# 实测：以 //go:build 开头的文件会被补，以普通注释开头的文件不会。

set -euo pipefail

fail=0
checked=0

report() {
  echo "错误: $1"
  fail=1
}

while IFS= read -r file; do
  checked=$((checked + 1))

  # ── 1. package 声明恰好一次 ──
  count="$(grep -c '^package ' "$file" || true)"
  if (( count != 1 )); then
    report "$file 有 ${count} 个 package 声明（应当恰好 1 个）"
    grep -n '^package ' "$file" | sed 's/^/    /' || true
  fi

  # ── 2. //go:build 必须在第 1 行 ──
  if grep -q '^//go:build' "$file"; then
    first="$(head -n 1 "$file")"
    case "$first" in
      //go:build*) ;;
      *)
        report "$file 的 //go:build 不在第 1 行（Go 会忽略它）"
        echo "    首行是: ${first}"
        ;;
    esac
  fi
done < <(find . -name '*.go' -not -path './vendor/*' | LC_ALL=C sort)

if (( fail != 0 )); then
  echo
  echo "修复提示：package 行必须在文件最前面，//go:build 必须在它上方、占据第 1 行。"
  echo "若某个工具往 //go:build 前面插了 package <目录名>，删掉那一行即可。"
  exit 1
fi

echo "文件头检查通过（${checked} 个 Go 文件）"
