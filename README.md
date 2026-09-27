# upkit-hub

> upkit 的官方插件库：托管插件制品与订阅清单 `feed.yaml`。

upkit 主仓库只做平台本体，另外把一个订阅地址编进二进制；插件的**制品**与**清单**都由
本仓库发布。两者分开是有原因的：清单里按平台写死了产物地址与 sha256，必须与某一次
插件发布严格对应 —— 留在主仓库会跟代码提交搅在一起被顺手改掉，让已发布版本的行为
跟着漂移。

## 两层结构（先分清这个）

这个仓库发布的东西分两层，**两层的机制完全不同**；混在一起看很容易绕晕：

```
   订阅地址（编在 upkit 二进制里，改不了）
   https://github.com/dezhishen/upkit-hub/releases/latest/download/feed.yaml
                          │
   第 1 层：插件包          │  清单只列「插件」本身：upkit-hub-windows-<arch>.exe
                          ▼
   feed.yaml ──► upkit 的订阅路径：域名授权 → 下载 → sha256 校验 → 落盘 plugin/
                          │
   第 2 层：软件包          │  upkit 启动插件子进程，问它：有哪些软件、哪些版本、下载什么
                          ▼
   插件的 Versions("ungoogled-chromium") ──► 上游（GitHub Releases / 索引站）的直链 + sha256
                          │
                          ▼
   upkit 的四轴（下载 → 解包 → 落地 → 探测）把软件装到本机
```

|  | 第 1 层：插件包 | 第 2 层：软件包 |
| --- | --- | --- |
| 谁描述 | `feed.yaml`（本仓库由 `gen-feed.sh` 生成） | 插件的 `Versions()`（`cmd/upkit-hub/`） |
| 什么时候确定 | 发布时写死 | 运行时向上游查询 |
| 地址形态 | `./upkit-hub-windows-<arch>.exe`（相对订阅地址） | 上游给的绝对地址 |
| 谁下载 | upkit 的订阅路径（`internal/pluginfeed`） | upkit 的四轴（`internal/engine`） |
| 摘要谁校验 | upkit：与清单里的 sha256 强制比对，不一致即失败 | **目前不校验**插件给的 Digest（upkit 侧待补） |
| 本仓库的相关代码 | `scripts/gen-feed.sh`、`internal/feedcheck`、CI | `cmd/upkit-hub/`、`internal/ucbinaries` |

订阅装完之后还有一条信任链要走（都在 upkit 侧）：订阅功能开关 → 订阅域名（添加时确认）→
下载域名（仅跨域绝对地址时需要）→ **插件 sha256 写进清单的 `sources[].trust`** —— 最后
这一步没做，插件不会被启动（界面上会显示「未信任 · 需在清单里写入 sha256」）。

## 域名声明（schema 2）

第 2 层的下载地址是插件**运行时**给出的，用户添加订阅时看不到它们。于是清单可以把它
提前声明出来，让用户在添加订阅（以及声明发生变化）时一次性确认，而不是每次安装弹窗：

```yaml
  - id: upkit-hub
    download_hosts:        # 宿主强制校验：插件给出的地址不在这里就拒绝下载
      - github.com
    plugin_hosts:          # 仅告知：插件进程自己会访问的域名，宿主拦不住也无法授权
      - api.github.com
      - ungoogled-software.github.io
```

声明**不手写**：它由各上游推导（`cmd/upkit-hub/catalog.go` 里每个 source 的 `hosts()`），
因为这条规则要拿去强制校验下载地址 —— 手写必然跟代码漂移，漏一个域名用户的下载就被拒。

```bash
go run ./cmd/upkit-hub -print-declarations   # --download-hosts=… / --plugin-hosts=…
make feed SCHEMA=2                          # 生成带声明的 schema 2 清单
```

⚠️ **发布顺序不能颠倒**：`download_hosts` / `plugin_hosts` 属于 schema 2，只有认识到
它们的宿主才认；旧宿主会以 `field download_hosts not found` 整份拒绝。而官方源走
`releases/latest`，一发就全量生效。所以顺序是：

1. 宿主把 `SchemaVersion` 提到 2 并发布；
2. 等用户升级到位；
3. 再把本仓库的默认 schema 改成 2（`build-release.yml` 的 `schema` 默认值、`make feed SCHEMA=2`）。

在此之前，脚本与 CI 默认都产 schema 1（不含声明），已发布清单的行为不变。

## 内置订阅地址

```
https://github.com/dezhishen/upkit-hub/releases/latest/download/feed.yaml
```

这个字符串已经编进 upkit 的二进制（`internal/pluginfeed/builtin.go`），改不了，于是就
有了本仓库的几条铁律：

