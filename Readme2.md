# Readme2 — HEIC 显示支持：把图片处理交给对象存储，memos 只做代理

这份文档记录了本 fork 相对上游的全部改动、改动理由、运维配置、迁移步骤和回退方式。

- 基线版本：`usememos/memos` @ `0d989707`（`git describe` = `v0.31.0-33-g0d989707`）
- 改动范围：**4 个 Go 文件 + 1 个 README**，外加 2 个测试文件；不涉及 proto、数据库、前端
- 新增依赖：**无**

---

## 1. 要解决的问题

小米手机拍出的照片是 HEIC。**除 Safari 外的主流浏览器都不支持 HEIC**（caniuse：HEIC 全球覆盖率 14.63%，AVIF 95.36%），而 memos 上传后浏览器里显示为破图。

根因有三条，互相独立：

| 断点 | 位置 | 事实 |
| --- | --- | --- |
| 二进制里没有 HEIC 解码器 | 全仓库 | `go list -deps` 只链入 `image/jpeg`、`image/png`、`image/gif`；真实 HEIC 会得到 `image: unknown format` |
| 上传时的"剥 EXIF 转 JPEG"从未生效 | `server/api/v1/attachment_service_image.go` | `imaging.Decode` 解不了 HEIC，`processAndSaveAttachment` 只记一条 warning 然后原样入库 |
| 缩略图对 HEIC 主动放弃 | `server/fileserver/fileserver.go` | `shouldUseOriginalForThumbnail` 直接返回 true，最终把原始 HEIC 字节以 `Content-Type: image/heic` 发出 |

结果是**上传静默成功、显示静默失败**：两端都不报错，服务端日志里几乎没有痕迹，只有浏览器里一个破图。

### 为什么不本地解码

2C2G 服务器上实测（9.2MP，`GOMAXPROCS=2`，纯 Go 编解码路径）：JPEG q92 峰值 67MB / 418ms，WebP q90 峰值 265MB / 1.23s，**无损 WebP 峰值 446MB / 3.5s（method 6 到 642MB / 17s）**。而仓库允许 3 个并发缩略图，10-bit 照片还会以 `NRGBA64` 让内存翻倍——本地解码会 OOM。无损 WebP 另有否决理由：源已经是有损的，无损不增加信息，只多付 7–9 倍体积。

### 为什么不直连对象存储

官方**试过并主动删掉了**：提交 `38412eb7`（`fix(storage): proxy S3 attachments through authenticated routes (#6210)`）删除了整个 `s3presign` 后台 runner，proto 字段只留墓碑（`reserved "last_presigned_time"`）。理由写在 `attachment_service_storage.go` 里：签名 URL 会**比 memo 可见性变更活得更久**。本改动遵守这个设计：**所有字节仍然经过 memos 的鉴权路由**。

---

## 2. 行为规格

| 请求 | 非 HEIC/HEIF | HEIC/HEIF（且附件在 S3） | HEIC/HEIF（LOCAL/DB 存储） |
| --- | --- | --- | --- |
| `GET /file/attachments/:uid[/:filename]` | 原样（存储对象） | **provider 生成的 display 派生图**（`image/avif`） | 原样（存储对象） |
| `?thumbnail=true` | 原样（本地生成 JPEG，行为不变） | **provider 生成的 thumb 派生图**（`image/jpeg`） | 原样（存储对象） |
| `?original=true`（新增） | 原样 | 原样（存储对象，逐字节） | 原样 |

`?original=true` 是新增的逃生门：**裸 URL 在 HEIC 上不再等于"存储里的字节"**，下载原片必须显式带上它。

作用域刻意收窄：只有 `image/heic` / `image/heif` 走 provider。JPEG/PNG/WebP 保持原有本地缩略图路径，既省 provider 调用，也避免 provider 重编码改变非 HEIC 图片的观感。

---

## 3. 改动的代码

### 3.1 `provider/storage/s3/s3.go`（+76 / -1）

