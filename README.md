# FluxMaker

FluxMaker 是一个使用 PancakeSwap V2 参考价格、同时支持 Binance Spot 和 MGBX Spot 的模块化做市系统。配置真源是 PostgreSQL，已发布配置和登录 Session 缓存在 Redis；交易引擎不再读取 JSON 配置文件。

系统只用于真实双边流动性、库存管理和外部成交，不实现自成交、关联账户互相成交、刷量或价格操纵。

币对可启用“成交量仿真（内部）”：它只读取交易所买一卖一，在合法 Tick 内生成带 `simulated=true` 的本地事件，不向交易所提交订单、不写入交易所成交量、不进入真实成交统计。仿真事件与真实成交分栏展示，并写入独立 Redis Stream，供 UI 或内部联调、压测程序消费。

## 服务组成

- `admin-api`：后台页面、登录、RBAC、配置草稿和发布。
- `fluxmaker`：读取已发布配置并运行报价、风控和订单管理。
- `watchdog`：主进程心跳异常时独立撤销受管订单。
- `postgres`：用户、角色、权限、配置草稿、发布快照和审计日志。
- `redis`：当前发布配置、管理员 Session、运行快照、引擎心跳和暂停控制。

交易所接入通过适配器注册表完成。每个适配器声明 Client Order ID、批量下单、批量撤单、订单查询、成交和规则同步能力；OMS、引擎和 Watchdog 不按交易所名称分支。支持 Client Order ID 的适配器只管理 FluxMaker 订单，不支持的适配器必须使用专用账户并管理该账户全部订单。

## Docker 启动

### 一条命令部署到 Linux 服务器

服务器还没有安装 Docker 也可以直接运行。脚本会自动安装 Docker Engine、Buildx、Docker Compose 和基础工具，打包当前代码并通过 SSH 上传，在服务器构建启动，开放本机防火墙端口并等待健康检查通过：

```bash
./deploy.sh root@服务器公网IP
```

默认部署 Java 后台，访问地址是 `http://服务器公网IP:8080`。首次部署会在服务器自动生成 PostgreSQL、Redis、管理员、Metrics 和主加密密钥，并在命令结束时显示一次管理员密码；重复部署会保留服务器的 `.env`、主加密密钥和 Docker 数据卷。部署成功后默认保留最近 3 个版本供回滚，自动删除更旧的 FluxMaker 镜像和发布目录，并清理 7 天前的未使用构建缓存；不会执行卷清理，也不会删除 PostgreSQL、Redis 或运行数据。

使用 SSH 私钥或其他端口：

```bash
./deploy.sh --identity ~/.ssh/server.pem --ssh-port 22 ubuntu@服务器公网IP
```

如果要明确使用已有生产环境变量（例如首次迁移已有主加密密钥），可以传入本机环境文件：

```bash
./deploy.sh --env-file .env root@服务器公网IP
```

部署账号必须是 `root` 或拥有 `sudo` 权限，支持 Ubuntu、Debian、CentOS、RHEL、Rocky Linux 和 AlmaLinux。脚本能处理服务器内的 UFW/firewalld，但云厂商安全组需要另外放行 TCP `8080`。默认保持 `FLUXMAKER_ENABLE_LIVE_TRADING=DISABLED`；只有显式上传的环境文件或后续人工修改才会开启实盘二次开关。公网 IP 方式目前是 HTTP 临时访问，配置域名和 HTTPS 前应把云安全组来源限制为自己的固定 IP，不要开启实盘。

准备环境变量：

```bash
cp .env.example .env
```

至少修改这些值：

```text
POSTGRES_PASSWORD
REDIS_PASSWORD
ADMIN_EMAIL
ADMIN_PASSWORD
CREDENTIAL_MASTER_KEY
METRICS_TOKEN
```

后台实现由 `.env` 的一个开关统一选择，默认继续运行现有 Go 版本：

```text
BACKEND_IMPL=go
```

需要切到 Java 时改为 `BACKEND_IMPL=java` 后重新执行 `docker compose up -d --build`。`admin-api`、`fluxmaker` 和 `watchdog` 会整体切换，PostgreSQL、Redis、数据卷、端口和配置快照均不变；不要同时运行 Go 与 Java 交易引擎。

密码不要使用示例内容。管理员密码至少 12 位。主加密密钥只生成一次：

```bash
openssl rand -base64 32
```

