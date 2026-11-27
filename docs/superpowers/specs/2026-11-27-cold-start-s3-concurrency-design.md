# 云端相册冷启动 214 并发优化（S3 路径）

日期：2026-11-27
状态：待批准

## 意图与动机

冷启动首次打开云端相册，S3 侧监控看到 214 个并发 GET。复查后的真实机制（初版误判为 gallery_body 窗口偏移计算 bug，实际 `gallery_body.dart:1250` 已按行累加图片行高，该判断已撤回）：

1. `main.dart:258` 的 `IndexedStack` 会构建全部子页——隐藏的云端相册 tab 在冷启动时即触发窗口预加载（约 -2屏~+3屏，含上百格）
2. 客户端无任何并发上限，S3 延迟高时大量请求同时在途
3. `state_model.dart:431` 每次刷新把 `remoteAssets = []` 重建，Asset/RemoteImage 全为新实例，内存缩略图缓存清零 → 整个窗口从头重拉；旧在途请求不取消 → S3 并发累积（214 ≈ 初始爆发 + 刷新轮次叠加）
4. 失败重试 3×固定 100ms，高压下雪崩放大
5. 缩略图仅内存缓存，冷启动必然全量重拉

S3 drive 本身（`server/drive/s3/s3.go`）逻辑健康：缩略图 <2MB 不触发分片，每张 1 个 GET，不是它放大了并发。

修复分两层：客户端治本（限流 + 缓存 + 退避），服务端 S3 路径加固（挡任何客户端风暴）。

## 目标

- 冷启动 S3 并发 GET ≤ 8（客户端限 6，服务端兜底 8）
- 第二次冷启动 S3 缩略图流量 ≈ 0（客户端磁盘缓存命中）
- 刷新触发的重复全量拉取同样命中缓存（新实例 → 磁盘缓存命中，S3 零流量）
- 失败重试指数退避 100/200/400ms，不再瞬间 3 连击

## 非目标 / 反目标

- 不重写网格为 GridView.builder/Sliver 虚拟化（限流 + 缓存后窗口大小不再是 S3 压力来源）
- 不做 IndexedStack 懒加载（隐藏 tab 的预加载经限流后不再打爆 S3；懒加载是独立的 UX 性能话题）
- 不做 SQLite/JSON 缓存索引（文件系统即索引，缩略图几 KB 一张）
- 不做服务端 GetThumbnail singleflight（客户端已有 completer 去重，重复仅在重试时发生）
- 不限流原图下载/视频流（单图查看器场景，天然低并发）
- 客户端信号量不覆盖 `videoDuration()`（其调用量受窗口限制，且每屏视频占比低）
- 不取消刷新触发的重复请求（磁盘缓存命中后刷新轮次不再产生网络请求，无需取消语义）

## 约束

- 客户端信号量复用 `async_locks` 的 `Semaphore`（已有依赖，`lib/sync/background_runner.dart:120` 同模式）
- S3 drive 并发限制只覆盖 GET；上传已有 worker=2 队列串行，不动
- 服务端信号量须覆盖 body 流式传输全程（Close 时释放），否则限的是"发起数"不是真实并发连接数
- 缓存 key 含 size，不用 crypto 包（不为此引新依赖）

## 备选方案

1. **客户端修复 + 磁盘缓存 + 服务端 S3 加固**——治本 + 兜底，改动集中在 3 个 Dart 文件 + 1 个 Go 文件。**已选**。
2. **GridView.builder 虚拟化重写**——能根治窗口问题，但窗口 bug 本身只需一行修复，重写不划算。弃。
3. **只改服务端**——客户端 214 请求照发，打开仍慢（响应排队），且每次冷启动仍全量拉。弃。

## 设计

### 客户端

**1. 缩略图信号量（`lib/storage/storage.dart`）**
模块级 `final thumbnailSemaphore = Semaphore(6)`，`RemoteImage.thumbnail()` 网络段 acquire/release（try/finally）。磁盘缓存命中路径不占信号量。

**2. 指数退避（`lib/storage/storage.dart`）**
重试延迟改为 `Duration(milliseconds: 100 << retryCount)` → 100/200/400。

**3. 磁盘缓存（`lib/storage/storage.dart` + 新文件 `lib/storage/thumbnail_cache.dart`）**
- 类 `ThumbnailCache(Directory baseDir)`，`read(path, size)` / `write(path, size, bytes)`，内部吞异常（best-effort，缓存故障不得让缩略图加载失败）
- 实例经 `thumbnailCacheLoader` 全局函数获得（默认实现走 `getApplicationSupportDirectory()/thumbnails/`，path_provider 不可用时返回 null 禁用缓存；测试可注入临时目录实例）
- 文件名：FNV-1a 64(path) + '_' + size（~5 行实现，无 crypto 依赖；64bit 空间 5 千张碰撞概率 ~1e-13）
- 命中直接返回字节（不走网络、不占信号量）；未命中走网络，成功且通过 `isValidImage` 校验后写盘
- `isValidImage` 从 `asset.dart` 移到 `util.dart`（storage.dart 与 asset.dart 有循环依赖，util.dart 是两方共同的依赖）

### 服务端（`server/drive/s3/s3.go`）

**4. GET 并发上限 8**
`S3` struct 加 `sem chan struct{}`（cap 8）。`download()` 拆为 wrapper：acquire（select ctx.Done）→ 调原逻辑（改名 `downloadInner`）；错误路径立即释放；成功返回包一层 `semBody{rc, sem}` 在 `Close()` 释放（`sync.Once` 防双释放）。分片尾片（`startTail` 内的 `getObjectRange`/`directGet`）不受限——每文件 4 连接是绕 TCP 慢启动的刻意设计，且走该路径的是原图不是缩略图。

**5. transport 连接池**
`tr.MaxIdleConnsPerHost = 16`（默认 2，并发 6~8 时空闲连接反复建拆）；SDK 客户端经 `config.WithHTTPClient(awshttp.NewBuildableClient().WithTransportOptions(...))` 设同样的 `ResponseHeaderTimeout` 与 `MaxIdleConnsPerHost`（follow302=false 时缩略图全走 SDK 客户端）。

### 缓存失效

- key 含 size：源图变化 → size 变 → key 变，天然失效。
- shortcut: 同 size 不同内容的缩略图会 stale（重新生成同尺寸缩略图罕见，遇到再升级。
- shortcut: 无 LRU/容量上限（214 张 × ~10KB ≈ 2MB，量级无忧），超过万张再补。

## 验证

- 客户端：`flutter analyze`、`flutter test` 全绿（含先修复 storage_test.dart 现存 3 个红测试）；FNV-1a 纯函数单测；本地 HttpServer 验证并发 ≤ 6 与退避重试
- 服务端：`go build ./...`、`go test ./server/drive/s3/`（semBody 单测无需 Docker；设 `S3_TEST_ENDPOINT` 跑集成）
- 端到端：清缓存冷启动，S3 侧并发 GET ≤ 8；二次冷启动/下拉刷新 S3 无缩略图 GET
