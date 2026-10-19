# Production Checklist
1. Web 管理后台不要直接暴露公网，优先放在 VPN / Zero Trust / SSO 后面；后台使用 Redis 会话登录（12 小时有效期），支持主动退出。HTTPS 反向代理应设置 `X-Forwarded-Proto: https`，以启用 Secure Cookie。
2. 修改 `ADMIN_WEB_PASSWORD`，Webhook 必须配置高强度 secret token。
3. 数据库使用独立账号，仅授予业务库权限；MariaDB/Redis 不对公网开放。
4. TRON 私钥不要写 `.env`、不要写数据库、不要交给 Bot。使用独立 Signer/KMS/HSM。
5. 提现加入：限额、地址黑名单、频控、二次确认、人工审核、幂等键、风控规则。
6. 充值地址建议一用户一地址；不要用一个公共地址直接自动归属多个用户，除非有额外可验证的归属机制。
7. TRC20 Watcher 使用持久化扫描进度、完整分页和 48 小时重叠回查，并依赖 txid 唯一键幂等；遇到更晚的历史补录，可通过后台执行历史补扫。
8. 为管理员操作增加独立审计表、IP、UA、请求 ID 和不可变审计存储。
9. AutoMigrate 适合开发；生产改为版本化 migration。
10. 加 Prometheus、OpenTelemetry、Sentry/日志告警、数据库备份、Redis 持久化监控。
11. Webhook 前置 Nginx，限制 body 大小、连接数、请求频率，只开放必要路径。
12. 链上广播前必须再次验证订单状态，使用数据库事务/分布式锁确保同一提现只能广播一次。

## 逻辑修复版本的升级与验证

- 先停止旧版本的 Bot/Worker，再启动新版本，避免旧 Worker 绕过新的防重与任务锁。
- 启动时 AutoMigrate 增加提现 `request_id`（允许历史 NULL、唯一索引）、充值地址 `scan_timestamp`、收件人 `media_sent`、工单投递状态，以及 `referral_grants`、`seed_migrations` 表。不会删除或重新计算历史余额、佣金。历史上已发生的重复入账应另行对账；新防重机制不会自动冲销它们。
- 用户与管理机器人的会话现在隔离；升级前未完成的会话需要重新开始。提现仅支持 TRC20，金额最多六位小数，并验证 Base58Check 地址。
- 默认菜单只在初始化时创建，删除后不会随重启恢复。已有菜单的升级会保留当前列表。后台可以编辑父菜单、启用状态和说明。
- 配置优先采用进程环境变量，`.env` 可选。补齐 `.env.example` 中的 `UPLOAD_*` 配置；Compose 持久化上传目录。Webhook 模式必须提供对应机器人的 URL 和 secret。
- 首次扫描从历史开始；后续从持久化进度向前重叠 48 小时扫描全部分页。进度仅在入账和佣金处理成功后推进，停机不会使扫描窗口跳到当前时间。后台充值地址的“历史补扫”可重置进度。只能绑定确认归属的充值地址，历史未入账交易也会被扫描。
- 取消推送会保留已经发送的结果，阻止后续投递；取消时已在 Telegram 请求中的一条消息可能仍然送达。仅终态任务可重试失败项，原先未发送的取消项不会重新发送。Worker 与手动重试共用可续租的 Redis 锁。
- 图片/视频与正文分开发送时保存媒体进度，正文重试不会再次发送已记录成功的媒体。Telegram API 与本地数据库不属于一个事务：远端已接收但响应丢失、或进程在远端成功后本地记录前退出，仍存在结果不确定窗口，不能承诺严格 exactly-once。
- 工单回复先保存内容、再发送；失败保留待处理状态和错误，可从后台回复按钮重试，不会把失败发送标成已回复。

验证命令：

```bash
go test ./...
go test -race ./...
go vet ./...
```

普通测试使用模拟 HTTP/SQL，不连接实际 Telegram、TronGrid 或业务库。真实并发资金测试需单独提供 `TEST_MYSQL_DSN`，指向隔离测试服务器，**不带数据库名**。该测试只创建并删除其唯一命名的 `tg_bot_test_*` 数据库，不读取 `.env`：

```bash
TEST_MYSQL_DSN='test_user:test_password@tcp(127.0.0.1:3306)/?parseTime=true' go test ./integration -v
```

## 多机器人版本升级

新增 `business_bots`、`bot_subscriptions`、`push_routes` 表；推送任务增加机器人渠道、业务类型、自动称呼开关、暂停前状态与错误说明；工单增加来源机器人。旧任务的渠道为默认机器人、自动称呼关闭，避免改变历史重试内容。升级前需停止旧 Worker，完成迁移后再启用新 Worker。

启用的新增机器人每 3 秒检查配置变化，使用可续租 Redis 锁保证单个机器人只有一个轮询器。加密 Token 不进入 API 返回值；Telegram 网络错误会移除 Token 后记录。密钥需在各实例保持一致，轮换前应准备重新录入所有机器人 Token。

暂停能阻止后续收件人的发送，但不能撤销正在进行的 API 请求。历史任务删除与 Worker 共用任务锁；任务尚在执行时删除会返回冲突，请稍后重试。后台删除只清理本地任务及其投递明细，不调用 Telegram 删除消息接口。

### 周期推送与会话登录升级

停掉所有旧 Worker 后再启动新版，避免旧 Worker 将周期任务视为单次任务结束。AutoMigrate 为 `push_campaigns` 增加 `repeat_seconds`（默认 0）及 `parent_id`（默认 0），历史任务保持单次行为。完成轮次的归档、收件人归属变更和下一轮重置在同一数据库事务内执行；请保留数据库备份。

后台从 Basic Auth 切换为登录表单和 Redis 会话。旧 API 调用方须改为 Cookie 登录，并为写入请求增加 `X-Requested-With: admin-console`。Redis 不可用时登录及受保护接口返回 503，退出失败也会明确提示，恢复 Redis 后重试。退出撤销当前会话；其他设备会话各自独立。代理应保留 Cookie，并在 HTTPS 入口覆盖 `X-Forwarded-Proto` 为 `https`。