把输出填入 `.env` 的 `CREDENTIAL_MASTER_KEY`，并在安全位置备份；丢失或更换它会导致数据库中的交易所凭证无法解密。

Prometheus 指标 Token 单独生成，不要与管理员密码或交易凭证共用：

```bash
openssl rand -hex 32
```

填入 `.env` 的 `METRICS_TOKEN`。未配置时 `/metrics` 返回 404，不会暴露币对和运行指标。

启动完整环境：

```bash
docker compose up -d --build
```

如果服务器安装的是旧版 Compose v1，使用 `docker-compose up -d --build`。出现 `unknown shorthand flag: 'd'` 通常表示当前 `docker` 命令没有 Compose v2 子命令。

代码更新后需要明确删除旧应用容器、使用新镜像强制重建时，运行项目脚本：

```bash
./scripts/rebuild-local.sh
```

也可以运行 `make docker-rebuild`。脚本会先构建带唯一 `code_build` 标识的新镜像；只有构建成功后，才强制替换 `admin-api`、`fluxmaker` 和 `watchdog`，并自动清理孤儿容器。PostgreSQL、Redis 和数据卷不会删除或重建。启动日志中的 `code_build` 是程序构建版本，而 `published configuration applied, version=42` 中的 `42` 是数据库配置发布版本，两者互不相关。

查看状态：

```bash
docker compose ps
docker compose logs -f admin-api fluxmaker watchdog
```

打开后台：

```text
http://服务器地址:8080
```

使用 `.env` 中的 `ADMIN_EMAIL` 和 `ADMIN_PASSWORD` 登录。

`ADMIN_EMAIL` 对应的 bootstrap 管理员密码以 `.env` 为准：如果数据卷中已经存在该账户，重启 `admin-api` 会同步更新它的密码并确保账户启用。其他后台用户的密码不会被修改。

首次启动还没有发布配置，FluxMaker 会保持等待，不会发送订单。登录后台后依次维护：

1. 系统模式和 BSC RPC。
2. Token、合约地址和币对。
3. PancakeSwap V2 单跳或多跳价格路径。
4. 每个币对的报价与库存策略。
5. 在“交易所凭证”中新增加密凭证。
6. 维护 Binance/MGBX Symbol、精度和最小金额，并为每个币对绑定凭证。
7. 保存草稿。
8. 发布配置。

发布后交易引擎会自动发现目标版本，先在不触碰订单的情况下构建候选运行时并验证 RPC、TWAP、盘口和实盘账户。候选失败时旧版本继续运行；候选通过后按新旧配置差异增量切换，不再因为普通配置发布而全量撤单。

后台按实际运营流程组织：