| 铁律 | 原因 |
| --- | --- |
| `feed.yaml` 必须是 Release 附件，**文件名精确为 `feed.yaml`** | 上门的地址直指这个附件名，错一个字符就是全网 404，且没有任何报错 |
| 每更新一次清单就要发一个 Release | 附件属于某一次 Release，没有别的挂载点 |
| 产物地址写成**相对形式** `./<插件ID>-windows-<arch>.exe` | 由宿主相对**订阅地址**解析：内置地址取的是同一次发布里的 `feed.yaml`，于是产物地址也落在同一次发布上 —— 同源（用户不必额外确认「下载域名」），且不必把 tag 写进清单。**必须带 `./`**：以 `/` 开头是「相对 origin 根」，会变成 `https://github.com/<文件>`，那是 404 |
| 每个插件必须覆盖 `windows/amd64` 与 `windows/arm64` | 漏一个架构，那个架构的用户会在「校验订阅」这一步失败，而这本可以在发布前发现 |
| sha256 / size 由脚本构建后现算 | 手工填摘要迟早会错，而错的摘要等于装不上 |
| 清单 schema 的升版必须等宿主支持 | 官方源走 `releases/latest`，一发就全量生效；旧宿主在严格模式下遇到不认识的字段会**整份拒绍解析** |
| 预览版用 `-rc.N` / `-beta.N` 后缀 | `releases/latest` 不含 prerelease，所以发预览版不会改变用户读到的清单 |

## 目录

```
├── cmd/upkit-hub/          官方插件（catalog 模式：一条订阅分发多个软件）
│   ├── catalog.go          软件清单：加一个软件 = 加一条 appSpec
│   ├── source_github.go    上游之一：GitHub Releases
│   └── source_ucbinaries.go 上游之二：ungoogled-chromium 官方二进制索引站
├── cmd/feedcheck/          发布前门禁：校验清单（可选核对产物与下载地址）
├── internal/feedcheck/     订阅的解析与校验，以及针对 gen-feed.sh 的测试
├── internal/ucbinaries/    ungoogled-chromium 索引站的 HTML 解析（带真实页面夹具）
├── scripts/
│   ├── build-plugin.sh     插件构建（交叉编译、注入版本号、发布命名）
│   ├── gen-feed.sh         清单生成器（扫描产物、现算摘要、检查平台覆盖）
│   ├── serve-feed.sh       本地 https 静态服务（自签证书，免 root）
│   └── check-headers.sh    文件头检查（package 唯一、//go:build 在第 1 行）
└── .github/workflows/      CI、构建、发布
```

`internal/feedcheck` 是宿主 `internal/pluginfeed` 的**最小复刻**，不是第二份权威：
宿主那个包在另一个 module 里、Go 不允许引入，所以这里保留一份够用的校验，作用是把
「宿主一定会拒绝的清单」拦在发布之前。它的两条自缚规则写在包注释里 —— 只允许比宿主
更严或一致，不发明宿主不认识的字段。

## 订阅里的软件

