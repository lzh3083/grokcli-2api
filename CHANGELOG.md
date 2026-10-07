# Changelog

All notable changes to `grokcli-2api` will be documented in this file.

---

## [v2.1.0] - 2026-10-07

> **版本分级说明**：本次更新引入全新的核心子系统「双模无人值守定时与低水位自动补齐注册调度器」，涉及 Python 守护引擎、Go 服务端路由透传、PostgreSQL 状态持久化及 Web 控制台可视化看板全链路升级，按语义化版本规范（SemVer）定级为 **功能大版本 / 次版本升级（Minor Version: `v2.0.11` → `v2.1.0`）**。

### 🚀 重点功能与重大更新 (Major Features - Minor Version Upgrade)

- **无人值守双模定时与自动补齐注册调度器 (`AutoRegisterScheduler`)**：
  - **随机时间间隔慢速保鲜模式 (`interval` / `both`)**：支持自定义最小与最大间隔区间（默认 `60~180` 分钟），每轮完成后在区间内动态随机抽取下一轮等待秒数，彻底打散固定周期的 Cron 机器人请求特征，规避 Cloudflare 与 xAI 频率风控。
  - **账号池低水位自动补齐模式 (`watermark` / `both`)**：后台常驻监测 PostgreSQL 账号池实时可用数量（`pool.live`）；一旦跌破最低警戒水位（默认 `20` 个），立即抢占触发自动补齐任务，按单次批次逐步拉升至目标恢复水位（默认 `30` 个），并内置波次冷却时间（默认 `15` 分钟）。
  - **账号池容量上限保护 (`max_pool_size`)**：当池内可用账号达到设定上限（默认 `100` 个）时，定时任务自动进入待命状态跳过本轮注册，防止无节制消耗代理流量与邮箱配额。
  - **连续失败安全熔断 (`Circuit Breaker`)**：当连续 N 轮（默认 `3` 次）自动注册均未成功入库任何可用账号时，自动触发熔断停机并在管理面板横幅告警，杜绝因代理失效或上游接口变更导致死循环空转。
  - **并发互斥锁与状态持久化**：与管理员手动发起的批量/单次注册共享会话互斥锁，检测到已有活跃注册任务时自动延后避让；调度器配置、运行阶段、熔断状态及最近 50 条历史记录全量持久化至 PostgreSQL `app_settings`。
- **Go 服务端与 Python Sidecar 全链路调度 API**：
  - 新增 `GET / PUT / POST /admin/api/accounts/register-email/scheduler`、`POST .../scheduler/trigger`（立即执行一轮）、`POST .../scheduler/reset`（重置熔断）标准管理端点及兼容回退通道。
- **Web 管理面板「定时与自动补齐注册」可视化看板**：
  - 在「账号管理 → 协议注册」页面新增定时调度配置与实时监控卡片，支持秒级倒计时刷新、实时可用水位展示、一键立即触发、一键重置熔断以及最近调度历史记录明细表。

### 🛡️ 稳定性、安全性与协议修复 (Stability & Security Fixes)

- **SSO 凭证前置持久化与 `sso_to_auth_json` 自动退避重试**：
  - 在执行 `sso_to_token` 设备流转换前，优先将注册所得原始 SSO Cookie 落盘备份至 `data/register_sso/`，彻底防止转换瞬时报错导致新注册账号丢失。
  - 为 `sso_to_token` 增加最多 3 次自动退避重试（间隔 4 秒），显著降低 xAI Device Flow 偶发 `slow_down / rate_limited` 导致的入库失败率。
- **代理池动态住宅哨兵与预检优化**：
  - 增强 `proxy_pool.py` 与 `browser_register_adapter.py` 对 `novproxy` / `residential` 动态代理哨兵的识别及多地区（SG / JP / US）自适应提取；默认关闭非必要的破坏性预检请求并隔离本地 Turnstile Solver `API_KEY`。
- **全面安全加固与流式协议缺陷修复 (P0–P3)**：
  - 修复热更新脚本参数校验、管理端 Cookie `Secure` 属性、`write_auth_map` 行级安全 `UPSERT`、并行首字节探测独立 Context 隔离及 Anthropic / Responses 流式工具调用内容保留问题。

---

## [v2.0.11] - 2026-10-03

### 🚀 重点功能与重大更新 (Major Improvements)

