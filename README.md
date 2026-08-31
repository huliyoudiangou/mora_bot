# mora_bot —— Jellyfin 用户管理 Telegram Bot

mora_bot 是一个用 Go 编写的 **Jellyfin 用户管理 Telegram Bot**：让用户在 Telegram 里自助注册/绑定 Jellyfin 账号、用卡密续期、签到攒积分，管理员通过面板发卡、管用户、处理求剧工单，全程无需登录服务器。

## 功能矩阵

| 模块 | 功能                                                                            | 状态 |
| ---- | ------------------------------------------------------------------------------- | ---- |
| 账号 | 邀请码注册（克隆模板用户 Jellyfin 权限策略）                                    | ✅   |
| 账号 | 绑定 / 续期 / 改密（旧密码验证 → 管理员重置）                                   | ✅   |
| 账号 | 自助解绑、自助注销（彻底删除 Jellyfin 账号）                                    | ✅   |
| 卡密 | 管理员批量生成邀请码 / 续期码（HMAC + XChaCha20-Poly1305 加密存储，明文不落库） | ✅   |
| 卡密 | 面板交互式发卡：邀请码选数量；续期码先选天数再选数量 | ✅   |
| 卡密 | 果果币兑换续期码 / 邀请码（/shop buy，价格运行时可在管理面板调整）                                               | ✅   |
| 卡密 | 续期码核销（/redeem）                                                                                           | ✅   |
| 积分 | 每日签到 + 连签加成 + 完整流水                                                                                  | ✅   |
| 追剧 | 追剧中心：用户提交红果短剧求剧工单（/drama）                                                                    | ✅   |
| 管理 | 管理/用户面板分离：/start 仅用户面板，管理面板仅 /admin（tg_id 须与 env 一致）                                  | ✅   |
| 管理 | 管理面板：统计 / 发卡 / 调积分 / 查用户 / 卡密溯源 / 白名单 / 卡密定价 / Jellyfin 线路管理                      | ✅   |
| 白名单 | 白名单用户永久有效、不受规则约束、无需保号（面板添加/移除/查看）                                                | ✅   |
| 线路 | 管理员维护 Jellyfin 线路；用户面板「查询线路」                                                                    | ✅   |
| 审计 | 管理员高危操作写 audit_logs                                                                                     | ✅   |
| 运维 | SQLite（glebarez pure-Go 无 CGO）+ WAL                                          | ✅   |

## 快速开始

```sh
cp .env.example .env    # 填写必填项
go mod tidy
go build ./...
go test ./...
go run ./cmd/mora
```

## Docker Compose 部署

项目内置 `Dockerfile` + `docker-compose.yml`，纯 Go + 无 CGO，构建为轻量静态二进制，容器内以非 root 用户运行。

```sh
# 1. 准备配置（已配置过 .env 可直接使用）
cp .env.example .env

# 2. 构建并启动（后台）
docker compose up -d --build

# 3. 查看日志
docker compose logs -f mora

# 4. 停止 / 重启 / 移除
docker compose down
docker compose restart
```

- `.env` 由 compose `env_file` 直接注入容器，**密钥不进入镜像**（见 `.dockerignore`）
- 数据库 `data/mora_bot.db` 通过 volume `./data:/app/data` 持久化，删容器不丢数据
- Telegram 为轮询模式，无对外端口，无需端口映射
- 应用收到 `SIGTERM` 后优雅退出（`main.go` 已用 `signal.NotifyContext` 处理）

> 提示：Windows 上需先启动 Docker Desktop（引擎运行中）再执行上述命令。

## 后台任务（自动）

| 任务 | 说明 | 配置开关 |
| ---- | ---- | -------- |
| 启动通知 | 进程启动后私聊所有管理员 | `BOT_STARTUP_NOTIFY_ADMINS=true` |
| 每日自动备份 | 每日在指定小时备份 SQLite（可选加密、可选推送到群），保留 N 份 | `BACKUP_DAILY_HOUR=3` / `BACKUP_KEEP_COUNT=14` / `BACKUP_ENCRYPT_KEY` / `BACKUP_GROUP_ID` |
| 到期提醒 | 每天扫描即将到期的用户并私聊提醒续费 | `NOTIFY_BEFORE_DAYS=3`（0=关闭） |

## 完整环境变量（见 .env.example）

**必填**：`TELEGRAM_BOT_TOKEN`、`JELLYFIN_API_KEY`、`SECURITY_PEPPER`。
**管理员**：`ADMIN_TELEGRAM_IDS`（与 `SUPER_ADMIN_TG_IDS` 兼容）。
**模板用户**：`JELLYFIN_TEMPLATE_USER_ID`。
**经济**：`SIGN_BASE_REWARD`、`SIGN_STREAK_BONUS`、`SIGN_STREAK_BONUS_CAP`、`PRICE_RENEWAL_CODE`、`DEFAULT_RENEWAL_DAYS`、`PRICE_INVITE_CODE`。
**账号有效期**：`NEW_ACCOUNT_VALID_DAYS`（新注册默认天数，0=永久）、`NOTIFY_BEFORE_DAYS`。
**追剧**：`DRAMA_REQUEST_DAILY_LIMIT`（每日求剧上限，0=不限）。
**通知**：`NOTICE_GROUP_ID`、`BOT_STARTUP_NOTIFY_ADMINS`。
**备份**：`BACKUP_DAILY_HOUR`（-1=关闭）、`BACKUP_KEEP_COUNT`、`BACKUP_ENCRYPT_KEY`、`BACKUP_GROUP_ID`。
**性能**：`WORKER_COUNT`、`QUEUE_CAPACITY`、`BOT_ADD_HANDLER_TIMEOUT_SECONDS`。