- 新增 `GetProcessedObjectStream(ctx, key, byteRange, process)`：`process` 是原始 query string（如 `w=600&fmt=avif`），**在 SigV4 签名之前**追加到请求 query string，因此签名覆盖它。`GetObjectStream` 变成它的空 `process` 包装。
- 新增 `addProcessQuery` 中间件：用 `middleware.Stack.Finalize.Insert(..., "Signing", middleware.Before)` 插入——与仓库已有的 `excludeAcceptEncodingFromSigning` / `forceSignedPayload` 同一套路。追加时**复制 `o.APIOptions` 切片**，避免并发写客户端共享的底层数组。
- 新增 `ErrObjectProcessingRefused`：provider 用 4xx 回答处理请求时归类为"对这张图的判决"；5xx/网络错误保持可重试。由 `isClientError` 判定。

### 3.2 `provider/storage/driver.go`（+11 / -0）

- `Driver` 接口新增 `GetProcessedObjectStream`。
- 别名导出 `ErrObjectProcessingRefused`（与已有的 `ErrRangeNotSatisfiable` 同风格），让 file server 不必 import AWS 类型。

### 3.3 `server/fileserver/fileserver.go`（+197 / -31）

- 新增 `imageDerivative` 描述符与两个实例：`thumbnailDerivative`（`{uid}.v2.jpeg`、`image/jpeg`、本地可回退）与 `displayDerivative`（`{uid}.display.v1.avif`、`image/avif`、仅 provider）。
- 把原来的 `getOrGenerateThumbnail` 主体泛化成 `getOrGenerateDerivative`：**缓存读取 → `.failed` 标记 → 信号量 → 生成 → 写缓存**的顺序与语义完全保留，缩略图路径行为不变，display 路径直接复用同一套缓存机制。
- 新增 `derivativeProducer` / `canProcessRemotely` / `generateRemoteDerivative` / `getAttachmentProcessedBlob`。
- `shouldUseOriginalForThumbnail` 对 HEIC 分支补上注释说明为什么仍然返回原图（走到本地生产者时说明 provider 不可用，本地既不能解码也不该压平广色域）。**LOCAL/DB 存储上 HEIC 的行为与改动前完全一致。**
- `generateThumbnail` 不再自己写缓存文件（写缓存统一由 `getOrGenerateDerivative` 负责），因此少了 `thumbnailPath` 参数与 `os.WriteFile`。
- `serveStaticFile` 增加 display 分支与 `wantOriginal` 参数；`serveAttachmentFile` 解析 `?original=true`。

### 3.4 `store/attachment.go`（+5 / -2）

- `deleteAttachmentDerivedCaches` 的清理清单加入 display 派生图及其 `.failed` 标记。该函数的注释明确要求文件名与 `server/fileserver` 保持一致，新增缓存文件必须同步加在这里，否则本地会永久残留孤儿文件。

### 3.5 `server/fileserver/README.md`（+2 / -0）

- 端点清单补 `?original=true`，并新增一条说明 provider 派生图的行为与开关位置。

### 3.6 新增测试（未跟踪文件，共 201 行）

- `provider/storage/s3/s3_process_test.go`（119 行）：5 个测试，覆盖处理参数确实上线、普通读取不带参数、4xx 归类为拒绝、5xx 保持可重试、**以及普通 S3 后端会忽略未知 query 参数**（这条保证把常量开着指向 RustFS/MinIO 也不会坏）。
- `server/fileserver/fileserver_derivative_test.go`（82 行）：端到端验证 HEIC 附件在 S3 上时，裸 URL 返回 `image/avif`、`?thumbnail=true` 返回 `image/jpeg` 且**产出了本地解码器无法产出的 `.v2.jpeg`**、`?original=true` 逐字节返回原对象、删除附件时两份派生图都被清掉。

### 3.7 行数与最初估算的差异（说明）

最初估"约 93 行"。**实际生产代码 289 行新增**（`driver.go` 11 + `s3.go` 76 + `fileserver.go` 197 + `store/attachment.go` 5），差距来自三点，都是后几轮讨论新增的需求：

1. **缓存语义**：为了 display 也吃上缓存，`getOrGenerateThumbnail` 被泛化成描述符 + 生产者间接层，而不是上一版"直通"；
2. **错误分类**：区分"provider 判决"与"瞬时故障"，与仓库既有的 `.failed` 标记设计对齐；
3. **注释密度**：仓库 lint 要求导出标识符有注释、解释"为什么"，这部分占了不少行。

---

## 4. 参数与开关

参数是**包级常量**，位于 `server/fileserver/fileserver.go`：

