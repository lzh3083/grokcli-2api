# 从旧版升级到 hybrid（Redis + PostgreSQL）

当前版本默认 **高并发 hybrid 模式**：

- **PostgreSQL**：账号凭证、API Key、设置、账号池状态（含冷却）
- **Redis**：粘性会话、热计数、轮询游标、维护锁、管理会话
- `data/*.json` **仅作迁移源与管理台导入导出**，运行时不再写本地 JSON 镜像

---

## 场景 A：旧版仅文件后端（`data/auth.json` 等）→ 新版 hybrid

### 1. 备份

```bash
# 备份旧 data 目录
cp -a ./data ./data.backup-$(date +%Y%m%d)

# 若已有 PostgreSQL，也请先备份
# pg_dump "$DATABASE_URL" > pg-backup-$(date +%Y%m%d).sql
```

### 2. 配置环境

```bash
cp .env.example .env
# 至少设置：
# GROK2API_ADMIN_PASSWORD=...
# REDIS_URL=...
# DATABASE_URL=...
# GROK2API_STORE_BACKEND=hybrid
# GROK2API_REQUIRE_SHARED_STORES=1
```

### 3. 启动依赖并执行 Schema 迁移与数据导入

> 说明：旧版 Python 脚本 `scripts/migrate_json_to_pg.py` 已废弃移除。当前 2.x 版本统一使用 Go 迁移工具 `bin/grok2api-migrate` 执行数据库表结构迁移（Docker 容器 `entrypoint.sh` 启动时也会自动执行 `grok2api-migrate up`），旧版 `data/auth.json` 等文件数据可通过管理后台导入。

**方式一：Docker Compose 启动自动迁移（推荐）**

```bash
# 容器启动时 entrypoint.sh 会自动执行 /app/bin/grok2api-migrate -dir migrations up
docker compose up -d
```

**方式二：使用升级辅助脚本或手动运行 `bin/grok2api-migrate`**

```bash
chmod +x scripts/upgrade_from_file_backend.sh
./scripts/upgrade_from_file_backend.sh --data-dir ./data
```

或手动执行 Schema 迁移：

```bash
docker compose up -d redis postgres
# 容器内执行迁移：
docker compose run --rm --no-deps \
  -e DATABASE_URL=postgresql://grok2api:grok2api@postgres:5432/grok2api \
  grokcli-2api \
  /app/bin/grok2api-migrate -dir /app/migrations up
```

本地非 Docker 环境（需本机已有可连接的 PostgreSQL）：

```bash
export DATABASE_URL=postgresql://grok2api:grok2api@127.0.0.1:5432/grok2api
./bin/grok2api-migrate -dir ./migrations up
./bin/grok2api-migrate -dir ./migrations verify
```

### 4. 启动应用并导入旧 JSON 数据

```bash
docker compose up -d
curl -fsS "http://127.0.0.1:${GROK2API_PORT:-3000}/health"
```

启动后登录管理后台（`http://127.0.0.1:${GROK2API_PORT:-3000}/admin`，默认端口 `3000`）：
- 在 **「账号管理」→「批量导入」** 中上传或粘贴原 `data/auth.json` 中的账号凭证数据完成导入；
- 在 **「API Keys」** 页面重新创建或管理调用令牌。

### 5. 验证

- 管理台账号数量与迁移前一致
- API Key 仍可访问 `/v1/models`
- 冷却/启用状态在账号列表可见（来自 `account_pool`）

### 迁移范围

| 迁入 PostgreSQL | 不迁移 |
|-----------------|--------|
| `auth.json` → `accounts` | Redis 热计数 / 粘性（可空启动） |
| `keys.json` → `api_keys` | 管理台登录会话（需重新登录） |
| `settings.json` 标量 / 注册配置 / 管理员密码哈希 | 审计日志历史（若旧版无表） |
| `settings.json` 内 `account_pool` | — |
| `models_cache.json` → `models` 表（一次性迁移，可 `--skip-models`） | 运行时不再使用 `models_cache.json` |

### 注意

- **首次迁移不要多实例并发跑** migrator；迁移完成后再拉高 `GROK2API_WORKERS`
- 使用 `--merge-pool` 可在 PG 已有数据时合并，避免误清空
- `keys` 导入为 **整表替换**（`replace_all`）；若 PG 里已有 Key 且 JSON 不全，请先备份
- 迁移完成后 hybrid 运行时 **不会** 再写回 `auth.json` / `keys.json` / `settings.json` / `affinity.json`；备份请用管理台导出或 `pg_dump`

