# 模型网关（Model Relay）

用 Go + React 实现的模型网关：统一接入多家上游（DeepSeek、智谱、MiniMax、火山方舟 Seedance 及任意 OpenAI 兼容服务），
对外提供一套 `/v1` 接口，并带有中文控制台。业务数据全部保存在一个 SQLite 文件中，单个二进制即可运行。

## 快速开始

```bash
make build          # 构建前端并编译出 bin/gateway（内嵌控制台）
./bin/gateway       # 默认监听 :3000，打开 http://localhost:3000 ，测试账号 admin / admin
make test           # 后端自动化测试（使用受控假上游，不产生真实费用）
```

配置全部来自环境变量，也可以写在工作目录的 `.env` 里（已被 `.gitignore` 忽略）：

| 变量 | 说明 | 默认值 |
|---|---|---|
| `SQLITE_PATH` | 数据库文件。首次启动自动建库并执行迁移，之后沿用同一文件 | `data/gateway.sqlite` |
| `ADDR` | 监听地址 | `:3000` |
| `PROVIDERS_ENV_FILE` | 存放上游凭证的私有文件路径（`KEY=VALUE`），只注入服务端进程 | 空 |
| `DEEPSEEK_API_KEY` / `BIGMODEL_API_KEY` / `MINIMAX_API_KEY` / `ARK_API_KEY` | **仅在全新数据库上**用于自动创建初始渠道 | 空 |
| `DATA_DIR` | 媒体成品目录 | 数据库同目录下的 `<库名>_media` |
| `DEFAULT_MAX_TOKENS` | 有限额度 Key 未指定 `max_tokens` 时注入的受控上限 | `4096` |
| `UPSTREAM_TIMEOUT_SECONDS` / `STREAM_IDLE_TIMEOUT_SECONDS` | 等待上游响应头 / 流式空闲的超时 | `300` / `120` |
| `PENDING_REVIEW_TTL_SECONDS` | “待核实”预留超期后自动按上界结算的期限 | `86400` |
| `MAX_ATTEMPTS` | 单次请求最多尝试的渠道数 | `3` |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | 首次启动创建的管理员 | `admin` / `admin` |

说明：当前部署范围是单节点，限流计数保存在进程内存，账务保存在 SQLite，因此没有使用 Redis 和消息队列
（《环境说明》中二者均为“按需”）。上游凭证保存在 SQLite 的渠道表中，接口只返回掩码，不会下发到浏览器，也不写日志。

## 目录结构

```
cmd/gateway            入口：/v1 网关接口 + /api 管理接口 + 内嵌控制台
cmd/mockupstream       受控假上游（回归与账务边界验证用，不能代替真实验收）
internal/gateway       全部后端逻辑
  db.go                SQLite 与迁移（只追加，不修改旧迁移）
  auth.go              管理员会话、网关 Key 鉴权（Bearer 或 控制台会话+Key ID）
  pipeline.go          调用主链路：权限 → 限流 → 在途名额 → 预留 → 渠道尝试 → 结算 → 日志
  limiter.go           按 Key 的 RPM 与在途请求数上限
  billing.go           额度预留、结算、待核实、重启恢复
  chat.go              聊天（流式/非流式）与 Embeddings
  media.go             视频、语音、长文本配音、音乐、音色复刻/设计、任务轮询
  admin.go             渠道、模型、Key、日志、导出、用量、账务核实
web/                   React + TypeScript 控制台（Vite）
scripts/verify_restart.sh  “杀进程重启”账务恢复验证
docs/验证记录.md        真实上游与受控上游的验证证据
```

## 网关接口（调用方）

所有接口使用 `Authorization: Bearer <网关 Key>`，响应头 `X-Request-Id` 是请求标识，可在控制台“调用日志”按它精确查询。

| 接口 | 说明 |
|---|---|
| `GET /v1/models` | 该 Key 可用的模型 |
| `POST /v1/chat/completions` | 聊天，OpenAI 兼容，支持 `stream` |
| `POST /v1/embeddings` | 文本向量，OpenAI 兼容 |
| `POST /v1/video/generations` | 视频任务：`model`、`prompt`、可选 `first_frame_image`（URL 或 data URI）、`duration`、`resolution` |
| `POST /v1/audio/speech` | 文本转语音：`model`、`input`、`voice`、`speed`；`stream: true` 时边生成边返回 MP3 |
| `POST /v1/audio/speech/async` | 长文本配音（后台任务） |
| `GET /v1/audio/voices` | 系统音色与自定义音色 |
| `POST /v1/audio/voices/clone` | 音色复刻（multipart：`file`、`model`、`consent=true` …） |
| `POST /v1/audio/voices/design` | 音色设计：`prompt`、`preview_text` |
| `POST /v1/music/generations` | 音乐生成（后台任务）：`prompt`、`lyrics` |
| `GET /v1/tasks`、`GET /v1/tasks/{id}`、`GET /v1/tasks/{id}/content` | 任务列表、状态、成品下载 |

