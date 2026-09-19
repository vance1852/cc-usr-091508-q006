# 推进剂样品链路服务（samplechain）

面向火箭试车前采样班组与化验室的样品责任链服务：记录液氧等推进剂样品从
**采集封存 → 转运交接 → 化验检测 → 复核放行** 的完整链路，在封签破损、
温度越界、温度空白、编号冲突时形成不可消除的偏差并暂停后续使用。

- 语言/框架：Go 1.23 + [chi v5](https://github.com/go-chi/chi)
- 存储：SQLite（`github.com/mattn/go-sqlite3`，CGO 链接系统 libsqlite3）
- 第三方依赖已整体放入 `vendor/`，SQLite 头文件放入 `third_party/cgo-include/`，
  无网络也可构建（见“构建与运行”）。

## 业务规则（如何被系统强制）

| 规则 | 实现方式 |
| --- | --- |
| 每次接收必须承接上一位保管人的确认 | 两阶段交接（release/receive）；接收时校验在途交接的交出人 == 链上当前保管人 |
| 链路中任一时刻只有一名当前保管人 | `samples.current_custodian_id` 单值；在途交接部分唯一索引 `ux_transfer_open`；写事务 `BEGIN IMMEDIATE` 串行化，并发第二笔交接直接 409 |
| 采集瓶、转运箱、实验室编号被不同班组重复登记 | 登记事务内逐项校验，冲突即拒绝创建样品，并追加 `number_conflict` 偏差与登记留痕（HTTP 409 带冲突明细） |
| 容器再次使用 | 归还（return）解除绑定后才能绑定新样品；未归还时容器被部分唯一索引占用 |
| 封签破损 | 接收必须显式声明 `seal_intact`；破损则追加 `seal_broken` 偏差、责任不转移、样品暂停 |
| 温度越界 / 记录空白 | 按登记时固化的方法区间与最大间隔判定，追加 `temp_excursion` / `temp_gap` 偏差；支持离线补传后跨批次合并判定 |
| 偏差不能靠改写记录消失 | 事实表（交接、温度、偏差、处置、结果、读数、状态事件、登记留痕）均有 SQLite 触发器禁止 UPDATE/DELETE；偏差关闭只能追加处置 |
| 已发布结果的纠正 | 旧版本不可变；只能提交 `supersedes_id` 指向旧版本的新复测版本，同状态机重新走复核 |
| 采样班组不能批准自己的检测 | 复核人所属采样班组 == 样品采样班组时拒绝；检测人不能复核本人提交的结果 |
| 试车审批人只读完成复核的结论 | `/approval` 仅呈现 `reviewed` 且被样品采用（`current_result_id`）的版本 |
| 外部实验室看到脱敏批号 | 批号 SHA-256 前 12 位单向脱敏；外部视图不含内部实验室编号、班组、人员、箱号 |
| 离线扫码恢复 | 离线交接事件携带客户端 `idem_key`，重复同步只生效一次；事件照样进入哈希链 |
| 防物理篡改 | 交接事件以 `prev_hash/hash`（SHA-256）成链，下钻视图重算校验 `chain_intact` |

## 角色

`sampler`（采样员，归属班组）、`analyst`（化验员）、`reviewer`（复核人）、
`approver`（试车审批人）、`external`（外部实验室）、`admin`。

认证使用不记名令牌：`Authorization: Bearer <token>`。

## HTTP 接口（前缀 `/v1`）

| 方法 | 路径 | 角色 | 说明 |
| --- | --- | --- | --- |
| POST | `/samples/register` | sampler | 登记样品（瓶/箱/实验室编号/封签/采样点/方法） |
| POST | `/samples/{labNo}/release` | sampler, analyst | 发起交接，指定唯一接收人（责任不转移） |
| POST | `/samples/{labNo}/receive` | sampler, analyst | 指定接收人确认承接，必带 `seal_intact`；可 `reject_transfer` |
| POST | `/samples/{labNo}/recover/release` | sampler, analyst | 离线恢复交出，必带 `idem_key`（重放幂等） |
| POST | `/samples/{labNo}/recover/receive` | sampler, analyst | 离线恢复接收，必带 `idem_key`（重放幂等） |
| POST | `/samples/{labNo}/return` | sampler, analyst | 归还容器，解除占用以便复用 |
| POST | `/shipments/{boxCode}/close` | sampler | 关闭转运箱发运（箱内样品须均已归还） |
| POST | `/samples/{labNo}/temperatures` | sampler, analyst | 追加温度记录（可批量、可离线 `source=offline`） |
| POST | `/results` | analyst | 提交结果版本（含原始读数，可 `supersedes_id` 引用旧版本） |
| POST | `/results/{id}/review` | reviewer | 复核：`{"approve":bool,"conclusion":...}` |
| POST | `/deviations/{id}/disposition` | reviewer | 偏差结案：`reject_sample`/`retest`/`accept_justified` + 理由 |
| GET | `/deviations` | reviewer, approver | 偏差清单 |
| GET | `/samples/{labNo}/trace` | reviewer | 单样品完整链路下钻 |
| GET | `/batches/{code}/trace` | reviewer | 批次下钻：逐站去向、异常处置、采用的结果版本、`chain_intact` |
| GET | `/batches/{code}/approval` | approver | 放行视图（仅完成复核并采用的结论可放行） |
| GET | `/batches/{code}/masked` | reviewer | 取批次的对外脱敏批号 |
| GET | `/external/batches/{masked}` | external | 脱敏批次视图 |
| GET | `/healthz` | 无 | 健康检查 |

错误以 400/403/404/409 返回，冲突类 409 附带 `conflicts` 明细。

## 构建与运行

需要 Go 1.23、gcc 与运行时的 `libsqlite3.so.0`（Debian/Ubuntu 由 libsqlite3-0 提供）。
若 Go 不在 PATH，可用环境变量 `GOROOT` 指定。

```bash
# 构建（首次会用系统 libsqlite3.so.0 生成 third_party/lib 下的开发链接，该目录不入库）
./scripts/build.sh

# 测试
./scripts/test.sh
./scripts/test.sh -race

# 初始化演示数据并启动
./bin/samplechain -db samplechain.db -addr :8080 -seed
```

演示账号（令牌即 `tok-<login>`）：

| 登录 | 角色 | 班组 |
| --- | --- | --- |
| zhang / li / wang | sampler | A / B / C |
| analyst1 | analyst | — |
| reviewer1 | reviewer | — |
| approver1 | approver | — |
| extlab | external | — |
| admin | admin | — |

示例：

```bash
curl -sS -X POST localhost:8080/v1/samples/register \
  -H "Authorization: Bearer tok-zhang" -H "Content-Type: application/json" \
  -d '{"batch_code":"FIRE-2026-009","box_code":"BOX-100","lab_no":"LOX-LAB-100",
       "container_code":"BTL-001","seal_no":"SEAL-100","point_code":"TANK-01",
       "method_code":"LOX-PURITY","sampled_at":"2026-09-19T08:00:00Z"}'
```

## 代码结构

```
cmd/samplechain/      程序入口（serve/seed）
internal/store/       SQLite schema、全部业务不变量与查询
  schema.go           表结构 + 只追加触发器
  register.go         登记、编号冲突、容器/箱/封签唯一性
  custody.go          两阶段交接、封签异常、容器归还
  temperature.go      温度越界与空白判定
  deviations.go       偏差与处置（职责分离）
  results.go          结果版本、复测引用、复核状态机
  queries.go          下钻与哈希链校验
  views.go            审批视图、外部脱敏视图
internal/api/         chi 路由、认证、角色中间件、HTTP 处理器
internal/seed/        演示基础数据
third_party/cgo-include/  仓库自带 sqlite3.h
vendor/               chi 与 go-sqlite3 源码（离线可构建）
```

## 不可变性的边界

应用账号始终不使用具备绕过触发器能力的连接；触发器对所有 UPDATE/DELETE 生效。
拥有数据库文件 shell 权限的人理论上可以 `DROP TRIGGER` 后直接改库——这属于
部署侧应通过文件权限/审计解决的问题；本服务保证的是**通过服务接口和正常数据库
会话无法静默改写历史**，且任何对链上事件哈希域内字段的物理改动都会在下钻时
表现为 `chain_intact=false`。
