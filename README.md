# LLM Proxy

第三方大模型 API 中转代理：**透明转发 + 自动重试 + 超阈值轮询下一个 API/Key**，带 Web 管理界面。

本地应用把请求发给本代理，代理负责重试和在多个 API/Key 间自动切换，结果原封不动带回。调用方无需改动任何代码。

## 特性

- **透明转发**：请求的 path/query/headers/body 与响应的 status/headers/body 逐字节原样透传，不改内容。
- **SSE 流式**：`stream=true` 场景逐块 flush，边收边转，不缓冲成整段。
- **智能重试**：仅对 429 / 5xx / 超时 / 连接错误重试（指数退避 + 抖动）；400/401/403 等业务错误直接透传、零重试。
- **自动轮询**：按**优先级**分层（数字越大越优先），同优先级层内轮询分摊流量，层间严格按高低顺序切换。
- **熔断降级**：某上游连续失败 3 次后临时降到队尾（15s 起翻倍，上限 5min；429 为 5s 起、上限 60s），一次成功立即恢复原优先级。调用方的 400/404 不计入。
- **请求总超时**：`total_timeout_sec`（默认 120s）限制单个请求含所有重试与切换的总耗时，超时返回 504。流式响应开始后不受限制。
- **多协议**：每个上游配置 `openai` 或 `anthropic` 协议；按路径路由（`/v1/chat/completions`→OpenAI，`/v1/messages`→Anthropic），两种协议的 key 互不复用。
- **调用洞察**：从响应里旁路解析每次调用的输入/输出 token、真实模型（`model=auto` 时上游实际选用的模型）、提供方，并记录客户端 IP 与 User-Agent。解析不影响透传；上游没返回的字段留空。
- **面板鉴权**：管理面板需账号密码登录（JWT）。初始账号 `admin` / `ChangeMe123!`，登录后可在面板改密。
- **客户端 Key 管理**：给调用代理的应用发凭据，每个 key 可设**到期时间**和 **token 总额度**；代理校验 key 合法（存在/启用/未过期/未超额）才转发。若一个 key 都不配置，代理开放（任何人可调）。
- **按 Key 统计**：日志和统计按发起调用的客户端 key 归属，显示每个 key 用了多少 token。
- **亮色/暗色主题**：面板默认亮色，右上角可切换，偏好持久化。
- **Web 面板**：配置管理、客户端 Key、监控日志、统计报表四个页面；刷新停留当前页。统计支持当天（默认）/ 近 3/7/15/30 天 / 自定义时间范围，含 token 汇总。

## 构建

需要 Go 1.26+。

```powershell
go build -o llmproxy.exe ./cmd
```

## 运行

```powershell
.\llmproxy.exe                    # 使用默认 config.json（首次运行自动生成）
.\llmproxy.exe -config my.json    # 指定配置文件
```

启动后监听两个端口：

| 端口 | 用途 |
|---|---|
| `:18080` | **代理端口**（绑 `0.0.0.0`）—— 应用把 SDK 的 base URL 指向这里 |
| `127.0.0.1:18081` | **管理面板**（默认仅本机）—— 浏览器打开 http://localhost:18081，初始账号 `admin` / `ChangeMe123!` |

端口可在配置文件里改（`proxy_addr` / `admin_addr`）。**公网部署时面板务必保持绑 `127.0.0.1`**，通过 nginx 反代访问（见下方部署章节）。

## 现网部署步骤（从零到上线）

假设：一台 Linux VPS，已装宝塔面板，有一个域名已解析到这台机器。按顺序做：

**1. 交叉编译 Linux 可执行文件**（在你本机，不是在 VPS 上）

```powershell
$env:GOOS="linux"; $env:GOARCH="amd64"
go build -o llmproxy ./cmd
Remove-Item Env:GOOS,Env:GOARCH
```

**2. 上传到 VPS**，建议单独一个目录，比如 `/www/wwwroot/llmproxy/`：

```
llmproxy/
├── llmproxy          # 上一步编译出的二进制
```

只需要传这一个文件——前端页面已经通过 `go:embed` 打进二进制里了，不用单独传 `web/static`。`config.json`、`stats.json` 会在首次启动时自动在这个目录下生成，不用你手动创建。

**3. 首次手动启动，确认能跑起来**

```bash
cd /www/wwwroot/llmproxy
chmod +x llmproxy
./llmproxy
```

看到两行日志说明正常：

```
admin dashboard listening addr=127.0.0.1:18081
proxy listening addr=:18080
```

