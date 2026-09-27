# upkit-hub 构建入口
#
# 官方插件库：托管插件制品与订阅清单。宿主内置的订阅地址是
#
#   https://github.com/dezhishen/upkit-hub/releases/latest/download/feed.yaml
#
# 于是有两条硬约束：feed.yaml 必须作为 release 附件发布且文件名精确为 feed.yaml
# （所以每更新一次清单就要发一个 release），附件与清单里的地址、摘要必须严格对应。
# 摘要因此一律由脚本在构建之后现算 —— 手工填迟早会错。
#
# 所有命令都基于 POSIX shell，可在 Linux / macOS / Git Bash / WSL 下运行，不依赖
# PowerShell。

SHELL := /bin/sh
GO    ?= go
DIST  := dist

PLUGIN_ID      := upkit-hub
PLUGIN_PACKAGE := ./cmd/upkit-hub
PLATFORMS      := windows/amd64 windows/arm64
PLUGINS_DIR    := $(DIST)/plugins
RELEASE_DIR    := $(DIST)/release

# 版本号与宿主同一个 tag（形如 v0.1.0）。注入插件与清单时去掉 v 前缀。
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
PLUGIN_VERSION  = $(patsubst v%,%,$(VERSION))

# 清单里产物地址的形态：默认写成相对形式（./<文件>），由宿主相对**订阅地址**解析。
# 内置订阅地址是 .../releases/latest/download/feed.yaml，于是相对地址正好落在同一次
# 发布里 —— 同源，用户不需要额外确认「下载域名」，也自然跟着最新发布走。
#
# 需要绝对地址时（自建分发、内网镜像）在命令行给 BASE_URL：
#   make feed BASE_URL=https://cdn.example.com/upkit-hub
BASE_URL ?=
URL_ARGS = $(if $(BASE_URL),--base-url $(BASE_URL),--relative)

# 核对相对地址时用来解析的清单地址（默认本地自签 https 源）。
FEED_URL ?= https://127.0.0.1:8443/feed.yaml

# 清单 schema：默认 1（不含域名声明）。
#
# 切到 2 的前提是**宿主已经支持**（它自己的 SchemaVersion >= 2）并已发布、用户升级
# 到位 —— 官方源走 releases/latest，一发就全量生效，而旧宿主在严格模式下遇到不认识
# 的字段会整份拒绝。声明本身不手写，由插件从 catalog 推导：
#
#   make feed SCHEMA=2
SCHEMA ?= 1

FEED_NAME   ?= upkit 官方源
PLUGIN_NAME ?= $(PLUGIN_ID)=upkit 官方插件集

.PHONY: all check fmt fmt-check headers-check vet test tidy \
        plugins feed release-local verify serve clean help

all: check

check: fmt-check headers-check vet test ## 本地质量门禁

fmt: ## 格式化代码
	$(GO) fmt ./...

fmt-check: ## 检查代码是否已格式化
	@if ! unformatted=$$(gofmt -l .); then \
		echo "gofmt 解析失败（通常是语法错误，比如重复的 package 行）"; \
		exit 1; \
	fi; \
	if [ -n "$$unformatted" ]; then \
		echo "以下文件未格式化，请运行 make fmt："; echo "$$unformatted"; exit 1; \
	fi

headers-check: ## 检查文件头（package 唯一、//go:build 在第 1 行）
	@bash scripts/check-headers.sh

vet: ## go vet 静态检查
	$(GO) vet ./...

test: ## 运行单元测试（含发布脚本的测试）
	$(GO) test -count=1 ./...

tidy: ## 整理 go.mod / go.sum
	$(GO) mod tidy

digests: ## 生成构建期摘要表（把需要它的产物下载一遍，实测 sha256）
	bash scripts/gen-digests.sh

plugins: ## 交叉编译两个架构的插件产物到 dist/plugins
	@for target in $(PLATFORMS); do \
		bash scripts/build-plugin.sh --release-name -v "$(PLUGIN_VERSION)" \
			-t "$$target" $(PLUGIN_PACKAGE) || exit 1; \
	done

# feed 依赖 digests：摘要表是**编进插件**的（go:embed），所以必须先算表再编译，
# 否则发出去的插件里带着上一版的摘要。
feed: digests plugins ## 组装 dist/release/ 并生成清单 feed.yaml（SCHEMA=2 时写入域名声明）
	@rm -rf $(RELEASE_DIR)
	@mkdir -p $(RELEASE_DIR)
	@cp $(PLUGINS_DIR)/*.exe $(RELEASE_DIR)/
	@decl=""; \
	if [ "$(SCHEMA)" = "2" ]; then \
		decl="$$($(GO) run ./cmd/upkit-hub -print-declarations | tr '\n' ' ')"; \
		echo "==> 域名声明: $$decl"; \
	fi; \
	bash scripts/gen-feed.sh --plugins-dir $(RELEASE_DIR) \
		--version "$(PLUGIN_VERSION)" \
		$(URL_ARGS) \
		--schema "$(SCHEMA)" \
		$$decl \
		--name "$(PLUGIN_NAME)" \
		-n "$(FEED_NAME)" \
		-o $(RELEASE_DIR)/feed.yaml

release-local: feed ## 本地走一遍发布：构建 + 清单 + 校验
	$(GO) run ./cmd/feedcheck -feed $(RELEASE_DIR)/feed.yaml -artifacts $(RELEASE_DIR)

verify: ## 校验 dist/release（含逐个下载清单里的包核对摘要）
	$(GO) run ./cmd/feedcheck -feed $(RELEASE_DIR)/feed.yaml \
		-artifacts $(RELEASE_DIR) -feed-url $(FEED_URL) -check-urls -insecure

serve: ## 起本地 https 静态服务（自签证书），把 dist/release 暴露给 upkit
	bash scripts/serve-feed.sh

clean: ## 清理构建产物
	rm -rf $(DIST) coverage.out

help: ## 显示本帮助
	@echo "可用目标："
	@echo "  check          格式化 + 文件头 + vet + 测试"
	@echo "  digests        生成构建期摘要表（下载产物实测 sha256）"
	@echo "  plugins        构建两个架构的插件产物"
	@echo "  feed           生成 dist/release/feed.yaml（SCHEMA=2 时带域名声明）"
	@echo "  release-local  plugins + feed + 校验"
	@echo "  serve          起本地 https 静态服务（自签证书）"
	@echo "  verify         校验 dist/release 并下载核对"
	@echo "  clean          清理产物"