```bash
curl http://localhost:3000/v1/chat/completions -H "Authorization: Bearer sk-relay-…" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-flash","messages":[{"role":"user","content":"你好"}],"max_tokens":200}'
```

错误体为 OpenAI 风格：`{"error":{"message","type","code","request_id"}}`。常见 `code`：
`invalid_api_key`(401)、`model_not_allowed`(403)、`price_not_configured`(403)、`insufficient_quota`(402)、
`rate_limit_exceeded`(429)、`concurrency_limit_exceeded`(429)、`upstream_error`(502)、`upstream_timeout`(504)。

## 控制台

| 页面 | 功能 |
|---|---|
| 概览 | 今日请求/费用、近 7 天趋势、待核实提醒 |
| 渠道 | 新建/编辑渠道；**从上游获取模型列表并勾选导入**，也可手动填写；支持模型别名（对外名 → 上游名）；获取失败时保留已有配置 |
| 模型 | 类型、计费方式与价格、输出上限；**收藏 / 取消收藏 / 仅看收藏**（保存在服务端，下次打开仍在） |
| 网关 Key | 额度（有限/无限）、RPM、在途请求数上限、模型权限、过期时间；完整 Key 只在创建时显示一次 |
| 聊天 Playground | 多轮对话、系统提示词、温度、最大输出、流式、停止生成、清空、重新发送、**导出 Markdown**；**选择已有 Key** 或手动输入 |
| 视频生成 | 选择厂商（MiniMax / Seedance），文生视频与首帧图生视频；任务状态、预览、下载 |
| 语音与音色 | 文本朗读（系统音色、试听、下载）、**流式朗读**（边生成边播放、可停止）、**长文本配音**（后台任务，刷新后仍在）、**音色复刻**、**音色设计** |
| 音乐生成 | 风格描述 + 歌词，试听与下载 |
| 调用日志 | **按请求 ID 精确查询**、筛选、详情（含每次上游尝试与预留）、**导出调用明细 CSV** |
| 用量与费用 | 所选时间段的**费用合计**（与翻页无关）与**按模型汇总** |
| 账务核实 | 处理“费用待核实”的预留：按核实金额结算，或确认未产生费用并释放 |

右上角可切换**浅色 / 深色 / 跟随系统**主题，选择保存在浏览器本地，覆盖所有页面和弹窗（提示与确认均为自绘组件，不使用浏览器原生弹窗）。

### Playground 使用说明

1. 在页面顶部选择一个已有的网关 Key（只把 Key 的 ID 发给服务端，密钥不经过浏览器），或手动粘贴一个 Key。
   手动粘贴的 Key 只保存在页面内存里：不写入地址栏、localStorage/sessionStorage 或日志，刷新后需要重新输入。
2. 模型下拉框只列出该 Key 有权访问的聊天模型。温度和最大输出受所选模型配置的范围约束；模型不支持温度时会禁用并说明。
3. 对话始终请求真实网关的 `/v1/chat/completions`，因此模型权限、额度、限流、日志与 API 调用完全一致。
4. “停止生成”会停止读取并尽力取消上游请求，已生成的内容保留并标记“已停止”；已产生的用量仍可能计费（日志中标为估算）。
5. “重新发送”会先弹窗说明“将发起新的请求、可能产生新的费用”，确认后才发送；页面不会自动重试或暗中重复提交。
6. 每条回复下方显示 Token 用量（上游未返回时标“估算”或“不可用”）、耗时、请求 ID（点击跳到对应日志）和实际使用的参数。

## 额度与记账规则

金额单位为“微元”（1 元 = 1,000,000 微元），全部用整数计算，费用向上取整到 1 微元。

**可用额度 = 额度上限 − 已结算费用 − 在途预留 − 待核实预留**

1. **先预留，再调用。** 请求发往上游前，按“费用上界”预留额度。预留是一条带条件的 `UPDATE`，在 SQLite 写事务中原子完成，
   同一笔余额不可能被两个并发请求同时占用；预留不足直接返回 402，不调用上游。只有这一步是串行的，上游调用完全并发。