- “币对管理”使用列表和详情页，详情分为基础信息、链上参考价、交易市场、策略与风控、成交量仿真（内部）。
- “交易所”只维护平台级连接；币对映射统一在币对详情的“交易市场”中维护。
- “交易所凭证”维护真实交易账号；币对选择凭证时只读取名称、类型和启用状态。
- 草稿允许分步骤、不完整保存；只有点击“校验并发布”时才执行完整安全校验。
- 配置仍以全局不可变快照发布，便于审计和回滚；但全局版本不再等于全量重启。策略变化只由 OMS 调整对应币对的不匹配订单，删除币对、停用市场或更换账号时只撤对应 `币对 × 交易所`，只有 Live→Shadow 和全局紧急停止才会全撤。
- “运行监控”按币对展示 Pancake 指数价、各交易所买一卖一、账户库存、挂单、近期成交和行情/账户连接状态，每 3 秒刷新。
- 每个币对支持 `1–100` 档双边报价；大深度订单按每个市场每轮最多 20 张滚动撤挂，避免单轮突发数百次 API 请求，重新定价时也不会一次撤空全部档位。
- 做市报价以 Pancake TWAP/指数价为定价中心，不要求交易所预先存在买一和卖一。空盘口会直接按策略半点差铺出双边订单，单边盘口会补齐缺失方向；公共盘口接口暂时不可用时也不会阻断指数价报价。读取到已有对手盘时仍会作为 Post-Only 边界，避免主动吃单。
- MGBX 未完成订单会按服务端 `total` 读取全部分页；普通重定价和安全撤单优先使用每批最多 20 张的原生批量撤单接口。
- OMS 会记录已提交但尚未出现在挂单列表中的订单，先占用对应目标档位，并通过订单详情确认异步结果，防止重复下单；异步撤单超时会有限重试，连续失败后阻断该市场。
- OMS 下单统一提交为每批最多 20 张的 Post-Only 请求：适配器实现安全的原生批量能力时调用一次批量接口，否则统一调度层逐笔回退；每个返回订单仍独立保存待确认状态和 fencing generation，部分成功不会被误判为整批成功。MGBX 批量创建文档未声明支持 `GTX`，确认前继续使用单笔 Post-Only 接口。
- 实盘会按每个交易市场的 Base/Quote 可控资金裁剪报价，优先保留内层档位。`余额预留（bps）` 用于保留不参与铺单的资金，运行监控会展示预算、目标需要量和实际可执行订单数。
- Redis 状态心跳和 Watchdog 文件由独立发布器每 2 秒维护，不再受多币对报价循环和批量 REST 操作阻塞。
- OMS 的待确认下单、待确认撤单和阻断状态保存到 Redis（7 天 TTL），引擎重启后先恢复状态再与交易所对账；操作员执行“暂停并撤单 → 恢复”后会清除已持久化的市场阻断状态。
- 每个 `交易所凭证 × 币对 × Symbol` 使用 Redis 市场租约和单调递增的 fencing generation。每次下单、单笔撤单和批量撤单前都会重新验证 `Owner + generation`；旧实例恢复网络后会被拒绝写入，Client ID 也携带 generation 便于追查。Watchdog 仍可在引擎失联时执行独立安全撤单。
- 候选版本预检会从 Binance `exchangeInfo` 自动同步价格 Tick、数量 Step、最小/最大数量与金额和单币对挂单上限；MGBX 自动同步价格与数量精度。运行中默认每 300 秒重新同步，可在“运行与 RPC”调整；变化只热更新对应市场，并保存旧值/新值告警，不会全局撤单。MGBX 未公开的最小金额继续使用后台配置。
- Binance 签名请求遇到 `-1021` 时间偏差时会读取交易所服务器时间、保存毫秒偏移并安全重试一次，避免容器虚拟机时钟校正导致账户查询和挂撤单持续失败。
- Binance 安全撤单仍按受管 Client ID 精确选择订单，但每批最多使用 5 个并发请求；共享账号中的手工订单不会被全市场撤单接口误伤。测试网 40 张订单的暂停撤单由约 11 秒缩短到约 3 秒。
- 相邻报价强制至少相差一个价格 Tick；交易所/指数价格偏差和交易所盘口点差超过策略阈值时，只停止并撤销对应 `币对 × 交易所`。
- 每个交易市场可配置最大 Base 和 Quote 资金占用，`0` 表示不额外限制；该硬上限与余额预留、交易所订单上限共同裁剪外层档位。
- 每个 `币对 × 交易所` 独立维护 `normal → degraded → canceling → paused → recovering → normal` 故障状态。异常状态持久保存到 Redis，配置热切换或进程重启后继承失败计数、撤单和恢复阶段；确认恢复为 normal 后才删除。普通超时先保留安全期内的原订单；达到连续失败阈值、价格过期或价格保护失败后才定向撤单。
- “运行与 RPC”可配置连续失败次数、连续恢复次数和故障安全宽限。后台运行监控展示故障阶段、失败/恢复计数及订单是否仍被保留。
- “对账并解除阻断”会先查询交易所当前挂单，再清理持久化 OMS 阻断状态；不会撤销现有订单，也不需要重启整个引擎。
- 实盘“运行状态”页提供一次性的“启动盘口重建”按钮。它在最新链上参考价附近用 Post-Only 补齐买一、卖一首档，不撤现有订单；异常盘口中间价/点差和日常库存单边抑制只在这次人工恢复中不作为首档依据，真实余额仍决定双边首档是否可承担，参考价有效期、精度、最小金额、挂单上限和租约围栏也必须通过。冷启动时即使所有币对都被预检阻断，引擎也会以禁止自动写入的降级模式运行控制循环，确保该按钮可以执行。
- Watchdog 同时检查独立进程心跳和交易循环进度。进程仍在线但超过 `交易循环卡死超时` 没有任何币对完成一次循环时，也会执行安全撤单。
- 交易轮次使用有界 Worker Pool 并行处理币对，默认最大并发数为 4，可在“运行与 RPC”调整为 1–32。同一交易凭证绑定的多个币对会自动使用账户锁串行执行，避免共享余额并发超配；不同账户和无凭证的 Shadow 币对可以并行。
- BNB Chain 最新区块读取会在 500ms 窗口内合并复用，同一轮多个币对不再重复请求 `eth_getBlockByNumber`；Pair 储备仍按各币对独立读取。
- 审计事件先在内存中完成 JSON 编码，每个交易轮次统一执行一次文件写入和 `fsync`，不再为每条报价与 OMS 事件单独打开并同步文件。写入失败时事件保留在待写队列并在下一轮重试。
- 运行监控展示最近交易轮次耗时、成功币对数和当前并发上限；交易进度 Redis 写入限制为每秒最多一次，减少多币对完成时的重复缓存写请求。
- “运行监控”顶部汇总当前严重告警和警告：引擎离线、交易进度超时、最近轮次失败/超时、实盘账户断连、行情断连、市场故障状态、审计待写以及 Watchdog 触发结果都会形成可定位到币对和交易所的告警。
- FluxMaker、Admin API、Watchdog 的 stdout 使用 JSON 结构化日志并附带 `service`；HTTP 变更请求、慢请求和错误请求附带 `request_id`、状态码与耗时。报价计划只记录档数、内外层价格和总数量摘要，不再为 100 档输出整张订单列表。
- 连续相同的交易循环错误最多每分钟输出一次 Warning；故障内容改变或恢复时立即记录，避免交易所长时间异常造成日志风暴。
- Watchdog 每次检查把健康状态、最近触发原因和撤单错误写入 Redis；同一轮心跳故障只执行一次保护撤单，恢复后才允许下一轮触发。
- 审计 JSONL 默认单文件 100MiB、保留 7 份，可在“运行与 RPC”调整；Docker stdout 默认每个容器单文件 20MiB、保留 5 份，可通过 `.env` 的 `LOG_MAX_SIZE` 和 `LOG_MAX_FILES` 调整。
- `.env` 的 `LOG_LEVEL` 支持 `debug/info/warn/error`，默认 `info`。只有临时排障时使用 `debug`，此时会额外输出每个币对的紧凑报价摘要和成功 GET 请求。
- “暂停并撤单”和“紧急暂停并撤单”先写入持久化控制指令；后台依次显示“撤单中 → 已暂停”，不会把指令已提交伪装成撤单已完成。交易引擎确认后停止该币对的新报价并撤销受管挂单。恢复会显示“恢复中 → 运行中”，且必须由拥有 `runtime:start` 权限的用户显式执行。
- 币对暂停后仍按正常轮询周期只读刷新指数价、交易所盘口、余额、实际挂单、成交和连接状态，不会把暂停前的锁仓余额或待确认订单长期显示为实时数据，也不会执行任何挂撤单写操作。
- PancakeSwap V2 路径只需填写 Pair 地址；后台通过已保存的 BNB Chain RPC 读取 `token0()`、`token1()` 和 `factory()`，再根据币对基础币或上一跳输出自动生成 Base/Quote Token。两跳路径必须连续，并最终到达币对的报价币合约。
- Pancake V2 TWAP 首次达到窗口后会在下一窗口形成前继续复用最近一次有效 TWAP，避免出现“就绪一轮、暖机一轮”的交替状态；每个完整窗口仍会滚动更新累计价格基线。
- 内部成交量仿真从指定市场读取公开盘口，默认规划器按价格 Tick 在 `bid < price < ask` 中随机选价，按数量 Step、最小数量和最小金额生成数量；没有合法内部 Tick 时跳过本轮。随机方向使用配置的买方向概率，默认 5000 bps。
- Java 仿真逻辑通过 `VolumeSimulationPlanner` 扩展；规划器只能接收配置和只读市场快照，返回方向、价格、数量。框架统一校验价差/Tick/Step 并强制生成 `SIM-*` ID 和 `simulated=true`，扩展层不持有交易所下单客户端。

