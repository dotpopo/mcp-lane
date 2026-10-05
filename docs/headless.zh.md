# 无头配置与审批（无需图形界面）

[English version](headless.md)

本页面向需要在无桌面窗口的机器上拉起 Companion 的 agent 与脚本：
注册文件夹、绑定设备、批准第一次写操作——全部经由 CLI 完成，
全部可在本机直接验证。

完整流程（每一步都会打印下一步所需的信息）：

```sh
# 0. 选好私有目录。serve 只绑 loopback。
export FYLANE_DATA_DIR=/tmp/fylane-headless-test/data
WS=/tmp/fylane-headless-test/ws
mkdir -p "$WS"

# 1. 注册文件夹（幂等：重复运行只做校验，exit 0）。
fylane-companion init --workspace "$WS" --non-interactive

# 2. 与 relay 配对（要求 relay 可达；在平台连接页输入配对码，
#    同时为脚本打印一行 PAIR_CODE=...）。
fylane-companion pair -relay https://relay.example \
  -data-dir "$FYLANE_DATA_DIR" --non-interactive --ttl 10m

# 3. 启动服务（只绑 loopback；放后台运行）。
fylane-companion serve -data-dir "$FYLANE_DATA_DIR" &
SERVE_PID=$!

# 4. 列出待批请求（第一次写操作到来之前为空）。
fylane-companion approvals --json

# 5. 批准一条（与桌面窗口走同一个 Resolve）。
fylane-companion approve --json <change-set-id>
# ……或拒绝：
fylane-companion reject --json <change-set-id>

kill $SERVE_PID
```

`--json` 约定（所有新增命令）：成功时 exit 0，stdout 上恰好一个
JSON 文档；人类可读说明走 stderr。不加 `--json` 时人类可读行走
stdout，`pair` 会在末尾追加一行机器可解析的行：

```
PAIR_CODE=ABCD-1234 EXPIRES_IN=10m0s CONNECTOR_URL=https://relay.example/mcp
```

密钥（配对码、token）只出现在上述两个地方——绝不进日志。
`PAIR_CODE=` 行携带配对码是因为脚本必须把它展示给用户；
流转过程中请按密码对待。

## 环境变量

| 变量 | 使用者 | 含义 |
|---|---|---|
| `FYLANE_DATA_DIR` | 所有子命令 | 数据目录覆写（与桌面壳约定一致）。`-data-dir` 优先级更高。 |
| `FYLANE_TUNNEL_TOKEN` | `serve`（legacy 共享 token 模式） | 隧道凭证；优先于设备凭证，从不落盘。 |
| `FYLANE_TUNNEL_PROVIDER_TOKEN` | 隧道 provider | Fylane 代起隧道所需的 provider 凭证。存 OS keychain，不进配置文件。 |
| `FYLANE_DEVICE_CREDENTIALS_FILE` | `pair`、`serve` | **显式 opt-in 回退**：没有 OS keychain 时的设备凭证存放处（无头服务器常见）。JSON 对象，`relay:<host>` 映射到 `{"device_id": ..., "device_secret": ...}`。文件必须仅属主可读写（0600），更宽权限会被拒绝。 |

## 没有 keychain 时（风险提示）

设备凭证默认只住 OS keychain，不进配置文件也不进数据库。
无头 Linux 服务器常常根本没有 keychain（无 D-Bus secret service）。
**此处没有任何静默回退**：不设置 `FYLANE_DEVICE_CREDENTIALS_FILE`，
`pair` 与 `serve` 会大声失败，且报错信息会点名这个变量。

设置该变量意味着设备密钥以**明文落盘**。它恰好在一个方面弱于
keychain：能读到该文件的人即拥有该设备身份（可申请配对码、可长期
占用隧道）。缓解措施，按优先级：

1. 首选真正的 keychain（无头环境也可跑 GNOME Keyring）。
2. 文件保持 0600，放在仅 root/该用户可读的磁盘上；不备份到共享
   存储，不打日志，不出现在命令行参数里。
3. 只有在理解其授权能力完全相同的前提下，才考虑 legacy 的
   `FYLANE_TUNNEL_TOKEN` 模式——它并不更弱，只是部件更少。

该回退文件的静态加密**尚未实现**（见下文遗留缺口）；在实现之前，
以上缓解措施就是全部保护。

