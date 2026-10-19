# Architecture
```text
Telegram User Bot ------------------┐
                                    │
Telegram Admin Bot (optional) ------┤
                                    v
                              Telegram Gateway
                                    │
                  ┌─────────────────┼─────────────────┐
                  v                 v                 v
             Menu Engine       Session FSM       RBAC/Admin
                  │                 │                 │
                  └──────────┬──────┴──────────┬─────┘
                             v                 v
                       Business Services   Web Admin
                       │    │    │    │         │
                       │    │    │    └ Support │
                       │    │    └ Referral     │
                       │    └ Wallet Ledger     │
                       └ TRC20 Deposit Watcher  │
                             │                  │
                             v                  v
                          MariaDB <---------- Gin API
                             │
                             v
                           Redis
```
## 钱包边界
Bot 进程负责账户展示、充值识别、余额账本、提现申请/冻结、审核状态和业务编排。
真实提现签名建议独立成 Signer/Wallet Service：私钥放 KMS/HSM/独立签名服务，不放 Bot 配置、数据库或 Web 管理后台。
## TRC20 充值
每个用户绑定一个唯一 TRON 充值地址到 `deposit_addresses`。Watcher 按地址读取已确认 TRC20 入账，按 `tx_id` 幂等写入 `deposit_orders` 并增加钱包余额。开启推广后，成功充值会按 `REFERRAL_RATE` 给直属上级发放佣金。
## 提现
`available -> frozen -> pending -> approved -> completed`。
拒绝时 `frozen -> available`。完成时扣减 frozen，并保存链上 TxID。
## 动态菜单
`menu_items` 控制按钮标题、角色、排序、行号、ActionType/ActionValue。修改数据库后无需重启机器人。
支持：
- `builtin`: 内置业务 action，如 wallet/deposit/withdraw/referral/support/admin
- `url`: 外链按钮
- `submenu`: 子菜单

## Message Push

```text
Web Admin
   |
   v
push_campaigns
   |
   v
Push Worker ---- target expansion ----> push_recipients
   |                                  pending/sent/failed
   v
Telegram Bot API
   |
   v
Telegram Users
```

Push Worker 支持立即任务和定时任务。Campaign 进入 processing 后按批次读取 pending recipient，进行限速发送；失败记录错误并自动重试，达到最大次数后标记 failed。管理员可以对失败项重新入队或取消进行中的 Campaign。