- **控制台多维表格双向联动顶部横向滚动条 (Dual Horizontal Scrollbars)**：
  - **顶部/底部双向实时同步**：针对「用量明细」、「账号列表」、「模型管理」等宽表格场景，在表格顶部自动增加轻量级横向滚动条。无论用户在查看表格头部还是尾部，均可直接抓取拖动，顶部与底部滚动位置 100% 毫秒级双向同步联动。
  - **ResizeObserver 动态响应**：无论分页切换、过滤搜索还是窗口尺寸变动，系统自动感知表格宽度变化；仅在表格产生横向溢出时呈现顶部滚动条，无溢出时自动隐退。
  - **现代化轻量 UI 调优**：全面重构表格滚动条外观为现代圆角 Ant Design 风格，解决传统系统滚动条粗重难看、抓取不灵敏的问题。

---

## [v2.0.10] - 2026-10-02

### 🚀 重点功能与重大更新 (Major Improvements)

- **修复版本检查与更新自洽展示逻辑 (Version Update Self-Consistency)**：
  - **更新源对齐当前仓库**：默认 Release 检查仓库由原作者上游重定向至当前仓库 `lzh3083/grokcli-2api`，避免因拉取上游停滞版本（v2.0.4）导致最新版本倒退展示。
  - **智能 Tags 回退机制**：当 GitHub Releases 接口未发布 Formal Release 时，自动调用 GitHub Tags 接口提取最新语义化版本（SemVer），精准获取实际发布的版本标签。
  - **本地 CHANGELOG 智能解析**：当版本为最新或处于超前构建状态时，自动从系统内置 `CHANGELOG.md` 中提取当前版本的完整更新说明与亮点展示。
  - **版本防降级逻辑兜底**：彻底修复“当前版本高、最新版本反显为旧版”的逻辑倒挂缺陷；当远端版本低于本地运行版本时，自动对齐当前版本为最新，确保管理后台展示自洽。

---

## [v2.0.9] - 2026-10-02

### 🚀 重点功能与重大更新 (Major Improvements)

- **动态住宅家宽流量极致优化体系 (Traffic Saving Engine)**：
  - **网络层路由拦截 (Context Route Abort)**：在 Camoufox 浏览器 Context 层拦截所有对注册与过盾无实质作用的重型资源：
    - 拦截字体与媒体文件（`.woff2`、`.woff`、`.ttf`、`.mp4`、`.webm` 等）；
    - 拦截所有纯装饰性大图片（Next.js 图标、横幅、SVG 动画等）；
    - 拦截分析遥测域名（`statsig.com`、`datadoghq.com`、`sentry.io`、`google-analytics.com`、`analytics.x.ai`）；
    - 严格白名单放行 `challenges.cloudflare.com`、Turnstile 核心脚本与 Next.js 核心 JS 逻辑。
  - **首选项调优 (Firefox Prefs Tuning)**：
    - 默认关闭外部远程字体下载（`browser.display.use_document_fonts=0`）；
    - 禁用浏览器自带的遥测上报与自动更新。
  - **提前切断与连接回收 (Early Browser Reclaim)**：
    - 一旦成功提取 SSO Cookie，立即主动切断 Camoufox 浏览器进程并回收代理连接，杜绝后续风控检测与后处理期间后台连接持续偷跑住宅流量。

---

## [v2.0.8] - 2026-10-02

### 🚀 重点功能与重大更新 (Major Improvements)

- **升级官方 Grok CLI 协议版本标识（0.2.93 -> 1.0.13）**：
  - 针对 x.ai 上游针对客户端版本的强制校验拦截（`Your Grok CLI version (0.2.93) is outdated. Please update to version 1.0.13 or later`），全面更新 Go 主程序与 Python 运行时请求头中的 `x-grok-client-version` 及 User-Agent 协议标识至 `1.0.13`，打通最新 API 会话交互。

---

## [v2.0.7] - 2026-10-02

### 🚀 重点功能与重大更新 (Major Improvements)

- **原生支持 NovProxy 动态住宅/家宽代理自动接入与国家指纹自适应**：
  - 针对东京节点机房 IP（Cloudflare WARP / 数据中心段）导致账号被 x.ai 后台打标降智的痛点，彻底打通 NovProxy 日本家庭住宅宽带（KDDI / NTT）出口。
  - 新增 `proxy_mode: "novproxy"` 支持：每次注册时自动调用白名单 API（`https://white.novproxy.com/white/api`），动态提取 1 个高权重独立住宅代理节点，天然实现“一号一物理家宽 IP”。
  - 增强 `us_consistency` 多国自适应：当代理出口为日本（JP）时，自动解除强制美国时区限制，将浏览器时区自适应对齐为 `Asia/Tokyo`、语言为 `ja-JP`，杜绝“日本 IP + 纽约时区”的指纹撕裂。

---

## [v2.0.6] - 2026-10-02

### 🚀 重点功能与重大更新 (Major Improvements)

