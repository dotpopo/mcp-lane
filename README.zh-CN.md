<div align="center">

<img src="assets/icon.png" width="96" alt="mcp-lane">

# MCP Lane (mcp-lane)

**让浏览器里的 ChatGPT、Claude、Grok 和 Gemini 读写你本机或 VPS 上的项目。每一次改动和命令，都先经过你的批准。**

[English](README.md) | 简体中文

[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.25-00ADD8)](go.mod)
[![MCP](https://img.shields.io/badge/protocol-MCP-6E56CF)](https://modelcontextprotocol.io)

</div>

<br>

## 这是什么

浏览器里的 AI 看不到你电脑上的项目。想改一个文件，你得把代码粘过去、再把答案粘回来。

mcp-lane 跑在你的电脑上。选一个目录，浏览器里的 AI 就能连上它：读文件、改代码、跑测试。

AI 只能发请求。真正的改动和命令由 mcp-lane 在你的机器上执行，并先显示在它的窗口里。未经你批准，什么都不会发生。

mcp-lane 是 [leazoot/fylane](https://github.com/leazoot/fylane) 的 fork。
协议、二进制名（`fylane-companion`、`fylane-relay`）和 Apache-2.0 许可都不变，
新增的是无头模式、安全加固和改造后的审批 lane——见
[本 fork 的新增内容](#本-fork-的新增内容)。

![等待批准的一次写入](assets/approval.png)

它能做什么：

- 问 ChatGPT“这个报错是从哪里抛出来的”，让它直接读项目。不用粘贴。
- 让 Claude 改三个文件，在 mcp-lane 里检查改动，再批准。
- 让 Grok 跑 `npm test`，把结果读回去。
- VPS 上的项目也一样，批准还是在这台电脑上。见[远程机器](#远程机器)。
- 明天开个新对话，接着昨天的地方继续。见[记忆](#记忆)。
- 合上笔记本，AI 就再也够不到它了。

连接方式：

```
浏览器里的 AI  ──►  公网地址（隧道或中继）  ──►  你电脑上的 mcp-lane  ──►  你选的目录
                                                                    │
                                                              在这里批准
```

只有你电脑上的 mcp-lane 碰你的文件。中间的公网地址只转发消息，不存任何东西。

项目在 VPS 上时，中间多一跳 ssh：

```
浏览器里的 AI  ──►  公网地址  ──►  你电脑上的 mcp-lane  ──ssh──►  VPS 上的 mcp-lane  ──►  服务器上的目录
                                              │
                                        批准还是在这里
```

VPS 上的 mcp-lane 不暴露到公网，只有你的电脑能通过 ssh 连到它。AI 平台那侧没有任何变化：连的还是同一个地址。

## 本 fork 的新增内容

### 无头 CLI：不需要窗口

服务器或自动化脚本可以在终端里走完整个流程——注册目录、配对设备、列出并处理审批——走的和桌面窗口是同一条审批路径，不存在第二套语义：

```sh
fylane-companion init --workspace ~/projects/my-app --non-interactive
fylane-companion pair -relay https://relay.example.com --non-interactive --ttl 10m
fylane-companion serve -data-dir ~/.fylane-data &
fylane-companion approvals --json
fylane-companion approve --json <change-set-id>
fylane-companion reject --json <change-set-id>
```

- `init` 注册目录（幂等：重复跑只做校验，退出码仍是 0）。
  `--approval-mode safe|balanced` 只在全新数据目录时写入策略。
- `pair` 注册设备，打印配对码，并在末尾加一行机器可读的
  `PAIR_CODE=... EXPIRES_IN=... CONNECTOR_URL=...`。
  `-relay` 必填；`--ttl` 只是声明想要的码有效期，最终以 relay 自己的 TTL 为准。
- `approvals` 列出待处理的；`approve`/`reject` 接受一个 `<change-set-id>`，
  经由桌面窗口用的同一个 Resolve 生效。
- `--json` 约定：成功时退出 0，stdout 上恰好一个 JSON 文档；
  人看的解释文字一律走 stderr。不加 `--json` 时，人看的文字走 stdout。
- 退出码：`0` 成功 · `1` 运行失败（relay 拒绝、daemon 没跑……） ·
  `2` 用法错误（flag 写错、参数个数不对……） ·
  `3` 目标不存在（id 未知或已经处理过）。

完整序列（含本地 relay、环境变量、无 keychain 时的风险说明）：
[docs/headless.zh.md](docs/headless.zh.md)。

### 安全加固

- **访问 token 默认 30 分钟过期**（上游原来是 2 小时），收窄被盗 token
  的重放窗口。relay 运维可用 `fylane-relay serve -access-ttl` 调整
  （下限 5 分钟）；从不刷新的平台在过期后会看到 `401` 直到重连，
  会刷新的平台由轮换的 refresh token 兜底。
- **Caddy 访问日志脱敏。** 在已有的 query 脱敏和 `Authorization` 删除之外，
  走 path 的传统 capability token（`/mcp/<token>`）也被脱敏——日志里只留
  “这一段存在”，不留它的值。见 [`deploy/Caddyfile`](deploy/Caddyfile)。
- **隧道并发上限。** companion 同时最多服务 32 个隧道请求；超限的请求立刻
  以可重试的 503 拒绝，而不会在“可能等人工批准的 handler”后面无限排队。

### lane 体验

- **每张审批卡都有风险条**：每个请求按低、中、高分级，并用一句话说清原因，
  先看再决定。
- **审批队列**：多个请求等待时，卡片显示当前排位（`2 of 5`），可上一个 /
  下一个逐步看，也可一键全部批准。
- **快捷键**：`a` 批准、`d` 拒绝、`e` 展开详情。只在卡片可见时生效——
  输入框里、按着修饰键时、有弹窗盖住时都不会触发。

## 安装

从 [Releases](https://github.com/dotpopo/mcp-lane/releases/latest) 下载：

| 系统 | 下载 |
| --- | --- |
| macOS | `fylane-desktop-macos.dmg`，拖进 Applications |
| Windows | `fylane-desktop-windows-amd64.zip`，解压运行 `Fylane.exe` |
| Linux / 服务器 | 目前只有命令行，见[命令行](#命令行) |

二进制名和上游保持一致（`fylane-companion`、`fylane-relay`、
`fylane-desktop-*`），给 Fylane 写的脚本照常用。

安装包目前没有签名。macOS 首次打开会说无法验证开发者：去“系统设置 →
隐私与安全性”点“仍要打开”。Windows 出现 SmartScreen 时点“更多信息 →
仍要运行”。想先验货，看 release 页面的 `SHA256SUMS`。

## 第一次运行

两条路二选一，终点一样。

### A. 在窗口里（不需要终端）

mcp-lane 在窗口里带你走完四步。

![第一步：选目录](assets/first-run.png)

1. **选一个目录。** AI 只能看到这个目录，它的上层看不到。随时可以换，
   也可以收回。
2. **试一次写入。** mcp-lane 往目录里写一个示例文件，让你看看批准长什么样。
3. **决定什么要问。** 默认跑命令时每个目录问一次，之后普通命令直接跑；
   写文件和危险命令还是要问。以后可以在设置里改。
4. **接一个 AI。** 这一步发生在 AI 平台上，见下一节。

然后就到了主界面。左边是 lane，等你处理的请求出现在这里；右边是当前
目录和已连接的 AI。

![lane](assets/lane.png)

### B. 无头模式（只要终端）

给没有桌面的机器用，比如 ssh 连上去的 VPS：

```sh
# 0. 选好私有目录。serve 只绑 loopback。
export FYLANE_DATA_DIR=~/.mcp-lane/data
WS=~/projects/my-app
mkdir -p "$WS"

# 1. 注册目录（幂等：重复跑只做校验，退出 0）。
fylane-companion init --workspace "$WS" --non-interactive

# 2. 和 relay 配对（打印配对码给平台连接页用，外加一行 PAIR_CODE=... 给脚本）。
fylane-companion pair -relay https://relay.example.com \
  -data-dir "$FYLANE_DATA_DIR" --non-interactive --ttl 10m

# 3. serve 跑起来（只绑 loopback；放后台别停）。
fylane-companion serve -data-dir "$FYLANE_DATA_DIR" &

# 4. 列出待审批（第一个写入到来之前是空的）。
fylane-companion approvals --json

# 5. 处理一个（和桌面窗口用的同一个 Resolve）。
fylane-companion approve --json <change-set-id>
# ……或者拒绝：
fylane-companion reject --json <change-set-id>
```

提醒：没有 OS keychain 的服务器上，`pair` 和 `serve` 需要
`FYLANE_DEVICE_CREDENTIALS_FILE` 指向一个仅属主可读（0600）的 JSON 文件，
里面的设备密钥是**明文存放**。有条件的话优先用真 keychain
（比如 headless 下也跑一个 GNOME Keyring）。细节和完整可验证示例：
[docs/headless.zh.md](docs/headless.zh.md)。

## 把 mcp-lane 接到 AI 平台

每个平台都是三步：从 mcp-lane 复制地址，填到平台的连接器设置里，
再回 mcp-lane 批准这次连接。

### 第一步：在 mcp-lane 里拿到地址

打开**设置 → 连接**。

![连接](assets/connection.png)

第一次选 **Cloudflare 快速隧道**，点设置。不需要账号和域名。几秒钟后出现
`https://xxx.trycloudflare.com/mcp` 这样的地址，点复制。

这个地址每次重启 mcp-lane 都会变，所以平台那边要重新填一次。想要固定
不变的地址，见[固定地址](#固定地址)。

### 第二步：填到平台里

<details open>
<summary><b>ChatGPT</b></summary>

需要付费套餐（Plus、Pro 或 Team）。

1. 头像 → **设置 → 安全与登录** → 打开**开发者模式**。
   老版本在 Apps & Connectors → Advanced 下面。

   ![开发者模式](assets/setup/chatgpt-developer-mode.png)

2. 去 **Plugins**（老版本叫 Apps & Connectors），点 **Create**，填：
   - Name：随便写，比如 `mcp-lane`
   - Connection：保持 **Server URL**，把地址粘进去
   - Authentication：选 **OAuth**。“No authentication”会报
     `Error creating connector`。
   - 勾上“I understand and want to continue”。

   ![新建 Plugin 表单](assets/setup/chatgpt-create-connector.png)

3. 点 Create，会打开一个浏览器页面，见第三步。
4. 在对话里点输入框旁的 **+** → **More**，勾上 `mcp-lane`。

</details>

<details>
<summary><b>Claude</b></summary>

1. 左下角头像 → **设置 → Connectors** → **Add custom connector**。
2. 名字写 `mcp-lane`，URL 粘地址。**Advanced 下面的 Client ID 和
   Client Secret 留空。** 填了会报错。
3. 点 **Continue**，再在列表里 mcp-lane 旁边点 **Connect**，会打开授权页，
   见第三步。
4. 在对话里打开输入框上的 **tools** 按钮，确认 mcp-lane 是开着的。

![添加自定义连接器](assets/setup/claude-add-connector.png)

Claude 里每个工具可以设“每次都问”或“总是允许”，那只是 Claude 自己的设置。
mcp-lane 该问的还是会问。

</details>

<details>
<summary><b>Grok</b></summary>

1. grok.com → **设置 → Connectors** → **New Connector** → **Custom
   Connector**。
2. 名字写 `mcp-lane`，Server URL 粘地址，点 **Add Connector**。
3. 点 connect，会打开授权页，见第三步。

![自定义连接器](assets/setup/grok-add-connector.png)

Grok 有几点不一样：

- **Grok 自己不确认写入。** 用 Grok 时，mcp-lane 里的写批准保持打开。
- Grok 有自己的云沙箱，“跑一下测试”可能跑在它那边。说清楚：
  “用 mcp-lane 的 run_command 跑测试”。
- Grok 每次调用只等 60 秒。到时你还没批，它会被告知请求 pending。
  批完让它再试一次。

</details>

<details>
<summary><b>Gemini</b></summary>

需要 Google AI Pro 或 Ultra，以及 Gemini Spark。Spark 在 EEA、英国、
瑞士、尼日利亚不可用。

1. gemini.google.com → 切到 **Spark** → **Connected Apps** → **Custom apps**
   下面点 **Add a custom app**。
2. 把地址粘进去。**Advanced features** 下面的字段留空：
   Gemini 会自己向 mcp-lane 注册。
3. 点 **Next**，会打开授权页，见第三步。

![添加自定义应用](assets/setup/gemini-add-custom-app.png)

Gemini 有几点不一样：

- Connected Apps 需要先打开 **Gemini Activity**。页面说加不了应用时，
  先去把它打开。
- 自定义应用只能在网页端添加。加上之后，手机 App 里也能用。
- 自定义应用只在 Spark 任务里生效，普通对话里用不了。

</details>

### 第三步：在 mcp-lane 里批准这次连接

平台打开一个授权页，上面有一串短码。mcp-lane 窗口里显示同样的码，
对一下，点**批准连接**。

![批准连接](assets/pairing.png)

如果你用的浏览器在另一台电脑上，页面会让你输配对码。在 mcp-lane 里去
**设置 → 连接**，点**显示配对码**，输进去。一个码有效 10 分钟，一次有效。

### 试一下

回对话里输入：

> 列出这个项目根目录的文件。

AI 会列出目录。读不需要批准。再试：

> 在项目里创建 hello.txt，内容是“hello”。

mcp-lane 窗口亮起，显示 AI 想写什么。批准，文件出现；拒绝，AI 会被告知不行。

### AI 拿到的工具

| | 工具 |
| --- | --- |
| 读 | `list_directory` `read_file` `search_files` `git_query` |
| 写 | `write_file` `edit_file` `apply_patch` `change_manage` |
| 跑 | `run_command` `task_status` `code_task` |
| 记 | `memory`，含 `recall` `note` `plan` `step` `search` `read` `compact` |
| 找 | `code_navigate`，找定义和引用 |
| 转 | `mcp_gateway`，把调用转给本机另一个 MCP server |

需要批准的写和命令在你决定之前都返回 `pending_approval`。
`change_manage` 管移动、删除和撤销。

### 固定地址

Cloudflare 快速隧道每次重启地址都变。想要固定的，去**设置 → 连接**换一种：

| 方式 | 需要 | 地址 |
| --- | --- | --- |
| Cloudflare 快速隧道 | 不需要 | 重启会变 |
| Tailscale Funnel | 装 Tailscale 并登录一次（免费） | 固定，`xxx.ts.net` |
| Cloudflare 命名隧道 | 一个托管在 Cloudflare 的域名 | 固定，你自己的域名 |
| ngrok | 一个 ngrok 账号 | 免费版会变，付费版固定 |
| 自己的 relay | 一台有公网域名的服务器 | 固定，你自己的域名 |

前四种 mcp-lane 帮你启动和管理，在窗口里点设置就行。

自己的 relay 适合团队，或者几台电脑共用一个地址。它跑在你的服务器上，
不存文件内容。见 [`deploy/`](deploy/)：

```bash
FYLANE_RELAY_HOST=relay.example.com docker compose -f deploy/docker-compose.yml up -d
```

## 远程机器

项目在 VPS 上，你在 Mac 前面，想让 AI 改那边的代码、跑那边的测试。
把那台机器加进 mcp-lane。AI 平台那侧没有任何变化。

**开始之前**：本机终端里 `ssh user@host` 已经能免密登录。mcp-lane 用的是
你系统的 ssh，密钥和 `~/.ssh/config` 照常用。它从不问你要密码。

1. 在 lane 的 **Machine** 下面点 **Switch machine → Add a remote
   machine…**，输入 `ssh` 后面会跟的那串，比如别名或 `user@host`。
2. 那台机器上还没有 mcp-lane 的话，在侧栏点 **Install mcp-lane**，
   装的是和本 App 同一版本。
3. 显示 **Connected** 后，在 Workspace 下面点 **Choose a folder**，
   选那台机器上的目录，或直接输路径如 `~/project`。

之后就和本机目录一样用。读、写、跑命令都在 VPS 上发生，批准在你的 Mac
上。Tasks 页里，远程机器的记录会标机器名。

几点说明：

- **VPS 上的 mcp-lane 不暴露到公网。** AI 经由你的电脑够到它，所以这台
  电脑关机时，那台 VPS 也连不上。
- **只在这台电脑上批准。** 远程机器上的命令默认每次都问。
- **切换机器只切换 lane 显示哪台。** Tasks 页还是显示所有机器，
  待处理的请求永远不会被藏起来。
- 远程机器的数据放在它那边的 `~/.fylane/`。**Remove** 只是让本机忘掉这台
  机器，那边什么都不删。
- 远程设置目前还不能在窗口里改。Windows App 需要系统自带的 OpenSSH 客户端。

VPS 上完全不要窗口？走无头流程：[第一次运行 → B](#b-无头模式只要终端)。

## 记忆

开个新对话，接着上次的地方继续。

每个目录，mcp-lane 记住两件事：

- **现状**：做完什么、下一步什么、定了什么、还开着什么。
- **经过**：做过的工作和重要的决定。

新对话里不用再讲一遍背景，直接说“接着上次继续”。

- 它没接上，说“先查一下这个目录的记忆”。
- 做完一段工作或定了重要的事，说“记下来”。

记忆不会无限膨胀。现状保持更新，旧笔记可以折成摘要，原件还在，
要用时找得到。

在 Memory 页可以查看、编辑、删除、导出或清空。所有记忆都存在 mcp-lane
里，从不写进你的项目文件。

## 命令行

在没有桌面 App 的机器上（比如 Linux 服务器），用 `fylane-companion`。
干的是同一份活，审批在终端里：按 `y` 批准，其他键拒绝。删整个目录要
输入 `yes`。

安装（macOS 和 Linux）：

```bash
curl -fsSL https://raw.githubusercontent.com/dotpopo/mcp-lane/main/scripts/install.sh | sh
```

然后进项目目录：

```bash
cd ~/projects/my-app
fylane-companion share
```

它会打印地址和配对码：

```
sharing my-app — starting a tunnel, this takes a few seconds

  Connector URL   https://swift-lane-9f2c.trycloudflare.com/mcp
  Pairing code    7K4M-2QB9   (valid for 10m0s)
```

后面就和桌面 App 一样：把地址填到平台，授权页输配对码。Ctrl-C 停。
在 ssh 连的服务器上，用 `tmux` 或 `screen` 包一层再跑，断开不断跑。

用自己的 relay：

```bash
fylane-companion pair -relay wss://relay.example.com/tunnel -register
fylane-companion serve -workspace ~/projects/my-app
```

脚本化 setup 和无人值守审批（`init` / `approvals` / `approve` /
`reject`、`--json`、退出码），见[本 fork 的新增内容](#无头-cli不需要窗口)和
[docs/headless.zh.md](docs/headless.zh.md)。

从源码构建需要 Go 1.25：

```bash
git clone https://github.com/dotpopo/mcp-lane
cd mcp-lane
go build -o bin/fylane-companion ./companion/cmd/companion
```

## 安全

只有这台机器上你的批准算数。AI 平台上的确认只是参考。
relay 从不存文件内容、改动、目录列表和敏感文件名。

完整信任模型、覆盖范围和漏洞上报方式：[SECURITY.md](SECURITY.md)。
已知限制，先说清楚：

- **设备密钥回退文件不加密。** 没有 OS keychain 的服务器上，
  `FYLANE_DEVICE_CREDENTIALS_FILE` 是明文存放设备密钥的。保持 0600，
  别备份到共享存储，有条件优先用真 keychain。
- **Caddy 脱敏自己验一遍。** 随仓库发的
  [`deploy/Caddyfile`](deploy/Caddyfile) 脱了 query、`Authorization` 头
  和 `/mcp/<token>` path 段，但 CI 不检查你部署的 Caddy 是否真生效——
  上线后读一遍自己的访问日志。
- **relay 侧背压还没做。** companion 超过 32 个并发隧道请求会回 503，
  但 relay 自己对每个等结果的调用最多干等 15 分钟且不限流，
  调用方卡住多了会堆 relay 的 handler。

## 许可证

[Apache 2.0](LICENSE)。mcp-lane 是
[leazoot/fylane](https://github.com/leazoot/fylane) 的 fork，同样 Apache-2.0——
原仓库的许可和署名原样保留在本仓库。