## 配置和缓存规则

```text
后台编辑 → PostgreSQL draft_configs
         → 权限和参数校验
后台发布 → PostgreSQL config_snapshots
         → Redis fluxmaker:config:active
交易引擎 → Redis 优先，PostgreSQL 回源
运行快照 → Redis fluxmaker:runtime:instrument:{instrument_id}（45 秒 TTL）
引擎心跳 → Redis fluxmaker:runtime:engine（15 秒 TTL）
交易循环进度 → Redis fluxmaker:runtime:trading-progress（持久保存最后进度时间）
最近轮次性能 → Redis fluxmaker:runtime:cycle-performance（5 分钟 TTL）
累计运行指标 → Redis fluxmaker:runtime:metrics（5 分钟 TTL）
Watchdog 状态 → Redis fluxmaker:runtime:watchdog（5 分钟 TTL）
实际应用版本 → Redis fluxmaker:runtime:applied-version（持久保存，供 Watchdog 使用）
OMS恢复状态 → Redis fluxmaker:oms:state:{credential_market_key}（7 天 TTL）
市场故障状态 → Redis fluxmaker:fault:state:{credential_market_key}（异常期间持久保存，恢复后删除）
市场运行租约 → Redis fluxmaker:lease:market:{credential_market_key}（自动续租）
租约围栏代次 → Redis fluxmaker:lease:generation:{credential_market_key}（单调递增，不随租约释放重置）
交易规则变更 → Redis fluxmaker:runtime:rule-changes（最近 100 条，24 小时 TTL）
暂停控制 → Redis fluxmaker:control:paused（持续保存，直到显式恢复）
内部仿真事件 → Redis Stream fluxmaker:simulation:fills:{instrument_id}（约 1000 条）
```