2. **费用上界怎么来。**
   - 聊天：输入上界 = `messages`/`tools` 的 JSON 字节数 + 每条消息 16 + 64（一个 Token 至少占 1 字节，所以一定不小于真实输入 Token 数）；
     输出上界 = `max_tokens × n`。有限额度 Key 未指定 `max_tokens` 时，网关会注入受控上限（模型的“默认输出上限”，否则 4096）。
   - Embeddings：输入字节数 + 每条 8。
   - 模型**未定价**时无法得到可信上界：有限额度 Key 被明确拒绝（`price_not_configured`）；无限额度 Key 可调用，费用记 0 并标“未定价”。
3. **按实际用量结算。** 请求结束后用上游返回的用量计算实际费用，多预留的部分立即释放。日志中的“费用状态”区分：
   `实际`（上游返回用量）、`估算`（上游未返回用量，按内容估算）、`待核实`（费用未知）、`未计费`、`未定价`。
4. **失败与中断不一律免费。**

   | 情况 | 处理 |
   |---|---|
   | 参数错误、无权限、限流、额度不足（未调用上游） | 不扣费 |
   | 连接上游失败、上游明确返回 4xx/5xx | 确认未产生用量，不扣费，释放预留 |
   | 流式中途被取消或断流（已收到内容） | 上游给了用量按实际，否则按已收到的内容**估算**结算 |
   | 已发往上游后超时 / 断连 / 调用方在返回前取消（没有任何用量信息） | 费用未知 → **待核实**，预留继续占用额度 |

5. **不重复记账。** 只有“确认未产生用量”的失败才会切换渠道重试，所以一次网关请求最多只有一次可能计费的上游调用；
   每次上游尝试都记录在日志详情里。结算以“预留仍处于占用状态”为前提，重复的完成通知不会生效。
6. **重启与异常退出。** 已结算费用在各自事务中落盘（`synchronous=FULL`）。启动时自动执行恢复：
   尚未发往上游的在途预留直接释放；已发往上游的转为“待核实”并继续占用额度。
   待核实预留由管理员在“账务核实”页处理（按核实金额结算 / 确认未产生费用并释放）；超过 `PENDING_REVIEW_TTL_SECONDS`
   未处理的，按预留上界自动结算并标“估算”——既不会永久锁住额度，也不会清零放行。
7. 不同 Key 的余额互相独立；无限额度 Key 照常统计用量，但不会因余额被拒。模型权限、RPM 和在途请求数限制先于额度检查。

### 仍无法精确计费的场景

- 上游不返回用量（部分 OpenAI 兼容服务的流式响应、被取消的流）：只能估算，日志明确标“估算”。
- 请求发出后超时或断连：网关无从得知上游是否计费，只能转“待核实”由人工对照上游账单。
- 缓存命中折扣、推理 Token 分档等厂商细分计价：目前只按输入/输出两档单价计算。
- 视频、语音、音乐、音色沿用同一套预留/结算机制，但其价格（按次、按字符、按视频 Token）是管理员填写的单价，
  与厂商按分辨率/时长的真实阶梯价可能不同；Seedance 按 Token 计费时的上界是按“宽×高×24fps×时长/1024 再加 20%”估的，
  不属于 PRD 中“严格防超用”的覆盖范围（PRD 只覆盖聊天与 Embeddings）。
- 音色复刻：厂商在音色首次用于正式合成时才收取复刻费，网关在创建时按 `voice-clone` 的单价记一次。
- 初始化渠道时写入的价格是**占位参考价**（模型备注里有标注），请对照上游账单修改。

### 现有安装的升级说明

直接用新版本二进制替换并重启即可：启动时按 `schema_migrations` 依次执行尚未应用的迁移（第 2 版加限流字段，
第 3 版加 `reserved_cost`、`reservations`、`upstream_attempts`，第 4、5 版加收藏与媒体任务），旧数据原样保留。
升级后需要注意两点：

1. 有限额度 Key 调用的模型必须在“模型”页勾选“已核对定价”，否则会被拒绝（无法计算费用上界）。
2. 有限额度 Key 未传 `max_tokens` 的聊天请求会被注入默认输出上限，如需更长输出请显式传 `max_tokens` 或调大模型的“默认输出上限”。

建议升级前备份 SQLite 文件（连同 `-wal`、`-shm`）。

## 测试与验证

- `make test`：16 组后端用例，覆盖并发额度、结算场景、重启恢复、限流、别名、导出、媒体任务等（受控假上游）。
- `scripts/verify_restart.sh`：真正 `kill -9` 网关进程后重启，核对账务。
- 真实上游的验证过程与结果见 [docs/验证记录.md](docs/验证记录.md)。