```go
remoteThumbnailProcess = "w=600&fmt=jpeg&q=80&cs=srgb"
remoteDisplayProcess   = "w=2560&fmt=avif&q=85"
```

- 缩略图用 `fmt=jpeg`：这样缓存文件名（`.v2.jpeg`）与响应 `Content-Type`（`image/jpeg`）与改动前完全一致，不必改动任何既有路径。
- display 用 `fmt=avif`：`Content-Type` 必须与之一致（`displayContentType`），否则 `nosniff` 下浏览器不会渲染。
- 缩略图带 `cs=srgb`：避免小图在非 HDR 环境下发灰；display 不加，交给 provider 决定 HDR。

**完全关闭这个能力**：把两个常量置空后重新编译即可（`process == ""` 时 `canProcessRemotely` 恒为 false，一切回到上游行为）。另一个等价做法是直接换回官方二进制——见第 7 节。

---

## 5. 缓存设计

| 层 | 行为 |
| --- | --- |
| memos 本地磁盘（主力） | `.thumbnail_cache/{uid}.v2.jpeg`（缩略图）与 `.thumbnail_cache/{uid}.display.v1.avif`（display），各带 `.failed` 标记。永久、跨重启；**每张图每个尺寸只调用 provider 一次** |
| S4 侧 | 不生成派生对象。CoreIX 是只读转换，桶里保持干净，没有孤儿对象需要清理 |
| 浏览器 | 保持官方 `private, no-store` 不变（这是官方为私有内容的刻意选择；本地缓存已经消除了重复处理成本） |

失败语义与仓库既有设计一致：**只有** `errThumbnailUnsupported`（永久判决，例如 provider 4xx）才写 `.failed` 标记，之后的请求直接回退原图；网络/5xx 故障不写标记，下次请求重试。

**参数变更会导致旧缓存被继续使用**：缓存文件名里没有参数指纹。改了常量请同时提升文件名版本（例如 `display.v1` → `display.v2`），并同步 `store/attachment.go` 的清理清单。

---

## 6. 迁移指导：RustFS → Bitiful S4

### 6.1 关键前提

**S3 附件的对象键不在 `reference` 列，而在 `payload` 里**（`s3_object.key` + `s3_object.storage_id`）。这意味着：

- 迁移必须**保持对象键完全一致**，数据库不用动；
- 必须**修改同一个 storage 条目的 endpoint / 密钥**，不要新建一个条目。老附件的 `storage_id` 指向原条目，新建条目会让老附件解析回旧配置（或解析失败）。新附件才会用新的默认条目。

### 6.2 步骤

```bash
# 1) 备份数据库（SQLite 就是数据目录里的单个文件）
cp "$DATA/memos_prod.db" "$DATA/memos_prod.db.bak"

# 2) 对拷对象，保持 key 一致（rclone 两个 S3 remote，都是 S3 兼容）
rclone copy rustfs:$BUCKET s4:$BUCKET --checksum --progress

# 3) 校验：对象数量与抽样字节一致
rclone check rustfs:$BUCKET s4:$BUCKET --size-only

# 4) 在 memos 里把原 storage 条目的 S3 配置改为 S4
#    endpoint / region / access key / secret / bucket，通常需要打开 UsePathStyle
#    存储条目 Id 必须保持不变

# 5) 保留 RustFS 与原数据一段时间作为回退
```

### 6.3 迁移后验证清单

- 随机抽 5 个附件下载，字节与原 RustFS 对象一致；
- 未开启派生图时 `?thumbnail=true` 行为与迁移前一致；
- 打开相册时 **memos 进程 CPU 基本不动**（这是成功的标志：解码编码都在 S4 侧）；
- `{data}/.thumbnail_cache/` 出现 `.v2.jpeg` 与 `.display.v1.avif`。

---

## 7. 上线前必须实测的三件事

这三件事**不需要改代码**，但没做就不该打开这个能力：

| 关卡 | 验什么 | 怎么验 | 不通过的后果 |
| --- | --- | --- | --- |
| **G1** | provider 能否处理你的真实小米 HEIC | 传一张到 S4，控制台 URL 预览加 `?w=600&fmt=jpeg&q=80&cs=srgb` 与 `?w=2560&fmt=avif&q=85` | 派生图永远失败，回退原图（不崩，但没效果） |
| **G2** | 处理参数能否**随签名**生效 | 用本 fork 直接跑：如果 S4 拒绝，日志会出现 `storage provider refused ...`，且 `.failed` 标记写入 | 需要改用外部生成器方案 |
| **G3** | 方向与 HDR | 竖拍照片方向是否正确；HDR 照片是否发灰（必要时给 display 也加 `cs=srgb`）；**`w=2560` 是否会把小图放大** | 需要调整参数常量 |

