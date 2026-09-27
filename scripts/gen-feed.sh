#!/usr/bin/env bash
# 生成 upkit 官方订阅清单（feed.yaml）。
#
# 用法:
#   bash scripts/gen-feed.sh --version <ver> --relative [选项]
#   bash scripts/gen-feed.sh --version <ver> --base-url <url> [选项]
#
# 选项:
#   -o, --out <file>           输出文件（默认 dist/feed.yaml）
#       --plugins-dir <dir>    插件产物目录（默认 dist/plugins）
#       --version <ver>        feed 里 plugins[].version（一般与 tag 一致）
#       --relative             产物地址写成相对 feed 自己（./<文件>）
#       --base-url <url>       产物地址的绝对前缀（release 的 download 地址）
#       --schema <1|2>         清单 schema（默认 1）
#       --download-hosts <列表> 声明的下载域名，逗号分隔（仅 schema 2）
#       --plugin-hosts <列表>   插件自有网络访问域名，逗号分隔（仅 schema 2）
#       --name <id>=<名称>     覆盖某个插件的展示名（可重复）
#   -n, --name-default <名称>  订阅本身的名称
#       --mode <catalog|full>  插件的安装模式（默认 catalog）
#       --min-host-version <v> 要求的最低宿主版本（默认不写）
#   -h, --help                 显示本帮助
#
# 产物文件名约定 <插件ID>-<os>-<arch>[.exe]，例如：
#     upkit-hub-windows-amd64.exe  ->  id=upkit-hub, windows/amd64
#
# 为什么要有这个脚本：清单里按平台写死了产物地址与 sha256，必须与某一次插件发布
# 严格对应。手工填摘要迟早会错，所以由脚本在构建之后直接算出来。
#
# 它原本在 upkit 主仓库的 scripts/ 下，现已搬到本仓库：主仓库只做平台，插件制品与
# 清单都由 upkit-hub 发布。脚本不依赖本仓库的任何东西，只吃一个插件产物目录与地址
# 形态（--relative 或 --base-url），因此也可以拿去构建第三方清单。
#
# 两种地址形态的区别：
#   --relative 写出来的 ./<文件> 由**宿主相对订阅地址**解析。内置订阅地址是
#     .../releases/latest/download/feed.yaml，于是解析结果落在同一次发布的产物上
#     —— 同源、不需要额外的下载域名授权，而且自然跟着最新发布走。
#   --base-url 把地址写死成绝对地址（自建分发、内网镜像时用）；跨域时宿主会单独
#     向用户确认该域名。
#
# 关于 --schema：默认仍是 1（没有域名声明）。2 会额外写 download_hosts /
# plugin_hosts —— 这两个字段**只有认识到它们的宿主才认**，旧宿主在严格模式下会
# 解析失败（field not found），而官方源走 releases/latest，一发就是全量生效。
# 因此升级顺序必须是：先让宿主支持（它自己的 SchemaVersion 提到 2）并发布、
# 等用户升上去，再切这里的 --schema 2。
#
# 声明本身不要手写：由插件自己算（`go run ./cmd/upkit-hub -print-declarations`），
# 唯一事实来源是 cmd/upkit-hub 的 catalog。

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

OUT="dist/feed.yaml"
PLUGINS_DIR="dist/plugins"
VERSION=""
BASE_URL=""
RELATIVE=0
SCHEMA=1
DOWNLOAD_HOSTS=""
PLUGIN_HOSTS=""
FEED_NAME="upkit 官方源"
MODE="catalog"
MIN_HOST=""
ALLOW_PARTIAL=0
declare -A NAME_OVERRIDES=()

# upkit 支持的平台。改了这里请同步改 internal/feedcheck 的 SupportedPlatforms()
# （那个包里的测试会在两者不一致时报警）。它对应 upkit 主仓库
# internal/pluginfeed/schema.go 里的同一份列表，宿主才是权威：这里的检查只是
# 把「宿主会拒绝的清单」提前拦在发布之前。
SUPPORTED_PLATFORMS="windows/amd64 windows/arm64"