- **防账号降智与全生命周期信誉增强体系 (Anti-Degrade & Warm-up Lifecycle)**：
  - **注册行为拟人化注入 (Human Entropy)**：
    - 页面视读等待：访问注册首页后加入随机 1.2～2.5 秒人类停留，模拟阅读页面。
    - 验证码接收拟人化延时：收到邮件验证码后加入 1.8～3.2 秒切屏/视读等待，彻底告别“0秒光速写入”的机器人特征。
    - 提交资料前人工核对微动：资料填写完毕后增加 1.0～2.2 秒随机停顿再提交。
  - **新账号破冰日常会话 (Warm-up Conversation)**：
    - 注册成功提取 SSO 后，在入库前自动对 grok.com 发起首轮日常轻量问答（`Hi, how are you today?`）。
    - 使得账号在 x.ai 后台首次落地即建立合法真实的 Conversation 历史上下文，脱离“初生空白号”高风险特征。
  - **降智自动检测与隔离闭环 (Quality Probe Enforcement)**：
    - 默认启用深度思考推理探测，以 `reasoning_tokens` 为金标准判定账号健康度。
    - 发现无思考（`reasoning_tokens=0`）的账号自动打标 `disabled` 移出活跃轮询池，确保下游大模型请求永远输出高质量推理内容。

---

## [v2.0.5] - 2026-10-02

### 🚀 重点功能与重大更新 (Major Improvements)

- **集成 Camoufox 反反爬浏览器引擎彻底旁路 Cloudflare Bot Management**：
  - 针对 x.ai 从 2026 年 9 月下旬起在 `accounts.x.ai` 实施的 Cloudflare Bot Management（JSD）机制，彻底解决 Chromium（CDP 协议 / `AutomationControlled`）被识别导致邮箱注册提交被拦截（提示 `Email sign-up isn't available right now. Sign up another way.`）及验证码邮件被静默丢弃的问题。
  - 采用加固版 Firefox（Camoufox，基于 Juggler 协议），自带真实 Windows/Linux 指纹、WebGL、时区与平台伪造，验证码邮件 2～3 秒秒收。
  - 新增 `camoufox_compat.py` 运行时适配器，向下兼容 DrissionPage 核心 API（`page.run_js`、`page.ele`、`page.wait.doc_loaded()` 等），支持无缝切换浏览器引擎。

- **修复 React Fiber 深度穿透与 Turnstile Token 注入**：
  - 针对 Next.js RSC / React 18+ 注册提交守卫强制要求 `onToken` 写入 React state 的限制，在 Firefox (Camoufox) 沙箱环境下，解决普通 DOM 对象无法枚举非公开 expando 属性的问题。
  - 在 `registration_browser.py` 中引入 `wrappedJSObject` 穿透，实现从表单/根节点出发的有界 BFS 遍历，精准捕获持有 `onToken` 的 Fiber 节点并自动注入（`called-onToken@6`）。
  - 在 `camoufox_compat.py` 的全局垫片 `_TURNSTILE_SHIM_JS` 中增加回调录制与实时 Bridge 桥接，保证本地 Solver 与 React 内部组件的双向连通。

- **打码流程优化与本地内联 Solver 深度联动**：
  - 优化 `_wait_for_turnstile` 流程，在 Camoufox 引擎下优先调用容器内集成的本地 Camoufox YesCaptcha Solver（`http://127.0.0.1:5072`），跳过无意义的被动等待与 CSP 拦截的页内注入。
  - 实现从邮箱创建、验证码秒提、本地 Solver 解题、React Fiber 状态注入、资料表单提交、SSO 提取到风控检查与 NSFW 开通的 100% 全自动毫秒级闭环。

### 🛠️ 修复与优化 (Bug Fixes & Refactoring)

- **`camoufox_compat.py`**：
  - 新增 `_PageWaiter` 兼容类，同时支持 `page.wait.doc_loaded()` 与 `page.wait(seconds)` 调用。
  - 增强 `_TURNSTILE_SHIM_JS` 对 `render`、`getResponse`、`execute` 的代理拦截与 callback 保存。
- **`captcha_solver.py`**：
  - 升级 `activate_turnstile_bridge`，返回包含 `renderCalls`、`pending`、`fired` 等结构化监控指标。
  - 增强 `solve_and_inject`，确保打码成功后同时触发 bridge 与 input 赋值。
- **`registration_browser.py`**：
  - 将 `browser_engine` 与 `browser_runtime` 加入 `_OWN_NAMES` 防止命名空间污染覆盖。
  - 完善 `fill_profile_and_submit` 中的超时与重试保护，增加提交前延时确保 React state 写入生效。

---

## [v2.0.4] - 2026-09-28

- 容器内热更新支持（无需宿主机 watcher）
- API Key 列表与管理面板显示修复
- 注册线路默认代理配置支持
- Go 主程序并发连接池与超时参数优化