G2 的机制部分已在本仓库离线验证（见第 9 节：参数确实进入签名请求并被发送），但**S4 服务端是否接受签名覆盖这些参数只能连真实账号验证**。

---

## 8. 与官方版本的兼容性

改动**只动代码，不动数据格式**：

| 部分 | 是否改动 | 交给官方版本运行 |
| --- | --- | --- |
| 数据库 schema | 否 | 无迁移、无版本变更 |
| `AttachmentPayload` | 否 | 官方照常读出对象键与 `storage_id` |
| S4 桶里的对象与键 | 否 | 官方用同一套 S3 配置读同一批键 |
| `.thumbnail_cache/{uid}.v2.jpeg` | 内容来源变为 provider，但仍是 JPEG、文件名不变 | 官方直接命中缓存并当 `image/jpeg` 发出，**照常显示** |
| `.thumbnail_cache/{uid}.display.v1.avif` | 新增 | 官方不认识、永不读取，只是占盘 |

刻意避开了两条会给"交给官方"制造麻烦的路：

1. **没有往 `AttachmentPayload` 加字段**——payload 以 protojson 存储，三个驱动的反序列化器都设了 `DiscardUnknown: true`，官方读到你加的字段不会报错，但**下次写这一行时会静默丢掉它**；
2. **没有在 S4 里生成派生对象**——否则桶里会积累官方不认识的孤儿对象，且官方的删除逻辑不会级联清理。

### 回退到官方版本

```bash
# 用相同版本的官方二进制（跨版本注意：数据库迁移是单向的，先备份再试）
# 可选：清理官方不认识的本地缓存文件
Get-ChildItem "$DATA/.thumbnail_cache" -Filter "*.display.*" | Remove-Item
```

降级后的现象：**已经缓存过的 HEIC 缩略图继续正常显示**（缓存名与格式都没变）；没有缓存的 HEIC 会退回破图；lightbox 的任何 HEIC 都会破图。非 HEIC 图片完全不变。

---

## 9. 测试情况

已执行（`CGO_ENABLED=0`，Go 1.27.0）：

| 命令 | 结果 |
| --- | --- |
| `go build ./server/fileserver/... ./provider/storage/... ./store/...` | 通过 |
| `go vet ./server/fileserver/... ./provider/storage/s3/...` | 通过 |
| `go test ./provider/storage/s3/...` | 5 个新测试全部通过 |
| `go test ./server/fileserver/...` | 通过（既有测试 + 1 个新端到端测试） |
| `go test ./provider/...` | 通过 |
| `go test ./server/...` | `server`、`server/api/v1`、`server/api/v1/test`、`server/auth`、`server/fileserver`、`server/mcp`、`server/test` 全部通过 |
| `go test ./store/test/...` | 6 个失败，**与未修改的 HEAD 逐一对齐**（用干净的 `git worktree` 跑过基线对比，失败集合完全一致，新增数为 0） |

环境性失败（**与本改动无关**，均非新引入）：

- `store/test` 与 `server/frontend` 的失败原因都是同一句：`TempDir RemoveAll cleanup: unlinkat ... memos_prod.db: The process cannot access the file because it is being used by another process.` —— Windows 上 SQLite 未及时关闭导致 `t.TempDir()` 清理失败；基线复现；
- `store/...` 中依赖 Testcontainers 的用例因 Docker 未运行而跳过。

未执行（均为环境限制，不是代码问题）：

- **`go test -race`**：`-race` 依赖 cgo，而本机没有 C 编译器（`cgo: C compiler "gcc" not found`），因此竞态检测**未能运行**。这是一项待补的检查，请在装有 C 工具链的环境执行：
  ```bash
  go test -race ./server/fileserver/... ./provider/...
  ```
  与并发相关的风险点已在实现时规避：`GetProcessedObjectStream` 追加中间件前**复制** `o.APIOptions` 切片（`o.APIOptions` 与客户端的选项共享底层数组，并发 append 会写坏共享数组）；派生图的缓存读写沿用上游既有的"后写覆盖、内容相同"模式，未引入新的共享状态。
