# 推进剂样品链路服务（samplechain）

面向火箭试车前采样班组与化验室的样品责任链服务：记录液氧样品从**采集、封存、转运到检测**的
完整链路，在封签破损、温度越界、温度记录空白或编号冲突时自动形成偏差并暂停后续使用，
为复测纠正、偏差处置和放行评审提供不可改写的依据。

- 语言/框架：Go 1.19 + [chi](https://github.com/go-chi/chi/v5) 路由
- 存储：SQLite（`mattn/go-sqlite3`，CGO；单连接 + `BEGIN IMMEDIATE` 序列化写入）
- 鉴权：`Authorization: Bearer <token>`，角色绑定人员与班组

## 核心不变量

| 需求 | 落地方式 |
| --- | --- |
| 每次接收必须承接上一保管人的确认 | 交接两阶段 `release → receive`；接收必须回传扫码得到的 `release_code` 且本人是被交接人 |
| 封签破损 / 温度越界 / 温度空白 / 编号冲突 | 接收/登记事务内自动开**偏差**并置 `hold=1`，暂停交接、消耗与采用 |
| 偏差不能靠改写记录消失 | 偏差与处置事件只增；四张事件表由 SQLite 触发器封禁 `UPDATE/DELETE`；只能靠追加处置事件推进（纠正/关闭/拒收） |
| 只能有一名当前保管人 | `samples.current_holder_id` 单行 + `version` CAS；`transfers` 对在途记录建样品/容器**部分唯一索引**；在途期间保管人仍是交出人 |
| 离线扫码恢复 | 所有写操作带 `op_id` 幂等台账（`Idempotency-Key` 头）：同键同载荷重放返回首执结果，同键换载荷 409；客户端可带 `expected_version` 乐观锁 |
| 容器再次使用 | 容器 `active_sample_id` 在用时禁止装新样；样品消耗或拒收后释放，方可复用 |
| 已发布结果只能纠正、不能改 | `results` 版本化只增，原始读数永不修改；复测以 `supersedes_version` 引用旧版本 |
| 采样班组不能批准自己的检测 | 复核人角色必须为 `reviewer` 且班组 ≠ 采样班组，否则 403 |
| 试车审批人只读完成复核的结论 | 审批人仅可访问 `/conclusions`（只投影已复核并采用的版本，并显式暴露 hold/void），不能下钻含原始读数的完整链路 |
| 外部实验室只见脱敏批号 | 外部角色只能访问 `/external/*`，凭 `public_code` 与方法名协作；回传结果仍须内部跨班组复核 |
| 评审逐站下钻 | `GET /v1/batches/{id}/chain`：采样点 → 样品 → 保管事件 / 偏差处置链 / 各结果版本与当前采用版本 |

样品状态机：`active → consumed`（化验消耗，释放容器）/ `active → void`（偏差拒收）；
`hold` 是与状态正交的暂停位，最后一个 open 偏差结案后自动解除。

## 快速开始

```bash
make build                 # 需要 CGO（gcc）；产出 bin/samplechain
make run                   # 默认 :8080，数据库 samplechain.db
make test
```

环境变量：`SAMPLECHAIN_DB`（数据库路径）、`SAMPLECHAIN_ADDR`（监听地址）。

空库自动写入演示种子（人员、令牌、气相色谱法）：

| 角色 | 人员 ID | 令牌 |
| --- | --- | --- |
| 化验室管理员 | p-admin | token-admin |
| 采样员（一组/二组） | p-sampler-a / p-sampler-b | token-sampler-a / token-sampler-b |
| 押运员 | p-courier | token-courier |
| 化验员（一组/二组） | p-analyst-a / p-analyst-b | token-analyst-a / token-analyst-b |
| 复核员（质量组） | p-reviewer | token-reviewer |
| 试车审批人 | p-approver | token-approver |
| 外部实验室 | p-external | token-external |

## API（/v1）

所有写接口接受 `Idempotency-Key` 头（离线客户端必传，在线不传则服务端生成）。

**档案（lab_admin）**
- `POST /people`、`POST /people/{id}/tokens`
- `POST /batches`、`POST /batches/{id}/points`、`PUT /batches/{id}/temp-limits`
- `POST /methods`

**采集与编号**
- `POST /containers`（sampler/lab_admin）
- `POST /containers/{id}/registrations` — body `{scope: collection|transit|lab, code, sample_id?}`；编号冲突返回 `409` 且样品已挂偏差暂停
- `POST /samples`（sampler）— `{public_code, batch_id, point_id, container_id, seal_no, seal_intact}`

**保管链**
- `POST /samples/{id}/release` — `{to_person_id, seal_intact, expected_version?}`，返回 `release_code`
- `POST /transfers/{id}/receive` — `{release_code, seal_intact, seal_no?, temp_min?, temp_max?}`；省略温度即记录空白；封签/温度异常照常换保管人但样品 `hold=true`
- `POST /transfers/{id}/cancel`（交出人）
- `POST /samples/{id}/consume`（analyst/lab_admin，释放容器）

**偏差处置（lab_admin）**
- `POST /samples/{id}/deviations` — 人工登记偏差（reviewer/lab_admin）
- `POST /deviations/{id}/dispositions` — `{action: investigate|corrective_action|resample|close|reject, note}`

**检测与结论**
- `POST /samples/{id}/results`（analyst）— `{method_id, raw_reading, purity, supersedes_version?}`
- `POST /results/{id}/review`（reviewer，跨班组）— `{approve, note?}`
- `POST /samples/{id}/adopt`（lab_admin）— `{version}`；仅可采用 `reviewed` 版本且样品无 open 偏差

**读取**
- `GET /samples/{id}` — 当前态与完整事件/偏差/版本（内部角色）
- `GET /batches/{id}/chain` — 评审逐站下钻（reviewer/lab_admin）
- `GET /batches/{id}/conclusions` — 放行结论投影（reviewer/lab_admin/**approver**）

**外部实验室（external_lab）**
- `GET /external/orders` — 仅脱敏批号 + 方法名
- `POST /external/results` — `{public_code, method, raw_reading, purity}`，生成待内部复核的外部版本

错误响应统一为 `{"error":{"code","message"}}`：401 未认证、403 角色/班组越权、
404 不存在、409 保管/状态/编号/版本冲突、422 入参校验失败。

## 典型流程

```
采集(sampler, hold?) → release(保管人) → receive(被交接人+确认码, 封签/温度校验)
  → 化验提交 v1(submitted) → 跨班组 reviewer 复核(reviewed)
  → lab_admin 采用 v1 → 审批人读 /conclusions
纠正：提交 v2(supersedes_version=1) → 复核 → 采用 v2（v1 原样保留）
异常：receive 自动开偏差 → hold → 处置(纠正/关闭解除，或拒收作废释放容器)
```

## 代码结构

```
cmd/server/            入口（配置、建库、种子、HTTP 启动）
internal/store/        SQLite schema 与全部事务不变量
  store.go             连接/迁移/表结构/触发器/错误码
  ops.go               人员、批次、采样点、容器、采集
  chain.go             编号登记、release/receive/cancel/consume
  deviations.go        偏差处置
  results.go           结果版本、复核、采用、外部委托
  query.go             下钻链路与结论投影
  helpers.go models.go seed.go
internal/api/          chi 路由、令牌鉴权、角色授权、处理器
*_test.go              存储层不变量测试 + HTTP 端到端测试（含并发与竞态检测）
```