usage() {
  sed -n '2,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

die() { echo "错误: $*" >&2; exit 1; }

# 支持 --flag=value 的写法：拆成两个参数再进主循环。
#
# 插件自己报出来的声明就是这种形态（`go run ./cmd/upkit-hub -print-declarations`
# 打印的片段），直接 $(...) 接在命令行后面即可。
if (( $# > 0 )); then
  normalized=()
  for arg in "$@"; do
    case "$arg" in
      --*=*) normalized+=("${arg%%=*}" "${arg#*=}") ;;
      *)     normalized+=("$arg") ;;
    esac
  done
  set -- "${normalized[@]}"
fi

while [[ $# -gt 0 ]]; do
  case "$1" in
    -o|--out)            OUT="${2:-}"; shift 2 ;;
    --plugins-dir)       PLUGINS_DIR="${2:-}"; shift 2 ;;
    --version)           VERSION="${2:-}"; shift 2 ;;
    --relative)          RELATIVE=1; shift ;;
    --base-url)          BASE_URL="${2:-}"; shift 2 ;;
    --schema)            SCHEMA="${2:-}"; shift 2 ;;
    --download-hosts)    DOWNLOAD_HOSTS="${2:-}"; shift 2 ;;
    --plugin-hosts)      PLUGIN_HOSTS="${2:-}"; shift 2 ;;
    --name)              NAME_OVERRIDES["${2%%=*}"]="${2#*=}"; shift 2 ;;
    -n|--name-default)   FEED_NAME="${2:-}"; shift 2 ;;
    --mode)              MODE="${2:-}"; shift 2 ;;
    --min-host-version)  MIN_HOST="${2:-}"; shift 2 ;;
    --allow-partial)     ALLOW_PARTIAL=1; shift ;;
    -h|--help)           usage; exit 0 ;;
    *) die "未知参数: $1（用 -h 查看用法）" ;;
  esac
done

[[ -n "$VERSION" ]] || die "缺少 --version（一般传 tag 去掉 v 前缀的版本号）"
if (( RELATIVE == 1 )); then
  [[ -z "$BASE_URL" ]] || die "--relative 与 --base-url 只能给一个：它们决定产物地址的写法"
else
  [[ -n "$BASE_URL" ]] || die "缺少产物地址形态：给 --relative（相对 feed 自己）或 --base-url <前缀>"
  BASE_URL="${BASE_URL%/}"
fi
[[ -d "$PLUGINS_DIR" ]] || die "找不到插件产物目录 $PLUGINS_DIR"
case "$SCHEMA" in 1|2) ;; *) die "--schema 只能是 1 或 2" ;; esac
if (( SCHEMA < 2 )); then
  # 别写了声明却忘了升 schema：那样存量宿主会在严格模式下直接解析失败。
  [[ -z "$DOWNLOAD_HOSTS" && -z "$PLUGIN_HOSTS" ]] \
    || die "域名声明需要 --schema 2（写进 schema 1 会让存量宿主解析失败）"
fi
case "$MODE" in catalog|full) ;; *) die "--mode 只能是 catalog 或 full" ;; esac

# ── 摘要工具（Linux 用 sha256sum，macOS 用 shasum）────────────
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "找不到 sha256sum 或 shasum，无法计算摘要"
  fi
}

size_of() { wc -c <"$1" | tr -d ' '; }

# emit_hosts 把逗号分隔的域名写成 YAML 列表（只在 schema 2 里用）。
emit_hosts() {
  local field="$1" list="$2" item
  [[ -n "$list" ]] || return 0
  printf '    %s:\n' "$field"
  local IFS=','
  for item in $list; do
    printf '      - %s\n' "$item"
  done
}

# ── 扫描产物，按插件归组 ──────────────────────────────────────
# 用「id|平台|文件名」三列的临时文本收集，最后排序输出，保证同样输入得到同样的文件
# （Diff 友好，也方便人核对）。
entries="$(mktemp)"
trap 'rm -f "$entries"' EXIT