`Ctrl+C` 停掉，进入下一步。

**4. 配上游、改初始密码、签发客户端 Key**（此时先不对外开放，只在服务器本机验证）

```bash
curl -s -X POST http://127.0.0.1:18081/api/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"ChangeMe123!"}'
```

拿到 token 后，用它调 `/api/upstreams/add` 把真实的上游 API/key 加进去（也可以先跳过，等 nginx 配好后直接在浏览器面板里操作，更省事）。**不管走哪种方式，都要在浏览器登录后立即改密码**——初始密码 `ChangeMe123!` 是写在源码里的，任何拿到这份代码的人都知道。

> ⚠️ **公网部署前必须签发至少一个客户端 Key。** 代理鉴权是"配了 Key 才校验"：只要「客户端 Key」列表是空的，代理对**任何人**开放，不需要任何凭证就能调用、消耗你配置的上游额度。本机验证阶段没配 Key 也能正常测试（此时是开放模式），但**在第 6 步开放公网端口之前，务必在「客户端 Key」页面签发至少一个 Key**，否则任何知道你域名的人都能白嫖你的上游额度。

**5. 配置 systemd，让它常驻 + 随机器重启自动起来**

新建 `/etc/systemd/system/llmproxy.service`：

```ini
[Unit]
Description=LLM Proxy
After=network.target

[Service]
Type=simple
WorkingDirectory=/www/wwwroot/llmproxy
ExecStart=/www/wwwroot/llmproxy/llmproxy
Restart=on-failure
RestartSec=3
User=www          # 用一个非 root 的普通用户跑，别用 root
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now llmproxy
systemctl status llmproxy      # 确认 active (running)
journalctl -u llmproxy -f      # 看实时日志
```

**6. 开放服务器防火墙 / 安全组端口**

只放 `18080`（代理端口，供 nginx 反代到它 + 供你的应用直连也行）。**`18081` 不要开**——它本来就只监听 `127.0.0.1`，从外面本来就连不进去，这是故意的设计，不用为它开端口。

如果用云厂商的安全组（阿里云/腾讯云等），去控制台放行 TCP 18080；如果宝塔面板自己有防火墙模块，同样放行 18080。80/443 交给下面的 nginx。

**7. nginx 反代 + HTTPS**（下面章节的配置照抄），在宝塔面板「网站」里新建一个站点，域名填你的域名，然后把下面的反代规则贴进去，申请一个 Let's Encrypt 证书。

**8. 验证上线**

```bash
# 代理端口能通：如果第 4 步已配了客户端 Key，这里不带 key 应返回 401（invalid_api_key）；
# 如果还没配任何 Key（开放模式），这里会直接透传到上游返回 200 —— 看到这个结果说明你还没设防，回第 4 步补上 Key
curl -i https://你的域名/v1/chat/completions

# 面板能进
# 浏览器打开 https://你的域名/admin/，应看到登录页

# 健康探针（无需登录）—— 注意 /healthz 挂在管理面板这一侧，走 /admin/ 路径
curl https://你的域名/admin/healthz    # 期望 {"ok":true}
```

用你真实签发的客户端 key 跑一次实际的 chat completions 请求，确认能拿到正常响应，再把线上应用的 base URL 切过来。

---

## nginx 反代 + HTTPS 配置

代理绑 `0.0.0.0:18080` 对外，面板绑 `127.0.0.1:18081` 仅本机，用 nginx（宝塔面板）同域名分路径反代 + HTTPS：

```nginx
# 根路径 -> 代理（应用调用入口）
location / {
    proxy_pass http://127.0.0.1:18080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_buffering off;              # 关键：不缓冲，保证 SSE 流式逐块透传
    proxy_read_timeout 3600s;         # 长连接/流式不被切断
}

# /admin -> 管理面板
location /admin/ {
    proxy_pass http://127.0.0.1:18081/;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
```

- 应用 base URL 填 `https://你的域名/v1`
- 浏览器访问面板 `https://你的域名/admin/`
- **`proxy_buffering off` 必须加**，否则 nginx 会把 SSE 流缓冲成整段
- `X-Forwarded-For` / `X-Real-IP` 让代理拿到真实客户端 IP（否则日志里都是 `127.0.0.1`）
- HTTPS 证书在宝塔面板站点设置里申请配置

## 运维接口

以下两个接口都挂在**管理面板端口**（`admin_addr`，默认 `127.0.0.1:18081`），不在代理端口上。经 nginx 反代后对应 `/admin/healthz`、`/admin/api/health`。