## 退出码

| 码 | 含义 |
|---|---|
| `0` | 成功（`init` 在工作区已注册时同样返回 0）。 |
| `1` | 运行期失败：relay 拒绝、daemon 未运行（无 `control.json`）、relay/server 侧拒绝，…… |
| `2` | 用法错误：缺 `--workspace`、`-ttl` 非法、参数个数不对，…… |
| `3` | 目标不存在：`approve`/`reject` 的 id 未知或已被决议。 |

每个子命令都支持 `--help`（标准 flag 包，`-h` 亦可）。

## 端到端例子（全程 loopback）

整条链路可在一台机器上验证：起一个本地 relay、配对、serve、
把一次写操作走完审批、决议。

```sh
export FYLANE_DATA_DIR=/tmp/fylane-headless-test/data
export FYLANE_DEVICE_CREDENTIALS_FILE=/tmp/fylane-headless-test/creds.json
WS=/tmp/fylane-headless-test/ws
mkdir -p "$WS"
echo "v1" > "$WS/a.txt"

# 本地 relay（OAuth 模式，内存存储），只绑 loopback。
fylane-relay serve -addr 127.0.0.1:19000 -issuer http://127.0.0.1:19000 &
RELAY_PID=$!

fylane-companion init --workspace "$WS" --non-interactive
# workspace ready: ws (ws_...) at /tmp/fylane-headless-test/ws
# next: pair this device, then serve it: ...

fylane-companion pair -relay http://127.0.0.1:19000 \
  --non-interactive --ttl 10m --data-dir "$FYLANE_DATA_DIR"
# PAIR_CODE=... EXPIRES_IN=10m0s CONNECTOR_URL=http://127.0.0.1:19000/mcp

fylane-companion serve -addr 127.0.0.1:18787 \
  -relay ws://127.0.0.1:19000/tunnel -data-dir "$FYLANE_DATA_DIR" &
SERVE_PID=$!
sleep 2  # 等 control.json 落盘

fylane-companion approvals --json
# {"approvals":[]}

# 构造一次需要审批的写操作：经 control API 建 b.txt
# （阻塞 5 秒拿决议，拿不到则返回 pending_approval + change_set_id）。
TOKEN=$(python3 -c "import json;print(json.load(open('$FYLANE_DATA_DIR/control.json'))['token'])")
ADDR=$(python3 -c "import json;print(json.load(open('$FYLANE_DATA_DIR/control.json'))['addr'])")
curl -s -X POST http://$ADDR/v1/save \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"provider":"local","summary":"headless e2e","files":[{"path":"b.txt","content_base64":"aGVsbG8K"}]}'
# {"status":"pending_approval","change_set_id":"chg_...","pending":true,...}

fylane-companion approvals --json
# {"approvals":[{"change_set_id":"chg_...","kind":"write",...}]}

fylane-companion approve --json chg_...
# {"approved":true,"change_set_id":"chg_...","resolved":true}

# 用同一个 change_set_id 重试：已记录的决议会被重放，写操作落盘。
curl -s -X POST http://$ADDR/v1/save \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"provider":"local","summary":"headless e2e","change_set_id":"chg_...","files":[{"path":"b.txt","content_base64":"aGVsbG8K"}]}'
# {"status":"applied",...}

cat "$WS/b.txt"   # hello
fylane-companion approvals --json
# {"approvals":[]}

kill $SERVE_PID $RELAY_PID
```

几点说明：`init` 已选定当前工作区，故 `serve` 无需再传
`-workspace`；relay 的配对码 TTL 为 10 分钟并 capped `--ttl`
（flag 只表达意图，relay 做决定）；批准→重试正是平台侧的正常
循环（预算到期降级，再用同一 `change_set_id` 重试），不是测试
手法。

## 遗留缺口

- `FYLANE_DEVICE_CREDENTIALS_FILE` 为明文落盘；加密回退尚未实现，
  在此之前适用上文缓解措施。
- `pair` 仍要求 relay（`-relay` 必填）。无 relay 的机器用 direct
  模式的 `init` + `serve`，从 served surface 配对。
- `init --approval-mode` 只在数据目录全新时写入；之后改策略仍走
  Safety 页 / `POST /v1/safety`。