shopt -s nullglob
found=0
for file in "$PLUGINS_DIR"/*; do
  [[ -f "$file" ]] || continue
  name="$(basename "$file")"
  stem="${name%.exe}"
  if [[ ! "$stem" =~ ^(.+)-(windows|linux|darwin)-(amd64|arm64|386|arm)$ ]]; then
    echo "跳过（文件名不符合 <插件ID>-<os>-<arch> 约定）: $name" >&2
    continue
  fi
  id="${BASH_REMATCH[1]}"
  os_name="${BASH_REMATCH[2]}"
  arch="${BASH_REMATCH[3]}"
  if [[ "$os_name" != "windows" ]]; then
    echo "跳过（upkit 只支持 Windows）: $name" >&2
    continue
  fi
  printf '%s|%s/%s|%s|%s|%s\n' "$id" "$os_name" "$arch" "$name" "$(sha256_of "$file")" "$(size_of "$file")" >>"$entries"
  found=$((found + 1))
done

(( found > 0 )) || die "$PLUGINS_DIR 里没有可用的 Windows 插件产物"

# ── 发布前检查：每个插件必须覆盖全部受支持平台 ──────────────────
# 漏一个架构，那个架构的用户会在「校验订阅」这一步失败 —— 而那本来可以在发布前
# 就发现。确实有意只发部分架构时用 --allow-partial 显式跳过。
if (( ALLOW_PARTIAL == 0 )); then
  for id in $(cut -d'|' -f1 "$entries" | LC_ALL=C sort -u); do
    for platform in $SUPPORTED_PLATFORMS; do
      if ! grep -q "^${id}|${platform}|" "$entries"; then
        die "插件 ${id} 缺少 ${platform} 的产物（如确实只发部分平台，加 --allow-partial）"
      fi
    done
  done
fi

# ── 生成 YAML ─────────────────────────────────────────────────
mkdir -p "$(dirname "$OUT")"
{
  cat <<'HEADER'
# upkit 官方订阅清单（由 scripts/gen-feed.sh 自动生成，请勿手工编辑）
#
# 它由 upkit-hub 发布，主仓库不再产清单。内置订阅读的是：
#   https://github.com/dezhishen/upkit-hub/releases/latest/download/feed.yaml
#
# 清单与插件产物分开放，是因为它按平台写死了产物地址与 sha256，必须与某一次发布
# 严格对应；留在主仓库会跟代码提交搅在一起被顺手改掉，让已发布版本的行为跟着漂移。
#
# 产物地址是相对形式（./<文件>）时，由宿主相对**本清单位置**解析：内置地址取的就是
# 同一次发布里的 feed.yaml，于是解析结果就是同一次发布的产物（同源、无需额外授权）。
#
# 摘要由产物现算，因此与产物天然一致。

HEADER
  printf 'schema: %s\n' "$SCHEMA"
  printf 'name: %s\n' "$FEED_NAME"
  printf 'updated_at: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'plugins:\n'

  # 按 id 排序；平台按 amd64 → arm64 的固定顺序
  current_id=""
  while IFS='|' read -r id platform file sha size; do
    [[ -n "$id" ]] || continue
    if [[ "$id" != "$current_id" ]]; then
      if [[ -n "$current_id" ]]; then
        printf '\n'
      fi
      current_id="$id"
      printf '  - id: %s\n' "$id"
      printf '    name: %s\n' "${NAME_OVERRIDES[$id]:-$id}"
      printf '    version: %s\n' "$VERSION"
      printf '    mode: %s\n' "$MODE"
      [[ -n "$MIN_HOST" ]] && printf '    min_host_version: "%s"\n' "$MIN_HOST"
      if (( SCHEMA >= 2 )); then
        # 两类域名分开写：前者宿主会强制校验（超出就拒绝下载），后者只是告知。
        emit_hosts download_hosts "$DOWNLOAD_HOSTS"
        emit_hosts plugin_hosts "$PLUGIN_HOSTS"
      fi
      printf '    packages:\n'
    fi
    printf '      %s:\n' "$platform"
    if (( RELATIVE == 1 )); then
      # 必须是 ./<文件>，不能写成 /<文件>：以 / 开头是「相对 origin 根」，会解析成
      # https://github.com/<文件> —— 那不是发布里的附件。实测（宿主 ResolveLocation）：
      #   ./upkit-hub-windows-amd64.exe
      #     -> https://github.com/dezhishen/upkit-hub/releases/latest/download/upkit-hub-windows-amd64.exe
      #   /upkit-hub-windows-amd64.exe
      #     -> https://github.com/upkit-hub-windows-amd64.exe   ← 404
      # 本地自签服务下两者恰好一样（清单就在服务根目录），所以这个错在本地看不出来。
      printf '        url: ./%s\n' "$file"
    else
      printf '        url: %s/%s\n' "$BASE_URL" "$file"
    fi
    printf '        sha256: %s\n' "$sha"
    printf '        size: %s\n' "$size"
  done < <(LC_ALL=C sort -t'|' -k1,1 -k2,2 "$entries")
} >"$OUT"

echo "==> 已生成订阅清单 $OUT"
echo "    版本:   $VERSION"
echo "    schema: $SCHEMA"
echo "    地址:   $( ((RELATIVE == 1)) && echo '相对（./<文件>，由宿主相对订阅地址解析）' || echo "绝对（$BASE_URL）" )"
echo "    插件:   $(LC_ALL=C sort -t'|' -k1,1 -u "$entries" | cut -d'|' -f1 | paste -sd' ' -)"
echo "    平台:   $(cut -d'|' -f2 "$entries" | LC_ALL=C sort -u | paste -sd' ' -)"