## 环境变量（见 .env.example）

必填三件套：`TELEGRAM_BOT_TOKEN`、`JELLYFIN_API_KEY`、`SECURITY_PEPPER`。
管理员：`ADMIN_TELEGRAM_IDS`（与 `SUPER_ADMIN_TG_IDS` 兼容，逗号分隔）。
模板用户权限克隆：`JELLYFIN_TEMPLATE_USER_ID`。

## 机器人交互（面板按钮优先）

发 `/start` 或 `/menu` 打开**主面板**，所有功能通过内联按钮操作：

| 面板按钮 | 说明 |
| -------- | ---- |
| ✅ 每日签到 | 领果果币（连签有加成） |
| 👤 我的账号 | 订阅状态 / 果果币 |
| 🛒 果果币商店 | 购买续期码 / 邀请码 |
| 🎬 追剧中心 | 提交求剧工单 / 查看记录 |
| 🔗 绑定已有账号 | 把已有 Jellyfin 账号关联到 bot（用户名+密码 两步） |
| 📝 注册新账号 | 用邀请码开通新 Jellyfin 账号（邀请码+用户名+密码 三步） |
| 🎟 使用续期码 | 输入续期码为自己的账号续期 |
| ⚙️ 账号管理 | 改密 / 解绑 / 注销 |
| 🌐 查询线路 | 查询管理员配置的 Jellyfin 可用线路 |

> 管理面板不会出现在用户面板中，管理员请使用 `/admin` 调出。

裸文本消息也会弹出主面板；任意面板均有「返回主菜单」，导航原地切换。

## 命令（兼容 / 高级操作）

| 命令           | 说明                                       |
| -------------- | ------------------------------------------ |
| `/start` `/menu` | 打开主面板                              |
| `/signin`      | 每日签到，得果果币                         |
| `/profile`     | 查看账号与订阅                             |
| `/shop`        | 果果币商店（`/shop buy` 购买续期码）       |
| `/redeem <码>` | 核销续期码续期                             |
| `/bind`        | 绑定已有 Jellyfin 账号（两步：用户名 + 密码） |
| `/register`    | 用邀请码注册新 Jellyfin 账号（三步：邀请码 + 用户名 + 密码） |
| `/account`     | 账号自助（改密 / 解绑 / 删号）             |
| `/drama`       | 追剧中心（求剧工单）                       |
| `/admin`       | 管理面板（仅超管）                         |

## 管理面板

- `/admin` —— 打开管理面板（tg_id 必须与 env `ADMIN_TELEGRAM_IDS` 一致，否则提示「您不是管理员，无法使用」）
- `/admin stats` —— 全局统计
- `/admin gencode <数量> [invite|renewal] [天数]` —— 批量生成卡密（命令方式）
- `/admin addpoints <tg_id> <数量>` —— 调整果果币
- `/admin user <tg_id>` —— 查询用户
- inline 回调：`admin:user:enable` / `admin:user:disable` 停用/启用用户

**面板交互式发卡**（推荐，按钮向导）：
- 🎟 生成邀请码：点按钮 → 输入数量（1-200）→ 立刻生成
- ⏳ 生成续期码：点按钮 → 先输入天数（1-3650）→ 再输入数量（1-200）→ 生成

**管理面板其他能力（按钮向导）**：
- 🔍 查询卡密：输入邀请码或续期码 → 显示类型、状态（未使用/已使用/作废）、批次、使用者与使用时间
- 🪙 调整积分：输入 tg_id → 输入变动值（正加负减）
- 👤 查询用户：输入 tg_id → 显示用户名 / Jellyfin / 果果币 / 状态 / 白名单 / 到期
- ✅ 白名单：添加（永久有效、不受规则约束、无需保号）/ 移除 / 查看
- 🏷 卡密定价：设置邀请码 / 续期码所需积分数（运行时即时生效，商店同步）
- 🌐 线路管理：查看 / 添加 / 删除 Jellyfin 线路（用户面板「查询线路」可查）

## 卡密格式

- 邀请码：`mora-` + 20 位大写字母/数字（排除易混淆字符），如 `mora-A1B2C3D4E5F6G7H8J9K2`
- 续期码：`R<天数>-` + 20 位大写字母/数字。`R30-` 表示 30 天续期，如 `R30-A1B2C3D4E5F6G7H8J9K2`
- 卡密明文仅生成瞬间存在于内存；库中只存 HMAC 散列 + XChaCha20-Poly1305 密文


## 安全设计

- 卡密明文仅生成瞬间存在于内存；库中只存 HMAC 散列 + XChaCha20-Poly1305 密文
- 果果币变动一律在事务内（乐观锁 + 禁止负余额）
- 管理员动作全部写 `audit_logs`
- `SECURITY_PEPPER` 用于卡密派生密钥（≥32 字节，勿与备份密钥相同）

## 目录结构

```
cmd/mora/          入口（config → db → jellyfin → tg polling）
internal/config/   .env 配置加载
internal/db/       模型 / Open(AutoMigrate) / 用户 / 签到 / 积分 / 审计
internal/codes/    卡密生成、加密、校验（HMAC + ChaCha20-Poly1305）
internal/jellyfin/ Jellyfin REST API 客户端 + 模板策略克隆
internal/bot/      路由、会话、各命令 handler、卡密兑换/核销
internal/core/     通用工具（时长格式化、输入解析、业务错误）
```

