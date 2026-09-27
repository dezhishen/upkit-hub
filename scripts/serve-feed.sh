#!/usr/bin/env bash
#
# 用 https 静态服务器把 dist/release 起来，方便在 upkit 的「来源」面板里添加它、
# 走一遍真实的「拉清单 → 挑包 → 校验 → 落盘」。
#
# 为什么必须是 https：宿主的订阅地址解析会拒绝明文 http —— 明文链路中间任何人都能
# 整份替换清单，而清单自带的 sha256 保护不了清单自己（摘要只保护清单以下那一层）。
#
# 为什么不用装系统证书：UPKIT 是 Go 程序，认 SSL_CERT_FILE / SSL_CERT_DIR 这两个
# 环境变量。所以自签一张证书、把它交给 upkit 就够了，全程不需要 root。
#
# 用法:
#   bash scripts/serve-feed.sh [选项]
#
# 选项:
#   --dir <dir>        要被服务的目录（默认 dist/release）
#   --port <port>      监听端口（默认 8443）
#   --cert-dir <dir>   证书存放目录（默认 dist/serve）
#   -h, --help         显示本帮助
#
# 起来之后（另开一个终端）:
#   curl --cacert <cert-dir>/ca.crt https://127.0.0.1:<port>/feed.yaml
#   # 在 upkit 仓库里：
#   SSL_CERT_FILE=<本仓库绝对路径>/<cert-dir>/ca.crt ./dist/upkit-dev
#   # 然后在「来源」面板按 a 填入 https://127.0.0.1:<port>/feed.yaml

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

SERVE_DIR="dist/release"
PORT="8443"
CERT_DIR="dist/serve"

usage() {
  sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

die() { echo "错误: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)       SERVE_DIR="${2:-}"; shift 2 ;;
    --port)      PORT="${2:-}"; shift 2 ;;
    --cert-dir)  CERT_DIR="${2:-}"; shift 2 ;;
    -h|--help)   usage; exit 0 ;;
    *) die "未知参数: $1（用 -h 查看用法）" ;;
  esac
done

[[ -d "$SERVE_DIR" ]] || die "找不到目录 $SERVE_DIR（先跑 make feed）"
[[ -f "$SERVE_DIR/feed.yaml" ]] || die "$SERVE_DIR 里没有 feed.yaml（先跑 make feed）"
command -v python3 >/dev/null 2>&1 || die "需要 python3 来起静态服务器"
command -v openssl >/dev/null 2>&1 || die "需要 openssl 生成自签证书"

# ── 自签证书（一次生成，可重复使用）───────────────────────────
mkdir -p "$CERT_DIR"
CERT="$CERT_DIR/ca.crt"
KEY="$CERT_DIR/server.key"
if [[ ! -f "$CERT" || ! -f "$KEY" ]]; then
  echo "==> 生成自签证书 $CERT"
  # SAN 里同时写 localhost 与 127.0.0.1：Go 在 https 下会校验主机名，
  # 只写 CN 是不够的（现代客户端一律看 SAN）。
  openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
    -keyout "$KEY" -out "$CERT" \
    -subj "/CN=upkit-hub local dev" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1 \
    || die "生成自签证书失败"
  chmod 600 "$KEY"
else
  echo "==> 复用已有自签证书 $CERT"
fi

ABS_CERT="$ROOT_DIR/$CERT"
cat <<EOF

==> 已启动本地 https 静态服务
    目录:   $ROOT_DIR/$SERVE_DIR
    地址:   https://127.0.0.1:$PORT/feed.yaml
    证书:   $ABS_CERT

在另一个终端核对（不用关掉本服务）:

    curl --cacert $ABS_CERT https://127.0.0.1:$PORT/feed.yaml

或直接让 upkit 用这张证书（在 upkit 仓库里执行）:

    cd /path/to/upkit && make build-dev
    SSL_CERT_FILE=$ABS_CERT ./dist/upkit-dev

然后在「来源」面板按 a 添加 https://127.0.0.1:$PORT/feed.yaml，
确认信任 127.0.0.1，进入详情按 enter 安装。

注意：插件产物是 Windows 可执行文件，在 Linux/macOS 上能装（下载 + 校验 + 落盘）
但起不来 —— 要真跑起来得在 Windows 上。Ctrl-C 结束本服务。

EOF

SERVE_DIR="$SERVE_DIR" SERVE_PORT="$PORT" SERVE_CERT="$CERT" SERVE_KEY="$KEY" \
python3 - <<'PY'
import functools
import http.server
import os
import ssl

directory = os.path.abspath(os.environ["SERVE_DIR"])
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=directory)

ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(os.environ["SERVE_CERT"], os.environ["SERVE_KEY"])

server = http.server.ThreadingHTTPServer(("127.0.0.1", int(os.environ["SERVE_PORT"])), handler)
server.socket = ctx.wrap_socket(server.socket, server_side=True)
try:
    server.serve_forever()
except KeyboardInterrupt:
    pass
finally:
    server.server_close()
PY
