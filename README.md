# Agent Memory

供多个智能助手共享受控用户记忆的 Go 服务。助手按用户授权和指定用途写入会话。用户审核候选、纠正已有偏好、撤销授权、分享某个版本或遗忘事实。MySQL 提供生效记录、历史版本和来源。独立签名日志在从旧备份恢复时重新实施遗忘与撤权。

## 功能

- 租户和助手身份、用途授权、失效时间、显式自动采信及模型授权。
- 来源及时点去重、带回执的来源冲突队列、敏感文本拦截、引用识别、双份证据裁决。
- 独立用途下的事实版本、来源归因、乱序保护、历史查询与有限关键词排序。
- 本人资料、纠正、待审、用途授权、分享、遗忘和脱敏审计工作台。
- 删除提交前落盘签名水位，重试返回原回执，旧库开放读取前自动重播。
- 可选 OpenAI 兼容模型，异步任务可按编号查询和重试，模型提议附可核对的原文片段，交给本人确认。

## 快速运行

准备 Go 1.26 或发布页的 Linux/Windows 程序，以及专用 MySQL 8.4 数据库。让 `MEMORY_MYSQL_DSN` 指向该库。将 `MEMORY_KEY_FILE` 和 `MEMORY_JOURNAL` 指向访问受限且有独立备份的持久目录。

```shell
go build -o agent-memory ./cmd/agent-memory
./agent-memory migrate
./agent-memory keygen
./agent-memory journal-init
./agent-memory provision lab riley owner 12
./agent-memory provision lab concierge app 12
./agent-memory provision lab storefront app 12
./agent-memory serve
```

打开 `http://127.0.0.1:8105/`，使用三个身份各自的令牌。以用户身份给两个应用授予 `support` 用途，给 concierge 写入权限，两个应用都给读取权限。应用用虚构文本 `Remember my notifications as email.` 写入，用户审核后，两个应用均可查到通知偏好。再用用户工作台纠正为 `sms`，核对当前与历史时点，以及撤销授权后对历史查询的拦截。

使用已授权的共享令牌可以访问一个指定版本，链接到期、撤回、事实被替代或相关权限代际改变时停止访问。

[接口与恢复](docs/operations.md) · [验收记录](docs/validation.md) · [产品文档](docs/requirements/01-产品总览-prd.md) · [Apache-2.0](LICENSE)