---

## 场景 B：已是 hybrid，仅升级应用版本（含 1.x → 2.0.3）

适用于：已在用 PostgreSQL + Redis，从 **1.9.x / 2.0.0 / 2.0.1** 升到 **2.0.3**。

### 1. 备份

```bash
docker exec grokcli-2api-postgres pg_dump -U grok2api -d grok2api \
  > ~/grok2api-backup-$(date +%F-%H%M%S).sql
```

### 2. 拉新镜像 / 重建

```bash
# GHCR（镜像名必须全小写）
docker pull ghcr.io/hm2899/grokcli-2api:2.0.3
# compose 中 image 改为 :2.0.3 或 :latest 后：
docker compose up -d

# 或本地构建
docker compose pull   # 若用 GHCR
# 或
docker compose build
docker compose up -d
curl -fsS "http://127.0.0.1:${GROK2API_PORT:-3000}/health"
```

Docker 入口会自动跑 `grok2api-migrate up`（≥2.0.1）。Go 进程本身只校验 schema、不改表结构。

### 3. 验证

- 管理台账号 / API Key 数量与升级前一致  
- **Ctrl+F5** 硬刷管理台（加载新 `core.*.js`）  
- 额度：曾成功查询的账号应从 DB `last_quota` 回填类型与用量  
- 新注册账号测活成功后应保持 **轮询中**（不进冷却池）  

### 4. 1.x → 2.x 说明

| 变化 | 说明 |
|------|------|
| 主进程 | 默认 **Go**；Python 仅 loopback sidecar（注册 / SSO / 过盾） |
| Schema | Docker 入口自动 migrate |
| 额度 | 2.0.3 起类型/用量持久化；历史 error 壳不当有效额度 |
| 排序 | 默认按加入时间；刷新/测活不打乱顺序 |

---

## 包结构迁移提示

真实实现已收敛到 `grok2api/` 包内：

- `grok2api/app.py`：FastAPI 应用主入口；根目录 `app.py` 只是兼容启动包装。
- `grok2api/store/`：Redis / PostgreSQL 存储层；根目录 `store/` 只是兼容包装。
- `grok2api/admin|pool|protocol|upstream/`：管理台、账号池、协议适配、上游集成。

旧脚本里的根导入仍暂时兼容，但新代码建议改成包路径：

```python
# old
import account_pool
from store.pg import connection

# new
from grok2api.pool import account_pool
from grok2api.store.pg import connection
```

---

## 回滚建议

- **文件时代**：保留 `data.backup-*`，可临时 `GROK2API_STORE_BACKEND=file` + `GROK2API_REQUIRE_SHARED_STORES=0` + `WORKERS=1` 应急（不推荐生产）
- **hybrid**：用 `pg_dump` 备份恢复；Redis 可丢（热状态）

---

## 相关命令速查

```bash
# 查看 SQL 迁移状态（只读，不修改数据库）
./bin/grok2api-migrate -dir ./migrations status
# 或使用包装脚本：
./scripts/upgrade_from_file_backend.sh --status

# 执行 SQL 迁移并校验 checksum
./bin/grok2api-migrate -dir ./migrations up
./bin/grok2api-migrate -dir ./migrations verify

# 健康与存储检查
curl -s "http://127.0.0.1:${GROK2API_PORT:-3000}/health" | jq .
curl -s "http://127.0.0.1:${GROK2API_PORT:-3000}/metrics" | head
```


---

## 场景 C：Go 2.x 空库 / `schema_migrations does not exist`

Go 主进程**不会**在启动时改 schema，只校验 `schema_migrations`。Docker ≥2.0.1 入口会自动跑 `grok2api-migrate up`。

手工恢复（兼容旧库，`IF NOT EXISTS` 不删数据）：

```bash
# 备份
docker exec grokcli-2api-postgres pg_dump -U grok2api -d grok2api \
  > /root/grok2api-before-migration-$(date +%F-%H%M%S).sql

# 迁移 + 校验
docker exec grokcli-2api /app/bin/grok2api-migrate -dir /app/migrations up
docker exec grokcli-2api /app/bin/grok2api-migrate -dir /app/migrations verify

# 重启
docker restart grokcli-2api
curl -fsS "http://127.0.0.1:${GROK2API_PORT:-3000}/health"
```

新部署用 `docker compose up -d` 即可；入口默认 `GROK2API_AUTO_MIGRATE=1`。
