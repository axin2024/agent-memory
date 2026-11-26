# 运行、集成与恢复

## 单实例运维

每个 MySQL 模式运行一个服务实例，持有模式级 MySQL 会话锁和日志文件锁。签发本地账号需在启动前或停机窗口执行。应用只使用期限内的 Bearer 令牌。生产环境用 TLS、受限数据库账号、受控入口及速率限制。日志文件和密钥存放在独立访问控制的持久卷，Windows 用 ACL，Linux 目录权限设为 `0700`。异步提取由单个后台进程依次处理。单次模型请求最多等待 8 秒，期间服务继续处理读取、授权和遗忘。

在完整持久卷保存最新 `MEMORY_JOURNAL`，备份加密且与 MySQL 的常规还原点独立留存。备份密钥和日志必须匹配。每个模式绑定一个日志身份，日志缺失、校验链损坏、日志落后数据库或已有观察到的删除无法清理时，服务拒绝启动或进入恢复状态。日志上限 128 MiB，单事实遗忘最多记录 20,000 项已知来源摘要。上线前按数据保留与写入规模规划日志容量。

## 业务接口

除公开分享请求之外，`/api/` 使用 `Authorization: Bearer …`。JSON 字段以 `internal/memory` 和 `internal/web/web.go` 为准。

| 路径 | 操作 |
| --- | --- |
| `GET /api/session` | 获取当前角色和范围 |
| `POST /api/consent` 与 `/api/consent/revoke` | 用户授权、撤权，用途与期限 |
| `POST /api/source` | 应用登记主体、用途、会话、来源号、发生时间、`kind`、文本 |
| `POST /api/extract` | 对有单独模型授权的保留原文提议候选 |
| `POST /api/extract/jobs` 与 `GET /api/extract/jobs?id=任务编号` | 提交异步提取并按编号查询进度，失败后用原三项参数重试 |
| `GET /api/pending` 与 `POST /api/review` | 本人核对证据和当前版本后裁决 |
| `POST /api/review/batch` | 本人一次裁决最多 50 项，返回逐项状态与错误 |
| `GET /api/source-conflicts?after=0` | 本人按回执顺序查看待处理的来源冲突，每页最多 50 条 |
| `POST /api/source-conflicts/dismiss` | 本人以 `{"id":"123"}` 确认保留原来源，同一回执可安全重试 |
| `POST /api/edit` | 本人用预期版本纠正，重复幂等键回读原回执 |
| `POST /api/recall` | 主体、用途、可选事实键、关键词与历史时点 |
| `GET /api/export` | 按本人有效授权和当前值导出 |
| `POST /api/forget` | 指定事实键或明确设置 `all: true`，必须使用操作键 |
| `GET /api/forget?id=操作编号` | 本人核对日志回执和已完成的清理水位 |
| `POST /api/share`、`/api/shared`、`/api/share/revoke` | 签发、读取、撤回一个限定版本的分享 |
| `GET /api/audit?after=0` | 以游标查看脱敏事件摘要 |
| `PUT /api/policy` 与 `POST /api/sweep` | 管理员缩短租户保留范围并清理过期正文 |

`/api/source` 的 `kind` 支持 `user`、`quotation` 和 `transient`。外部来源号在同租户、用户、应用、会话下幂等。相同来源收到不同内容会返回 `409`。原来源仍在留存期内时，回执包含 `conflict_id`。重试同一次冲突会拿到同一编号和 `replayed: true`。队列保留原来源的定位信息与经密钥计算的摘要，不保存被拒内容。留存到期后队列条目不可见，清理任务删除对应记录。遗忘原来源时一并清除队列，旧来源返回 `410`。

事实候选审核会并列展示新增提议及同用途下仍有效的旧记录。证据片段随来源保留期清理。`confidence` 描述提议的证据类型，`explicit-preference` 对应显式长期偏好规则，`evidence-matched` 对应匹配原文的待审模型提议，`owner-confirmed` 对应用户直接改写。

规则识别英文 `Remember my notifications as sms.` 和中文 `我的长期偏好：notifications=短信`。敏感模式过滤用于减少误采集，接入方继续负责用户知情和源数据的脱敏。

应用对写入来源分类负责。部署方显式配置 `MEMORY_MODEL_URL` 与 `MEMORY_MODEL_NAME`，可选 `MEMORY_MODEL_TOKEN`。只在用户同意的用途下向模型发送当前应用保留的单条文本。HTTPS 和本机回环受支持。相同来源与模型共用一个任务号。进程中断后，同一模型的后台进程会恢复未领取或租约已到期的任务。推理期间撤权或遗忘，结果会在落库前丢弃。任务页记录尝试次数、耗时和供应方返回的 Token。模型原始响应和凭据不写入审计。合适的模型与业务词汇需按你的数据重新验证。

## 从旧备份恢复

1. 停止读写流量，保全最新的独立签名日志与对应密钥，记录水位。用 `mysqldump --single-transaction --hex-blob` 备份 MySQL。
2. 将历史数据库快照还原到隔离模式。配置恢复进程使用最新日志与对应密钥。运行 `agent-memory migrate` 补齐当前版本的数据库结构，再用 `agent-memory recover` 验证签名链、模式日志身份、已应用序号与完整回放。
3. 回读应用和本人当前与历史召回，发送已删除来源的相同会话与编号，检查 `410`。查询已撤销的分享和授权。
4. 验证结果后切换到恢复副本，原实例保持下线。日志落盘到数据库提交之间若中断，重启会先重播。发现物理损坏时保留原文件与备份，由维护人员核对完整签名链及最近可信水位。

逻辑删除和回放控制服务及恢复副本的可见性。数据库旧备份、已有导出、被分享内容及第三方模型的留存由各自的数据保管策略处理。