| id | 上游 | 取哪个产物 |
| --- | --- | --- |
| `fzf` | `junegunn/fzf` 的 Releases | `windows_<arch>.zip`（便携版） |
| `ungoogled-chromium` | [官方二进制索引站](https://ungoogled-software.github.io/ungoogled-chromium-binaries/) | `windows_<arch>.zip`（便携版） |
| `7zip` | `ip7z/7zip` 的 Releases | `7z*-<arch>.exe`（官方安装器，静默安装） |

`ungoogled-chromium` 的二进制由社区贡献者提交、**不是官方构建**，也无法保证可复现 ——
索引站首页自己就这么写。这段说明会作为版本备注出现在界面上，并且每个文件都带上站点给的
SHA256。

## 增加一个软件

一个软件 = `cmd/upkit-hub/catalog.go` 里的一条 `appSpec`：`src` 说明版本与产物从哪个
上游来，其余字段交给宿主的四条轴。加完软件 → 构建 → 生成清单 → 发版，用户重新拉取订阅
就能看到它。

上游目前有两种形态：

| `src` | 怎么拿版本与产物 |
| --- | --- |
| `githubReleases{repo, asset}` | 查 GitHub Releases，按 `asset` 通配挑包；`{arch}` 会按本机架构展开 |
| `ucBinaries{platformDirs}` | 读索引站的平台索引页与版本页，挑出便携 zip 及其 SHA256 |

三条容易踩的坑：

- **产物必须按本机架构挑**。宿主只用 `Artifacts[0]`，挑错了没有第二次机会。插件是按架构
  分别构建的（订阅里同时有 windows/amd64 与 windows/arm64 两份），所以用 `runtime.GOARCH`；
  上游对架构的叫法不一致时（7-Zip 把 amd64 叫 `x64`）在 `appSpec.arch` 里给映射。
- **优先便携版而不是安装器**。upkit 自己负责解包、备份与回滚，安装器会绕开这套机制。
- **尽量带上摘要**。索引站给的 SHA256 会作为 `Artifacts[].Digest` 交给宿主；
  `PickPortable` 因此宁可不提供某个版本，也不给一个无法校验的下载。

`cmd/upkit-hub/catalog_test.go` 里有几条约束性用例：id 必须合法且唯一、必须声明入口与
安装路径、`{arch}` 必须按上游命名展开。加软件时它们会替你挡住最低级的错误。

想确认真实上游当下是否接得通（默认跳过，CI 不联网）：

```bash
UPKIT_HUB_LIVE=1 go test ./internal/ucbinaries/ ./cmd/upkit-hub/ -run Live -v
```

## 本地开发

```bash
make check                 # 格式化 + 文件头 + vet + 测试
make release-local         # 构建两个架构的插件 → 生成 dist/release/feed.yaml → 校验
```

清单里的产物地址默认是相对的（`./<文件>`）：它由宿主相对订阅地址解析，所以同一份
`feed.yaml` 在本地自签服务、在 GitHub Release 上都成立。要给绝对地址（自建分发、
内网镜像）时加 `BASE_URL`：

```bash
make feed BASE_URL=https://cdn.example.com/upkit-hub
```

产物都落在 `dist/`：

```
dist/plugins/upkit-hub-windows-amd64.exe     构建原始输出
dist/plugins/upkit-hub-windows-arm64.exe
dist/release/feed.yaml                       清单（发布的就是这个文件）
dist/release/upkit-hub-windows-*.exe         与清单同一次构建的产物
dist/serve/ca.crt                            本地自签证书
```

### 在 upkit 里装一遍

宿主的订阅地址解析**拒绝明文 http**（明文链路中间任何人都能整份替换清单，而清单自带的
sha256 保护不了清单自己），所以本地服务也必须是 https。自签证书不需要 root —— upkit
是 Go 程序，认 `SSL_CERT_FILE`：

```bash
# 1) 本仓库：起本地 https 服务（前台运行，Ctrl-C 结束）
make release-local
make serve

# 2) 另一个终端：先用 curl 确认拉得到
curl --cacert dist/serve/ca.crt https://127.0.0.1:8443/feed.yaml

# 3) upkit 仓库：构建开发版，并把自签证书交给它
cd /path/to/upkit && make build-dev
SSL_CERT_FILE=/path/to/upkit-hub/dist/serve/ca.crt ./dist/upkit-dev
```

然后在 upkit 的「来源」面板按 <kbd>a</kbd> 填入 `https://127.0.0.1:8443/feed.yaml`，
确认信任 `127.0.0.1`，进入详情按 <kbd>enter</kbd> 安装。

> 插件产物是 Windows 可执行文件，在 Linux/macOS 上**装得上但起不来**：下载、摘要校验、
> 落盘 `plugin/upkit-hub.exe` 与 `plugin/upkit-hub.plugin.yaml` 都能完成，真正运行插件
> 得在 Windows 上。

不想开界面时，也可以只用仓库内的门禁核对（含逐个下载产物核对摘要）：

```bash
make verify                # 校验 dist/release，并下载清单里的每个包比对摘要
```

## 发布

打 tag 即发布，走 `.github/workflows/release.yml`：

```bash
git tag v0.1.0 && git push origin v0.1.0     # 正式版：会更新 releases/latest
git tag v0.2.0-rc.1 && git push origin v0.2.0-rc.1   # 预览版：不影响用户读到的清单
```

流水线做四件事：

1. **verify**：gofmt / 文件头 / vet / 测试（与 CI 同一套门禁）；
2. **build**：交叉编译两个架构 → 生成 `feed.yaml` → `feedcheck` 校验清单与产物逐字节一致
   → 确认产物是 PE 文件、版本号确实注入了（`-ldflags -X` 写错符号路径不会报错，
   只会静默保留 `dev`）；
3. **publish**：上传 `feed.yaml` 与两个 exe，随后用 API 复核附件真的在、且正式版确实是
   `releases/latest` 指向的那个（`gh` 有可能什么都没做却退出 0）；
4. 全程与 `ci.yml` 的发布演练共用同一份定义 —— `main` 上是绿的，就意味着现在能发版。

清单格式、插件开发、信任模型的完整说明见 upkit 的
[`docs/plugin-subscription.md`](https://github.com/dezhishen/upkit/blob/main/docs/plugin-subscription.md)
与 [`docs/plugin-dev.md`](https://github.com/dezhishen/upkit/blob/main/docs/plugin-dev.md)。

## 许可证

MIT