- `golangci-lint run` —— 本机未安装（AGENTS.md 要求的另一项检查）：
  ```bash
  golangci-lint run
  ```
- 前端检查 —— 本改动不涉及前端。

---

## 10. 同步上游 SOP

本节是**流程**，不是脚本。每次同步由 AI 按下面的步骤执行，人工只在"冲突"和"main 无法快进"两种情况下介入。

### 10.1 一次性前提（已配置好，无需重复）

| 项 | 值 |
| --- | --- |
| `origin`（自己的 fork） | `git@github.com:shinyes/memos.git`，可读可写 |
| `upstream`（官方） | `git@github.com:usememos/memos.git`，**push 地址为 `DISABLED`**（本地拦截，改动永远不会流向官方） |
| `remote.pushDefault` | `origin`（裸 `git push` 只去自己的 fork） |
| `rerere.enabled` / `rerere.autoupdate` | `true`（同一处冲突解过一次后自动复用） |
| **约定** | `main` 永远是上游纯镜像，只做 `--ff-only`；所有改动在 `feat/object-storage-image-derivatives` 分支上 |

### 10.2 每次你要说的话

正常同步：

```text
读 Readme2.md 第 10 节，按 SOP 同步上游。
```

同步并推送：

```text
读 Readme2.md 第 10 节，按 SOP 同步上游，完成后推送到 origin。
```

出现冲突、你想让 AI 代解时：

```text
Readme2.md 第 10 节，rebase 停在冲突状态。请解决冲突后继续，
并说明每一处冲突你选择了哪一边、为什么。
```

### 10.3 执行步骤

```bash
# 步骤 0 — 预检。工作树必须干净（有未提交改动就停下问人，不要 stash 后硬跑）
git status --short
git rev-list --left-right --count upstream/main...main   # 期望 "0 0"

# 步骤 1 — 拉上游（只读，含 tag）
git fetch upstream --tags --prune

# 步骤 2 — main 快进为纯镜像
git checkout main
git merge --ff-only --no-stat upstream/main
# 若报错：说明 main 上有自己的提交，破坏了纯镜像约定 → 停下问人，不要强推

# 步骤 3 — 把自己的改动重放到上游之上
git checkout feat/object-storage-image-derivatives
git rebase upstream/main
# 若冲突：走 10.4

# 步骤 4 — 验证（同步结果必须仍然能构建并通过测试）
go build ./server/fileserver/... ./provider/storage/... ./store/...
go vet   ./server/fileserver/... ./provider/storage/s3/...
go test  ./server/fileserver/... ./provider/storage/...
# 有 C 工具链的环境再补：go test -race ./server/fileserver/... ./provider/storage/...
# 有 golangci-lint 的环境再补：golangci-lint run

# 步骤 5 — 推送（仅当用户要求）
git push origin main
git push --force-with-lease origin feat/object-storage-image-derivatives
# rebase 改写了提交，分支必须用 --force-with-lease（不要用 --force）
```

### 10.4 冲突处理

```bash
git status                    # 冲突文件清单
# 逐个编辑解决，然后：
git add <文件>
git rebase --continue
# 放弃本次同步：
git rebase --abort
```

`rerere` 已开启：**同一处冲突只要解过一次，下次自动复用解法**，不需要再次人工介入。所以人工成本只发生在"上游第一次以新方式改动了你改过的代码"。

解决冲突时的判断原则（按优先级）：

1. 上游的新结构优先——跟着上游的写法改自己的代码，不要把自己的旧结构塞回去；
2. 本改动的三条不变量不能丢：处理参数必须在**签名前**注入（`provider/storage/s3/s3.go`）、派生图缓存文件名必须与 `store/attachment.go` 的清理清单一致、`?original=true` 必须仍返回存储对象；
3. 拿不准就停下问人，不要猜测。

### 10.5 完成后的自检

```bash
git log --oneline upstream/main..HEAD     # 只应列出自己的提交
git log --oneline HEAD..upstream/main     # 应为空（已同步）
git diff upstream/main...HEAD --stat      # 自己的改动足迹
git log -1 --oneline main                 # main 应等于 upstream/main
```

### 10.6 预期冲突面