草稿不会影响交易；只有发布快照才会被执行。

“运行与 RPC”同样属于全局运行配置：点击“保存全部草稿”只写入 PostgreSQL，不改变当前引擎；点击“发布全局版本”后才生成新快照并由引擎切换。草稿与当前运行版本一致时，后台会禁用发布按钮，避免产生无内容变化的新版本。

## 权限

内置角色：

- `super_admin`
- `risk_admin`
- `operator`
- `viewer`
- `auditor`

后台支持菜单/操作权限和币对数据范围。非全局角色只能看到分配给自己的 `instrument_id`。前端菜单隐藏只是展示层，后端接口仍会再次检查权限和币对范围。

受限用户保存草稿时，服务端只合并其有权访问的币对及市场映射，不会覆盖其他币对或修改全局交易所开关。角色页面使用权限和币对复选框，不需要手工填写权限代码。

## 密钥

交易所 API Key/Secret 在后台按“凭证账号”维护，可供不同币对分别绑定。明文在后台提交后使用 AES-256-GCM 加密写入 PostgreSQL；Redis、配置草稿和发布快照只保存 `credential_id`，浏览器后续只看到名称、Key 尾号和指纹，不会回显明文。

`CREDENTIAL_MASTER_KEY` 是唯一不能放进数据库的根密钥，保存在服务器 `.env`，生产环境建议交给 Docker Secret/KMS。API Key 应关闭提现权限并限制服务器出口 IP。修改根密钥前必须设计密钥轮换流程，不能直接覆盖。

Shadow 验证完成前保持：

```text
mode = shadow
trading_enabled = false
FLUXMAKER_ENABLE_LIVE_TRADING = DISABLED
```

Binance Spot Testnet 联调：在“交易所”把运行环境选择为“测试网”，后台会自动使用 `https://testnet.binance.vision`。绑定从 Spot Testnet 创建的 API Key，保持 STP 为 `EXPIRE_BOTH`。若要验证真实测试网铺单，再将系统切到 Live 并开启服务器的 Live 二次开关；内部成交量仿真本身在 Shadow 模式即可运行，且永远不会调用下单接口。

MGBX 测试网只填写其官方提供的 HTTPS Base URL 和测试网凭证；项目不会猜测或内置未经官方确认的测试地址。

实盘需要后台配置和服务器环境变量同时开启：

```text
mode = live
trading_enabled = true
FLUXMAKER_ENABLE_LIVE_TRADING = I_UNDERSTAND
```

## 管理 API

- `POST /api/login`
- `GET /api/me`
- `PUT /api/me/password`
- `GET|PUT /api/config/draft`
- `POST /api/config/publish`
- `POST /api/config/plan`
- `GET /api/config/active`
- `GET /api/runtime`
- `GET /api/monitoring`
- `GET /api/runtime/{instrument_id}`
- `POST /api/runtime/{instrument_id}/pause`
- `POST /api/runtime/{instrument_id}/resume`
- `POST /api/runtime/{instrument_id}/emergency-cancel`
- `POST /api/oracle/pancake-v2/inspect-pair`
- `GET|POST /api/users`
- `PUT /api/users/{id}`（邮箱、启停和角色）
- `PUT /api/users/{id}/password`
- `POST /api/users/{id}/revoke-sessions`
- `GET /api/user-role-options`
- `GET /api/roles`
- `PUT /api/roles/{id}`
- `GET /api/permissions`
- `GET /api/credential-options`
- `GET /api/venue-types`
- `GET|POST /api/credentials`
- `PUT /api/credentials/{id}`
- `GET /healthz`
- `GET /livez`（只检查 Admin API 进程）
- `GET /readyz`（检查 PostgreSQL 和 Redis）
- `GET /metrics`（Prometheus 文本格式，需要 Metrics Token）

