# grokcli-2api

<p align="center">
  <img src="https://raw.githubusercontent.com/lzh3083/grokcli-2api/main/static/favicon.svg" width="96" height="96" alt="grokcli-2api logo" />
</p>

<p align="center">
  <strong>高性能 Grok 转 OpenAI / Anthropic 协议网关</strong><br>
  原生支持防降智拦截 · 假思考密文过滤 · 会话粘性 Prompt Cache · 自动换号容灾 · 多协议批量注册
</p>

<p align="center">
  <a href="https://github.com/lzh3083/grokcli-2api/releases"><img src="https://img.shields.io/github/v/release/lzh3083/grokcli-2api?color=blue&label=Release" alt="Release"></a>
  <a href="https://github.com/lzh3083/grokcli-2api/pkgs/container/grokcli-2api"><img src="https://img.shields.io/badge/GHCR-ghcr.io%2Flzh3083%2Fgrokcli--2api-blue" alt="GHCR"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License"></a>
  <img src="https://img.shields.io/badge/Go-1.24-00ADD8?logo=go" alt="Go 1.24">
  <img src="https://img.shields.io/badge/Docker-Multi--Arch-2496ED?logo=docker" alt="Docker">
</p>

---

## 目录

- [核心特性](#-核心特性)
- [防降智与假思考拦截机制 (New)](#-防降智与假思考拦截机制-new)
- [系统架构](#-系统架构)
- [快速开始](#-快速开始)
  - [方式 A：Docker Compose 一键部署（推荐）](#方式-adocker-compose-一键部署推荐)
  - [方式 B：预构建镜像运行](#方式-b预构建镜像运行)
- [环境变量配置](#-环境变量配置)
- [API 客户端接入](#-api-客户端接入)
  - [OpenAI Chat 接口](#openai-chat-接口)
  - [OpenAI Responses 接口 (Codex / Grok CLI)](#openai-responses-接口-codex--grok-cli)
  - [Anthropic Messages 接口 (Claude Code)](#anthropic-messages-接口-claude-code)
- [管理控制台与号池运维](#-管理控制台与号池运维)
- [版本历史](#-版本历史)
- [License](#-license)

---

## 🌟 核心特性

- **三大主流协议原生兼容**：
  - **OpenAI Chat Completions** (`/v1/chat/completions`)
  - **OpenAI Responses API** (`/v1/responses`) —— 为 Codex、Grok CLI 与 OpenAI 原生工具链量身打造
  - **Anthropic Messages** (`/v1/messages`) —— 原生适配 Claude Code、Cursor、Cherry Studio
- **深度防降智拦截与流式扣留 (Peek & Hold)**：
  - 首创流式质量探测状态机，在首包下发客户端前自动扣留校验思考真实性
  - 严格校验密文下限（Ciphertext Floor），精准拦截 `gAAAA-` 短伪密文与空推理 Stub
  - 遭遇降智时静默自动轮换健康账号重试（最多 6 轮），降智账号自动惩罚冷却 12 小时
- **极致的会话粘性 (Prompt Cache)**：
  - 支持 `prompt_cache_key`、Claude Code Session UUID 与上轮 `previous_response_id` 自动锁定同账号
  - 消息/工具 Schema 出站规范化排序，最大化命中上游 Prefix Cache 降低首字延迟与计费
- **Claude Code / Codex 工具调用加固**：
  - 自动将 Grok 专有的 `Update` / `StrReplace` 映射纠偏为 Claude Code 标准 `Edit`
  - 补齐必选字段 `"annotations": []` 与错误终态 `response.failed.model`，消除客户端序列化异常崩溃
- **生产级混合存储体系 (Hybrid Store)**：
  - **PostgreSQL**：持久化管理账号凭据、API Keys、系统设置、审计与任务日志
  - **Redis**：维护多 Worker 共享分布式锁、会话粘性缓存、请求 In-flight 计数与实时监控
- **全自动协议注册机**：
  - 内置 Camoufox 反反爬浏览器引擎与内联 Turnstile 验证码求解器
  - 支持 MoeMail、YYDS Mail、GPTMail、TempMail 等多种临时邮箱批量全自动注册
  - 具备批次自愈、孤儿会话回收与一键推送导出能力

---

## 🛡️ 防降智与假思考拦截机制 (New)

xAI / Grok 模型在上游账号异常或受到安全风控时，常常出现**静默降智（Fake Reasoning）**：即返回虚假的推理账单（`reasoning_tokens` 虚高），但流式过程中只发送空存根（`: grok2api-reasoning-start`）、极短的伪加密密文，或直接在没有思考阶段的情况下秒吐简答正文。

本项目现已全面移植并升级了核心流式质量探测引擎：

```
上游响应流 (SSE) 
       │
       ▼
 ┌────────────────────────────────────────────────────────┐
 │            流式探测与窥探器 (Peek & Hold)              │
 │  - 最多等待 30s 或可见正文达到 8 个 tokens             │
 └────────────────────────────┬───────────────────────────┘
                              │
            ┌─────────────────┴─────────────────┐
            ▼                                   ▼
    【命中假思考/降智行为】              【有效真实思考证据】
    1. 空 reasoning stub 占位           1. 流式明文思考增量 (delta)
    2. 密文 < floor(256B, tok*4)        2. 密文 >= max(256B, tok*4)
    3. <2s 瞬时大正文倾倒 (Burst)                 │
    4. 128k 状态死循环 (Drool)                    ▼
            │                           【立即放行 (Deliver)】
            ▼                            零延迟无损流转客户端
    【判定扣留 (Withhold)】
            │
            ├─► 降智账号打入 12 小时冷却惩罚
            └─► 释放连接，自动挑选下一账号重试 (最多6轮)
```

### 关键判决原则：
1. **密文下限验证 (Ciphertext Floor)**：
   $$\text{EncryptedBytes} \ge \max(256\text{ 字节},\; \text{ReasoningTokens} \times 4)$$
   凡低于此阈值的加密段（如测试 stub `gAAAA-...`）绝不视作思考证据。
2. **零额外延迟放行**：一旦在流式前缀中捕获到合规密文或明文思考片段，网关立刻通过 `io.MultiReader` 拼接前缀与剩余流，**0 额外延迟下发给客户端**，保证极致的首字延迟（TTFT）。
3. **空流即刻重试**：遇到上游返回的空 `response.completed` 或零输出 `[DONE]` 时，无需死等超时，几毫秒内立即换号重试。

---

## 🏗️ 系统架构

```
客户端 (OpenAI SDK / Anthropic SDK / Claude Code / Codex / NextChat)
                           │  HTTP / SSE
                           ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                     grokcli-2api (Go 高性能主进程)                       │
│                                                                         │
│  ├─ API 网关路由: /v1/chat/completions · /v1/responses · /v1/messages   │
│  ├─ 质量探测层: PeekQualityStream (思考密文校验 · 防降智扣留 · 静默重试)│
│  ├─ 账号调度中心: 负载均衡 (Least-Used/RR) · 冷却状态机 · In-flight 限制 │
│  ├─ 协议桥接器: ResponsesBridge (双向协议转换 · 字段纠偏 · 注释透传)   │
│  └─ 管理后台 /admin: 账号管理 · API Key 配额 · 节点测活 · 用量大盘       │
└─────────────────────┬───────────────────────────┬───────────────────────┘
                      │                           │
                      ▼                           ▼
        ┌──────────────────────────┐    ┌──────────────────┐
        │   PostgreSQL 关系型持久库 │    │    Redis 内存库  │
        │   (账号/Key/配置/审计日志)│    │ (会话粘性/锁/监控)│
        └──────────────────────────┘    └──────────────────┘
                      │ (Loopback)
                      ▼
        ┌──────────────────────────────────────────────────┐
        │      Python Sidecar (可选辅助服务 - 仅注册场景)   │
        │   - Camoufox 真实指纹浏览器环境                  │
        │   - Turnstile Solver 验证码本地/云端解题         │
        └──────────────────────────────────────────────────┘
                      │
                      ▼
             cli-chat-proxy.grok.com (xAI 上游)
```

---

## 🚀 快速开始

### 方式 A：Docker Compose 一键部署（推荐）

1. **克隆仓库并初始化配置**：
   ```bash
   git clone https://github.com/lzh3083/grokcli-2api.git
   cd grokcli-2api
   cp .env.example .env
   ```

2. **按需编辑 `.env` 文件**（请务必修改管理后台密码与数据库口令）：
   ```bash
   # 管理后台登录密码（首次启动将作为初始密码种子）
   GROK2API_ADMIN_PASSWORD="YourSecureAdminPassword"
   
   # 数据库访问凭据
   DATABASE_URL="postgresql://grok2api:YourDBPassword@postgres:5432/grok2api"
   ```

3. **启动所有容器**：
   ```bash
   docker compose up -d --build
   ```

4. **检查运行健康状况**：
   ```bash
   curl -fsS http://127.0.0.1:3000/health
   # 响应: {"status":"healthy"}
   ```
   随后即可在浏览器访问管理控制台：`http://你的服务器IP:3000/admin`。

---

### 方式 B：预构建镜像运行

```bash
docker run -d \
  --name grokcli-2api \
  -p 3000:3000 \
  -e GROK2API_ADMIN_PASSWORD="YourSecureAdminPassword" \
  -e DATABASE_URL="postgresql://grok2api:pwd@your-pg:5432/grok2api" \
  -e REDIS_URL="redis://your-redis:6379/0" \
  ghcr.io/lzh3083/grokcli-2api:latest
```

---

## ⚙️ 环境变量配置

所有核心功能均支持通过环境变量灵活调优（开箱自带最合理的生产默认值）：

| 环境变量 | 默认值 | 详细说明 |
|---|---|---|
| **`GROK2API_QUALITY_HOLD_ENABLED`** | `true` | 是否启用流式防降智探测与拦截重试 |
| **`GROK2API_QUALITY_MAX_ATTEMPTS`** | `6` | 遭遇降智或空流响应时的最大自动换号重试次数 |
| **`GROK2API_QUALITY_HOLD_TIMEOUT_SEC`** | `30` | 流式首包思考证据的最长等待保留时间（秒） |
| **`GROK2API_QUALITY_MIN_OUTPUT_TOKENS`** | `8` | 触发缺少思考判定时的最小可见正文 Token 阈值 |
| **`GROK2API_QUALITY_COOLDOWN_HOURS`** | `12` | 被判定为降智的账号的惩罚冷却时间（小时） |
| **`GROK2API_EMPTY_OUTPUT_BLOCK_SEC`** | `240` | 上游空模型输出时软封禁特定模型的时间（秒） |
| `GROK2API_HOST` | `0.0.0.0` | HTTP 服务监听绑定地址 |
| `GROK2API_PORT` | `3000` | HTTP 服务监听端口 |
| `GROK2API_ADMIN_PASSWORD` | - | Web 管理面板登录密码种子 |
| `GROK2API_WORKERS` | `4` | 内部处理请求的 Worker 并发工作协程数 |
| `GROK2API_INLINE_SOLVER` | `1` | 是否在主容器内启用 Python 验证码求解 Sidecar |
| `TURNSTILE_THREAD` | `3` | 本地 Camoufox 验证码求解并发浏览器线程数 |
| `TZ` | `Asia/Shanghai` | 容器运行时区 |

---

## 🔌 API 客户端接入

在管理后台的 **「API Keys」** 页面生成你的专属令牌（例如 `sk-g2a-xxxx`）。

### OpenAI Chat 接口

```bash
curl -N http://127.0.0.1:3000/v1/chat/completions \
  -H "Authorization: Bearer sk-g2a-xxxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5",
    "stream": true,
    "messages": [
      {"role": "user", "content": "请用一句话解释相对论，并详细展示你的思考推导过程。"}
    ]
  }'
```

### OpenAI Responses 接口 (Codex / Grok CLI)

```bash
curl -N http://127.0.0.1:3000/v1/responses \
  -H "Authorization: Bearer sk-g2a-xxxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5",
    "stream": true,
    "input": [
      {"role": "user", "content": "写一个快速排序的 Go 语言实现。"}
    ]
  }'
```

### Anthropic Messages 接口 (Claude Code)

```bash
curl -N http://127.0.0.1:3000/v1/messages \
  -H "x-api-key: sk-g2a-xxxx" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5",
    "max_tokens": 1024,
    "stream": true,
    "messages": [
      {"role": "user", "content": "分析以下代码的潜在并发安全问题..."}
    ]
  }'
```

---

## 🖥️ 管理控制台与号池运维

进入 Web 管理台（`/admin`）可使用如下强大功能：

1. **号池健康监控与调度**：
   - 查看账号池总览：**正常轮询**、**冷却中**、**额度耗尽**、**模型封禁** 及 **Token 已过期** 严格互斥分类。
   - 自动令牌续期：后台定期检测并在 Token 到期前平滑完成刷新。
2. **多源账号导入 / 导出**：
   - 支持粘贴 **SSO Cookie** 自动化换取 Token 入库。
   - 支持多格式 JSON 账号池批量导入与无阻塞全量导出。
   - 支持**一键推送到 sub2api** 及与 **CLIProxyAPI (CPA)** 凭证目录双向互通。
3. **高频可观测指标**：
   - 记录每次调用的首字时间（TTFT）、总耗时（Latency）、输入输出 Token 数与**思考 Token 详情**。
   - 实时监控 Prompt Cache 缓存命中率。

---

## 📜 版本历史

- **v2.1.6 (Current)**
  - **核心防降智与假思考拦截引擎落地**：实现流式 Peek & Hold（30s 探测窗口与 8 Token 可见正文下限）。
  - **密文下限验证 (Ciphertext Floor)**：严格校验 $\ge \max(256\text{B}, \text{Tokens} \times 4)$，彻底根除假推理与 stub 刷额度。
  - **12 小时降智惩罚与 6 轮自动容灾**：遇到降智流秒级自动换号重试，降智账号自动冷却 12h。
  - **协议兼容性补齐**：为 `output_text` 注入必选 `annotations: []`，消除 Grok CLI 反序列化异常；补齐 `response.failed.model` 终态模型名。
  - **双节点生产验证**：在甲骨文新加坡与甲骨文东京两大生产节点经过三协议高并发流式压力验证。
- **v2.1.5**
  - 自愈看门狗与调度优化，修复过期 registration 会话引起的延迟循环。
- **v2.0.5**
  - 集成 Camoufox 真实指纹引擎与 React Fiber 穿透注入，显著提升自动注册成功率。
- **v2.0.0**
  - 正式发布 Go 语言重构的高性能主进程架构。

---

## 📄 License

本项目采用 [MIT License](./LICENSE) 开源协议。