这些是上游的活跃文件，也是本改动挂接的地方，冲突大概率发生在这里：

| 文件 | 冲突形态 |
| --- | --- |
| `server/fileserver/fileserver.go` | 改动最大（+197 行），`serveStaticFile` / 派生图管线被上游重构时最易冲突 |
| `provider/storage/s3/s3.go` | 上游新增签名中间件或改 `GetObjectStream` 时 |
| `provider/storage/driver.go` | `Driver` 接口被扩展时，把 `GetProcessedObjectStream` 重新挂上 |
| `store/attachment.go` | `deleteAttachmentDerivedCaches` 的清理清单被改动时（**必须保持文件名一致**） |
| `server/fileserver/README.md` | 端点清单被改写时 |

随时可以看上游动没动你的文件：

```bash
git log --oneline upstream/main -- server/fileserver/fileserver.go provider/storage/s3/s3.go
```

**别攒着**：上游动过这几个文件就当天同步，冲突通常只有几行；攒三个月再 rebase 等于重写。

### 10.7 退出条件

**如果上游自己加上了 HEIC 支持**（引入解码器，或提供类似的对象存储处理机制），就应当**删掉本改动、改跟官方走**：`git checkout main`，删除 `feat/object-storage-image-derivatives` 分支（或留作参考）。这个 fork 分支存在的唯一理由是上游缺这块能力。

---

## 11. 已知限制

1. **原图仍带 GPS**：CoreIX 只在输出派生图时删 Exif，**不改桶内原图**；而 memos 的 HEIC 上传路径依旧无法剥 EXIF（没有解码器）。这一点与改动前一致，不是新引入的问题。要连原片也不留位置信息需要单独处理。
2. **HEIC 里的动态照片不会被识别**：动态照片检测只在 `image/jpeg` / `image/jpg` 上触发。
3. **display 派生图不缓存到 S4**：这是刻意的（避免孤儿对象），代价是换设备/清缓存后每个尺寸会重新调用一次 provider。
4. **参数改常量需重编译**，且改参数要手动提升缓存文件名版本（见第 5 节）。
5. **裸 URL 语义变化**：HEIC 上不再等于"存储里的字节"；需要原片请带 `?original=true`。归档不受影响——导出/导入走服务端直读存储（`GetAttachmentBlob`），不经过 `/file` 路由。
6. **G2/G3 未在真实 S4 上验证**（见第 7 节）。

---

## 12. 仓库状态与提交

改动**已提交**在分支 `feat/object-storage-image-derivatives` 上，`main` 仍是上游纯镜像（`git rev-list --left-right --count upstream/main...main` = `0 0`）。

```
632e099d docs: record the derivative pipeline, its caches, and the S4 migration
96bf81ad feat(fileserver): serve provider derivatives for undecodable images
1f65a7b6 feat(storage): read objects through a provider processing expression
```

每个提交只含一个关注点，便于日后 rebase 时按需 `--skip` 或重写：

| 提交 | 文件 |
| --- | --- |
| `1f65a7b6` 存储层 | `provider/storage/s3/s3.go`、`provider/storage/driver.go`、`provider/storage/s3/s3_process_test.go` |
| `96bf81ad` file server | `server/fileserver/fileserver.go`、`server/fileserver/README.md`、`server/fileserver/fileserver_derivative_test.go`、`store/attachment.go` |
| `632e099d` 文档 | `Readme2.md` |

合计 8 个文件、+929 / −34。第 2 个提交依赖第 1 个（用到 `GetProcessedObjectStream` 与 `ErrObjectProcessingRefused`），顺序不能颠倒。

工作树里剩下的两个未跟踪目录已加进**本地**排除文件 `.git/info/exclude`，不会产生任何跟踪改动、永远不与上游冲突：

| 路径 | 内容 | 处理 |
| --- | --- | --- |
| `.heic-poc/` | 前期调研工具（`probe.exe` 探测 HEIC、`bench.exe` 编码基准、合成样本） | 已在排除列表；G1/G3 验证时可继续用 |
| `.heic-poc-cache/` | Go 模块与构建缓存（约 2.8 GB） | 已在排除列表；纯缓存，可安全删除（下次构建重新下载） |

尚未推送到 `origin`。要备份到 GitHub 时：

```bash
git push -u origin feat/object-storage-image-derivatives
```