浏览器使用 Redis Session 的 HttpOnly Cookie。API 调用也支持 `Authorization: Bearer`。

后台账户不做物理删除：停用账户可以保留历史审计关联。账户状态、邮箱、角色或密码修改，以及“强制下线”操作，都会递增权限版本并立即拒绝全部旧 Session；系统禁止停用当前账户、移除自己的 `super_admin`，并保证至少保留一个已启用的超级管理员。

## 日志与监控

查看结构化日志：

```bash
docker-compose logs -f --tail=200 fluxmaker admin-api watchdog
```

检查存活与依赖就绪：

```bash
curl -fsS http://127.0.0.1:8080/livez
curl -fsS http://127.0.0.1:8080/readyz
```

抓取 Prometheus 指标：

```bash
curl -fsS \
  -H "Authorization: Bearer $METRICS_TOKEN" \
  http://127.0.0.1:8080/metrics
```

主要指标包括：

- `fluxmaker_engine_up`、`fluxmaker_engine_ready`
- `fluxmaker_engine_heartbeat_age_seconds`
- `fluxmaker_trading_progress_age_seconds`
- `fluxmaker_cycle_duration_seconds`
- `fluxmaker_cycles_total`、`fluxmaker_cycle_failures_total`
- `fluxmaker_instrument_failures_total`
- `fluxmaker_venue_fault_events_total`
- `fluxmaker_oms_placed_orders_total`、`fluxmaker_oms_canceled_orders_total`
- `fluxmaker_rule_changes_total`、`fluxmaker_recent_rule_changes`
- `fluxmaker_lease_fence_rejections_total`
- `fluxmaker_audit_flush_errors_total`、`fluxmaker_audit_pending_events`
- `fluxmaker_watchdog_healthy`、`fluxmaker_watchdog_last_trigger_age_seconds`
- 每个币对和交易所的连接状态、运行耗时、挂单数和待确认订单数

Prometheus 可基于这些指标连接 Alertmanager。第一组建议规则：引擎离线持续 30 秒、交易进度超过配置阈值、Watchdog 不健康、审计待写大于 0、实盘账户连接为 0、循环失败率持续升高。后台 `/api/monitoring` 使用相同运行数据提供即时告警汇总。

## 停止和备份

正常停止：

```bash
docker compose stop fluxmaker watchdog
```

主程序收到终止信号时会尝试撤销受管订单。修改或关闭交易所前，应先在后台将其切换到停止状态并确认订单已经撤销。

Binance 的近期成交来自账户成交明细；MGBX 第一版使用 REST 历史订单中的累计成交数量和均价，因此后台会标记为“订单汇总”，不把它伪装成逐笔成交。紧急撤单对 Binance 只撤销 FluxMaker Client ID 前缀的订单；MGBX 实盘要求专用账户，紧急撤单会管理该币对账户中的全部挂单。

PostgreSQL 和 Redis 使用 Docker named volumes。PostgreSQL 仍是配置真源，生产环境必须设置定时备份；Redis 同时承载租约、围栏代次和运行安全状态，Compose 已开启 AOF，生产环境也必须监控 AOF 持久化与磁盘健康，不能按可随意清空的普通缓存运维。

## 本地验证

```bash
go test ./...
go vet ./...
mvn -f java/pom.xml test
docker compose config --quiet
make test-integration
```

`make test-integration` 会在 PostgreSQL 中创建并自动删除独立 schema，同时只使用 Redis DB 15；不会修改当前发布配置、交易凭证、运行控制或 Redis DB 0。它覆盖配置缓存一致性、OMS/故障状态持久化、租约 fencing generation、角色变更后的会话即时失效，以及账户创建、启停、邮箱/角色更新、密码重置、强制下线和最后管理员保护。
colima start
docker-compose up -d --build
docker-compose logs -f --tail=500 fluxmaker
/opt/fluxmaker/current
./deploy.sh root@168.144.132.3
ssh root@168.144.132.3 

./scripts/rebuild-local.sh
