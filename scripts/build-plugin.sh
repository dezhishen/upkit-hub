#!/usr/bin/env bash
#
# upkit 插件构建脚本（纯 bash，可在 Linux / macOS / Git Bash / WSL 下运行）。
#
# 插件是独立可执行文件，必须与宿主同平台同架构（宿主是 Windows 时插件也要是 .exe）。
#
# 用法：
#   bash scripts/build-plugin.sh                          # 构建示例插件（默认 windows/amd64）
#   bash scripts/build-plugin.sh -t windows/arm64         # 换一个架构
#   bash scripts/build-plugin.sh ./cmd/my-plugin          # 构建自己的插件包
#   bash scripts/build-plugin.sh --release-name -v 1.2.3  # 发布用：产物带平台后缀并注入版本
#
# 输出：dist/plugins/<插件目录名>[.exe]
#       --release-name 时为 dist/plugins/<插件目录名>-<os>-<arch>[.exe]
#       （发布流水线需要这个命名，脚本 gen-feed.sh 靠它识别 id 与平台）
#
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

OUT_DIR="dist/plugins"
DEFAULT_PACKAGE="./cmd/upkit-hub"

usage() {
  sed -n '2,14p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  cat <<'EOF'

选项:
  -t, --target <GOOS/GOARCH>  目标平台（默认 windows/amd64）
  -o, --out <dir>             输出目录（默认 dist/plugins）
  -v, --version <ver>         注入插件版本号（默认 dev）
      --release-name          产物名带 <os>-<arch> 后缀（发布用）
  -h, --help                  显示本帮助
EOF
}

# upkit 只发行 Windows 版本，插件必须与宿主同平台，所以默认交叉编译到 windows/amd64。
# 开发机（Linux/macOS）上照样能构建，只是产物是 Windows 可执行文件。
GOOS_NAME="windows"
GOARCH_NAME="amd64"
TARGET=""
PACKAGE=""
PLUGIN_VERSION="dev"
RELEASE_NAME=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    -t|--target)     TARGET="${2:-}"; shift 2 ;;
    -o|--out)        OUT_DIR="${2:-}"; shift 2 ;;
    -v|--version)    PLUGIN_VERSION="${2:-}"; shift 2 ;;
    --release-name)  RELEASE_NAME=1; shift ;;
    -h|--help)       usage; exit 0 ;;
    -*)              echo "未知参数: $1" >&2; usage >&2; exit 2 ;;
    *)               PACKAGE="$1"; shift ;;
  esac
done

if [[ -n "$TARGET" ]]; then
  GOOS_NAME="${TARGET%%/*}"
  GOARCH_NAME="${TARGET##*/}"
  if [[ -z "$GOOS_NAME" || -z "$GOARCH_NAME" || "$GOOS_NAME" == "$GOARCH_NAME" ]]; then
    echo "错误: 目标格式应为 GOOS/GOARCH，收到 '$TARGET'" >&2
    exit 2
  fi
fi

if [[ -z "$PACKAGE" ]]; then
  PACKAGE="$DEFAULT_PACKAGE"
  echo "==> 未指定包路径，构建示例插件 ${PACKAGE}"
fi

if ! command -v go >/dev/null 2>&1; then
  echo "错误: 未找到 go 命令，请先安装 Go 工具链" >&2
  exit 1
fi

name="$(basename "$PACKAGE")"
bin_name="$name"
if (( RELEASE_NAME == 1 )); then
  # 发布产物带平台后缀：gen-feed.sh 靠它同时识别插件 id 与平台。
  bin_name="${name}-${GOOS_NAME}-${GOARCH_NAME}"
fi
if [[ "$GOOS_NAME" == "windows" ]]; then
  bin_name="${bin_name}.exe"
fi

mkdir -p "$OUT_DIR"
out_path="${OUT_DIR}/${bin_name}"

echo "==> 构建插件 ${GOOS_NAME}/${GOARCH_NAME} -> ${out_path}"
echo "    版本:   ${PLUGIN_VERSION}"
CGO_ENABLED=0 GOOS="$GOOS_NAME" GOARCH="$GOARCH_NAME" \
  go build -trimpath -ldflags "-s -w -X main.version=${PLUGIN_VERSION}" -o "$out_path" "$PACKAGE"

echo
echo "==> 完成。部署方式："
echo "    1. 把 ${bin_name} 放到 upkit 的 plugin/ 目录（与 upkit 可执行文件同级）"
echo "    2. 同时放一个 ${name}.plugin.yaml 描述文件："
cat <<EOF

       id: ${name}
       name: 我的插件
       mode: catalog

EOF
echo "    3. 首次启动 upkit，日志会给出该文件的 sha256（「插件来源未启用 ...」）"
echo "       把哈希写进 apps.yaml 的 sources[].trust 后即可启用"
