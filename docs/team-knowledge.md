# 团队知识库

团队成员将个人来源中获准共享的原文片段提名给特定项目和用途。项目内双人评审、按条件发布，并保留句子到证据的定位。撤回后可追踪影响范围，恢复旧库时先重播签名日志。 [完整需求与 A1–A14](requirements/08-团队知识-prd.md)

## 用虚构项目试跑

先按根目录 README 准备 MySQL 8.4、签名日志与服务。为虚拟工作室保留一个来源所有人，并用 `provision lab <名称> app 12` 另建编辑员、两位审核员和读者。分别保存令牌。进入工作台的「团队知识」。

1. 来源所有人创建 `shop-floor` 空间及 `operations` 用途，绑定 `editor`、`reviewer`、`reader` 或 `maintainer`。导出是独立成员权限。权限变更携带当前 `policy_rev`。
2. 使用个人工作台给来源应用 `operations` 用途，授权其写入来源。写入虚构句子 `Cedar pump seals are replaced at the north workshop.` 并保留来源回执。来源所有人在团队页提名，填入来源编号、授权版本、到期时间和原文范围。范围以 UTF-8 字节计，起始含、末尾不含。重复提名返回原编号。
3. 以编辑员提交知识草稿，选择已提名证据，主题可填 `pump seals`，适用条件填 `北车间`。每个知识句要有一段当前获授权的证据。两位独立审核员分别核对句段、权限和条件再批准。发现不同地区做法时，拆分适用条件后独立评审。
4. 以读者执行团队搜索，使用 `shop-floor` 和 `operations` 查询 `Cedar pump seals`。打开回执可逐句核对来源、原文定位和授权版本。只有获准导出的成员可以用当前结果生成 JSON。
5. 回到来源所有人身份，预览提名撤回，确认后记录撤回操作编号和索引水位。再用原读者令牌查询，相关知识正文会退出可见结果。使用其他有效来源的知识继续可查。有效替代证据需要重新走审批才能发布。

## API

以下空间内接口都使用 `Authorization: Bearer <令牌>` 和 `/api/team-spaces/{space}` 前缀。带 `purpose` 的读取按用途重新校验成员权限。

| 操作 | 路径 |
| --- | --- |
| 创建或更新空间与策略 | `PUT /api/team-spaces/{space}` |
| 设定成员角色、导出权限 | `PUT /membership` |
| 先检查再提交证据 | `POST /nominations/preview`、`POST /nominations` |
| 提交草稿与读取评审 | `POST /claims`、`GET /claims/{id}?purpose=operations`、`GET /review-queue?purpose=operations` |
| 投票和分派 | `POST /claims/{id}/votes?purpose=operations`、`POST /claims/{id}/assignment?purpose=operations` |
| 召回和导出 | `POST /search`、`POST /export` |
| 更新向量及评测 | `POST /reindex?purpose=operations`、`POST /evaluations`、`GET /evaluations/{id}?purpose=operations` |

`POST /claims` 携带 `Idempotency-Key`、`nomination_ids`、相应 `grant_revisions`、`sentences` 和 `evidence_spans`。首次成功为 201，原请求重放为 200，原键改内容为 409，缺授权材料为 422。全局 `/api/team-nominations/{id}/impact` 提供影响数，`POST /api/team-nominations/{id}/withdraw` 携带幂等键，`GET /api/team-withdrawals/{id}` 汇总索引水位和全部影响数，并返回前 500 条影响明细。个人事实遗忘也写入同一签名水位。 [HTTP 启动验收脚本](../scripts/test-upgrade.ps1)

## 模型、词法搜索与评测

默认支持关键词查询和人工知识草稿。启用团队混合召回时配置 `MEMORY_TEAM_EMBED_URL`（完整的 loopback `/v1/embeddings` 地址）、`MEMORY_TEAM_EMBED_NAME`、`MEMORY_TEAM_EMBED_WEIGHTS_SHA256`、可选 `MEMORY_TEAM_EMBED_TOKEN`。维护员通过工作台重建向量。请求在排序和取证后均以当前权限与水位复核。证据无归纳授权时不能送往模型，无证据的候选需要人工补证。评测保存回执、用例数、用途和输入摘要，评测问题正文从调用方提交。

真实评测使用 Qwen3-Embedding-0.6B Q8_0，llama.cpp b12686 CPU 四线程，`--embedding --pooling last`，权重 SHA-256 为 `06507c7b42688469c4e7298b0a1e16deff06caf291cf0a5b278c308249c3e439`。维护动作是同步的，重建期间安排独立维护时段。 [固定模型版本](https://huggingface.co/Qwen/Qwen3-Embedding-0.6B-GGUF/tree/370f27d7550e0def9b39c1f16d3fbaa13aa67728)

## 验收与备份恢复

Windows 下运行 `pwsh -File scripts/test-mysql.ps1 -MySQLBin <8.4-bin> -Scenario Team` 建立真实 MySQL 的两个独立模式，并在恢复副本重放撤回日志。设定 `MEMORY_TEAM_EVIDENCE` 输出结果。`-Scenario TeamCapacity` 运行 10 万条合成数据。运行 `-Scenario TeamLive` 前按上节启动本地模型，并配置 `MEMORY_TEAM_LIVE_EMBED_URL`、`MEMORY_TEAM_LIVE_EMBED_NAME`、`MEMORY_TEAM_LIVE_EMBED_SHA256` 与可选的 `MEMORY_TEAM_LIVE_ENGINE`。 [评测与逐请求证据](validation.md#v200-团队知识)

运行 `-Scenario Upgrade -PreviousBinary <v1.1.0-agent-memory.exe>` 可以重放实际旧程序。若同时验收浏览器，把 `MEMORY_TEST_BROWSER_SCRIPT` 指向 `scripts/team-browser.cjs` 的绝对路径，`MEMORY_TEST_PLAYWRIGHT` 指向本地已安装的 Playwright 模块，`MEMORY_TEST_CHROME` 指向 Chrome 或 Chromium。

从 v1.1.0 升级前保留原 MySQL 备份、加密密钥和独立签名日志。停止旧服务后部署新程序并运行 `agent-memory migrate`，用原密钥和最新日志重新启动。以旧备份恢复时须在隔离环境核对签名水位、历史个人数据、团队撤回与样例权限，再切换业务。此前的个人来源由所有人显式提名。 [运维与恢复](operations.md)