| 端点 | 鉴权 | 用途 |
|---|---|---|
| `GET /healthz` | 无 | 存活探针，只返回 `{"ok":true}`，供容器健康检查 / 宝塔监控 / uptime 服务使用 |
| `GET /api/health` | 需登录 | 各上游成功率、平均耗时、熔断状态明细 |

## 首次登录与安全

- 首次启动自动生成：JWT 密钥（随机）、admin 密码哈希（bcrypt，初始密码 `ChangeMe123!`）。
- 登录后**立即在「修改密码」里改成强密码**。
- 配置文件 `config.json` 含密钥和哈希，权限 0600，勿泄露。


## 本地应用如何接入

把应用原本指向第三方 API 的 base URL 改成代理地址即可，其余不变：

```python
# OpenAI SDK 示例
client = OpenAI(base_url="http://localhost:18080/v1", api_key="任意占位")
```

- 应用发的 `Authorization` / `x-api-key` 会被代理**替换**成所选上游配置的真实 key，其余请求内容原样转发。
- OpenAI SDK 打 `/v1/chat/completions` → 走 OpenAI 上游组；Anthropic SDK 打 `/v1/messages` → 走 Anthropic 上游组。
- 一个 base URL 同时服务两种协议，无需为不同格式配置不同地址。

## 配置文件

首次运行生成 `config.json`，也可在 Web 面板里增删改（面板改动会写回文件；文件被外部修改会在 1 秒内热加载）。

```jsonc
{
  "admin_addr": "127.0.0.1:18081",  // 公网部署保持 127.0.0.1，经 nginx 反代
  "proxy_addr": ":18080",
  "stats_file": "stats.json",
  "log_buffer": 200,               // 保留多少条调用明细（内存 + 落盘）
  "retry": {
    "max_attempts": 3,             // 每个上游最多尝试次数
    "base_delay_ms": 500,          // 退避基础值
    "max_delay_ms": 10000,         // 退避上限
    "total_timeout_sec": 120,      // 单请求总耗时上限（含所有重试与切换）；0 = 不限
    "retry_on_429": true,
    "retry_on_5xx": true,
    "retry_on_timeout": true
  },
  "failover": {
    "mode": "round_robin",
    "skip_unhealthy": true,        // 熔断：跳过正在失败的上游
    "max_rotations": 0             // 单请求最多切换几个上游；0 = 不限
  },
  "upstreams": [
    {
      "name": "OpenAI 主 key",
      "protocol": "openai",
      "base_url": "https://api.openai.com",
      "api_key": "sk-...",
      "priority": 10,              // 越大越优先；相同值的上游轮询分摊
      "enabled": true
    }
  ]
}
```

## 行为说明

- **重试 vs 切换 vs 透传**：
  - 429 / 5xx / 超时 / 网络错误 → 同上游重试，用尽后切下一个上游
  - **401 / 403**（上游 key 被拒）→ 不重试（同一个 key 再试还是被拒），但**会切下一个上游**
  - **400 / 404**（调用方请求有问题）→ 立即原样返回，不重试也不切换
- **切换时机**：一个上游把 `max_attempts` 次重试用完仍失败，才切下一个上游；不是每次报错就切。
- **`Retry-After`**：上游 429 若带此响应头，按它指示的时间等待（上限 5 分钟），不再用自己的指数退避。
- **key 脱敏**：面板和日志只显示 key 的首尾 4 位（如 `sk-t…1111`），完整 key 只存在配置文件里。
- **token 采集**：非流式请求直接从响应 `usage` 解析。**流式请求需在请求里带 `"stream_options":{"include_usage":true}`**，上游才会在最后一个 chunk 返回 usage，否则输出 token 无法统计（这是上游/OpenAI 协议的行为，非代理限制）。
- **真实模型/提供方**：从响应的 `model` 和 `_routed_via` 字段解析；上游不返回时留空。

## 测试

`test/mock.go` 是一个可切换返回 200/429/401 和 SSE 流式的模拟上游，用于验证代理行为：

```powershell
go run test/mock.go            # 监听 :9100
# 另开终端：POST http://localhost:9100/mode {"mode":"429"} 切换返回码
```

## 目录结构

```
cmd/          程序入口
config/       配置模型与读写
proxy/        转发 + 重试 + 轮询 + 协议路由核心
stats/        调用日志与统计（内存 + 定时落盘）
web/          管理面板（后端 API + 内嵌前端）
test/         测试用 mock 上游
docs/         需求规格
```
