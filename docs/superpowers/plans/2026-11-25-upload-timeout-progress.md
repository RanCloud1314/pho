# 上传超时可配置 + 进度真实化/速率显示 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** WebDAV/NFS/SMB 超时秒数可配置（默认 WebDAV/NFS 60、SMB 0=不限），上传进度封顶 99% 并显示 `30% 30/100MB · 1.2MB/s`。

**Architecture:** 超时值从 Flutter 表单经 SharedPreferences + gRPC `timeout_seconds` 传入内嵌 Go server 的三个 drive 构造函数；WebDAV/NFS 直接用底层库的 SetTimeout，SMB 用「每读一块续期 TCP deadline」的 reader wrapper。进度/速率只改 Dart 侧 state_model + sync_body。

**Tech Stack:** Flutter/Dart、Go (gomobile)、protobuf/gRPC。

**Spec:** `docs/superpowers/specs/2026-11-25-upload-timeout-progress-design.md`

## Global Constraints

- 本机工具链：`go`（1.27）、`D:\dev\protoc\bin\protoc.exe`、`D:\dev\flutter\bin\dart.bat`、`D:\dev\flutter\bin\flutter.bat`（均不在 PATH，需全路径调用）。`protoc-gen-go`/`protoc-gen-go-grpc` 已在 GOPATH/bin。`protoc_plugin` 需先 `dart pub global activate protoc_plugin 21.1.2`。
- 默认值：WebDAV 60、NFS 60、SMB 0（0 = 不限制，负数当 0）。
- prefs key：`webdav_timeout` / `nfs_timeout` / `smb_timeout`（int）。
- Dart 单测用 `D:\dev\flutter\bin\flutter.bat test test/unit/...`；Go 单测用 `go test ./server/drive/smb/ -run TestDeadline`（无 Docker 的纯单测）。集成测试（需 Docker）不在本计划内，跑不了就说明。
- 每个任务结束 `git commit`。

## Review Focus

- `timeout_seconds` 传 0 时三个 drive 都不得引入新的超时（回归验证 SMB 默认行为不变）。
- 旧客户端（不传 timeout_seconds，proto3 默认 0）连新 server：WebDAV 会从 60s 变成无限制 —— 这是接受的行为变化（issue 要的），不是 bug。
- 超时输入非法（非数字/负数/超大）要落在默认值，不得 crash。
- 进度封顶 99% 后，`finishUpload` 失败路径必须仍显示错误文本（uploadFailedMap 分支不依赖 percent）。
- 速率显示在 chunks 很小很频繁时不得每帧重排 UI（EMA + notifyListeners 已有节流：值相同不通知）。

---

### Task 1: proto 加 timeout_seconds + 重新生成

**Files:**
- Modify: `proto/img_syncer.proto`（3 个 message）
- Regenerate: `proto/img_syncer.pb.go`、`proto/img_syncer_grpc.pb.go`、`lib/proto/img_syncer.*.dart`

**Interfaces:**
- Produces: `SetDriveSMBRequest{..., TimeoutSeconds int32}`（字段号 6）、`SetDriveWebdavRequest{..., TimeoutSeconds int32}`（字段号 6）、`SetDriveNFSRequest{..., TimeoutSeconds int32}`（字段号 3）。Dart 侧对应 `timeoutSeconds` 命名参数。

- [ ] **Step 1: 修改 proto 三个 message**（追加 `int32 timeout_seconds = N;`，SMB/WebDAV 用 6，NFS 用 3）
- [ ] **Step 2: 安装 dart 插件并重新生成**

```powershell
D:\dev\flutter\bin\dart.bat pub global activate protoc_plugin 21.1.2
$env:PATH = "$env:PATH;" + (Join-Path (go env GOPATH) "bin") + ";D:\dev\protoc\bin"
protoc -I. --go_out . --go_opt paths=source_relative --go-grpc_out . --go-grpc_opt paths=source_relative --dart_out=grpc:lib proto/*.proto
```
（工作目录 = 仓库根；先确认 `dart pub global` bin 目录进 PATH，否则 protoc 找不到 protoc-gen-dart。）

- [ ] **Step 3: 验证**：`go build ./...` 通过；`git diff --stat` 只含 proto 相关文件。
- [ ] **Step 4: Commit** `feat(proto): add timeout_seconds to SetDrive requests`

### Task 2: WebDAV + NFS 超时接线（Go）

**Files:**
- Modify: `server/drive/webdav/webdav.go:28-43`、`server/api/webdav.go:17`
- Modify: `server/drive/nfs/nfs.go:30-53,108-122`、`server/api/nfs.go:17`
- Modify: 所有 `NewWebdavDrive`/`NewNfsDrive` 调用处（`server/drive/drive_test.go`、`server/api/*_test.go` 传原默认值 60 即可）

**Interfaces:**
- Produces: `NewWebdavDrive(url, username, password string, insecure bool, timeoutSec int32)`、`NewNfsDrive(url string, timeoutSec int32)`。`timeoutSec <= 0` = 不设超时。
- NFS 把 `timeoutSec` 存入 struct，`checkConn()` 重连处（`nfs.go:112,118`）用同一值；`<= 0` 时跳过 `SetTimeout` 调用。

- [ ] **Step 1: 改两个构造函数 + 调用处**（WebDAV: `timeoutSec > 0` 才 `d.cli.SetTimeout(time.Duration(timeoutSec) * time.Second)`；否则不调用）
- [ ] **Step 2: 验证**：`go build ./...`、`go vet ./server/...` 通过。
- [ ] **Step 3: Commit** `feat(drive): configurable timeout for webdav/nfs`

### Task 3: SMB 无进展超时（Go，TDD）

**Files:**
- Modify: `server/drive/smb/smb.go`（struct 加 `conn net.Conn`、`timeout time.Duration`；`Dial()` 存 conn；`Upload` 包裹 content；`DownloadWithOffset` 包裹返回的 f）
- Create: `server/drive/smb/smb_deadline_test.go`
- Modify: `server/api/smb.go:27`（传 `req.TimeoutSeconds`）
- Modify: `NewSmbDrive` 调用处（tests 传 0）

**Interfaces:**
- Produces: `NewSmbDrive(addr, username, password string, timeoutSec int32)`；内部类型 `deadlineReader struct{ io.Reader; s *Smb }`，其 `Read` 每次先 `s.conn.SetDeadline(time.Now().Add(s.timeout))` 再代理 Read。`timeout <= 0` 时 `Upload`/`Download` 不包裹。

- [ ] **Step 1: 写失败测试**（`smb_deadline_test.go`，用 `net.Pipe()` 造 fake conn + `strings.Reader`）：断言 (a) `timeout > 0` 时 Read 前会推进 conn deadline（读 deadline 变化）；(b) `timeout = 0` 时 deadline 不被改动。

```go
func TestDeadlineReaderExtendsDeadline(t *testing.T) { ... }
func TestDeadlineReaderDisabledWhenTimeoutZero(t *testing.T) { ... }
```

- [ ] **Step 2: 运行** `go test ./server/drive/smb/ -run TestDeadlineReader -v`，预期 FAIL（类型未定义）。
- [ ] **Step 3: 实现** `deadlineReader` + `Smb` 的 `conn`/`timeout` 字段 + `Dial` 存 conn + `Upload` 中 `io.Copy(f, s.wrapDeadline(content))` + `DownloadWithOffset` 返回处包裹。并发上传共享同一 conn 的 deadline 相互续期是已知局限，加 `shortcut:` 注释。
- [ ] **Step 4: 运行测试通过**；`go build ./...`。
- [ ] **Step 5: Commit** `feat(smb): stall timeout via per-read deadline renewal`

### Task 4: Dart 三个表单「高级设置」+ 启动恢复 + i10n

**Files:**
- Modify: `lib/storageform/webdavform.dart`、`lib/storageform/nfsform.dart`、`lib/storageform/smbform.dart`
- Modify: `lib/global.dart:185-258`（恢复配置时传 timeoutSeconds）
- Modify: `lib/l10n/app_localizations.dart`、`app_localizations_en.dart`、`app_localizations_zh.dart`

**Interfaces:**
- Consumes: Task 1 的 `timeoutSeconds` 参数。
- Produces: l10n getter `advancedSettings`（高级设置/Advanced）、`timeoutSeconds`（超时秒数/Timeout (seconds)）、`timeoutUnlimitedHint`（0 = 不限制/0 = no limit）。各表单 `TextEditingController timeoutController`，init 从 prefs 读（默认 60/60/0），保存时 `prefs.setInt('<drive>_timeout', v)`。

- [ ] **Step 1: l10n 三个文件加 3 个 getter**（跟现有 getter 的写法一致）
- [ ] **Step 2: 三个表单加同一款「高级设置」ExpansionTile**：内含数字输入（`keyboardType: TextInputType.number`，validator：空或非法 → 用默认值不报错；负数按 0 保存）。所有构造 `SetDrive*Request` 的调用点（webdav: checkWebdav/testStorage；nfs: checkNFS/testStorage；smb: initState listener/refreshShare/testStorage）加 `timeoutSeconds: <int>`。
- [ ] **Step 3: global.dart 恢复配置**三个分支加 `timeoutSeconds: prefs.getInt('<drive>_timeout') ?? <default>`。
- [ ] **Step 4: 验证**：`D:\dev\flutter\bin\flutter.bat analyze` 无新增告警；`flutter test test/unit/global_prefs_test.dart test/widget/sync_body_test.dart` 通过。
- [ ] **Step 5: Commit** `feat(ui): advanced timeout setting in storage forms`

### Task 5: 进度真实化 + 速率 + 显示（Dart，TDD）

**Files:**
- Modify: `lib/state_model.dart:167-255`（transmitState + updateUploadProgress + getUploadPercent）
- Modify: `lib/sync_body.dart:324-353`（subtitle）
- Test: `test/unit/state_model_test.dart`

**Interfaces:**
- Produces: `transmitState.rateBps`（double，EMA α=0.3）；`getUploadPercent` 返回 `min(raw, 0.99)`（仅上传，下载不动）；sync_body 私有 `String _fmtBytes(int)`（B/KB/MB/GB，1 位小数）与 `String _fmtRate(double bps)`。

- [ ] **Step 1: 写失败测试**（state_model_test.dart）：(a) `updateUploadProgress(id, 100, 100)` 后 `getUploadPercent` == 0.99；(b) 连续两次 update（时间差手动可控：第二次传更大 transmitted）后 `rateBps > 0`；(c) `finishUpload` 后 `getUploadPercent` == 0 且条目移除。
- [ ] **Step 2: 运行** 预期 FAIL。
- [ ] **Step 3: 实现**：transmitState 加 `lastTime`/`lastBytes`/`rateBps`；update 时 `dt > 0` 才更新 rate（EMA 0.3）；`getUploadPercent` 加 `min(..., 0.99)`。注意两次 update 若同一微秒 dt=0 则跳过 rate 计算。
- [ ] **Step 4: 实现 sync_body 显示**：percent > 0 时 subtitle 改为 Column：`LinearProgressIndicator(value: percent)` + 4px 间距 + `Text('30% 30/100MB · 1.2MB/s', style: bodySmall + secondary色)`。数字用 `toStringAsFixed`，避免跳动可用等宽 `fontFeatures: [FontFeature.tabularFigures()]`（若主题字体不支持则省略）。
- [ ] **Step 5: 运行** `flutter test test/unit/state_model_test.dart test/widget/sync_body_test.dart` 通过。
- [ ] **Step 6: Commit** `feat(sync): cap upload progress at 99%, show percent/bytes/rate`

### Task 6: 总验证

- [ ] `go build ./...` + `go vet ./server/...`
- [ ] `go test ./server/drive/smb/ -run TestDeadlineReader`
- [ ] `D:\dev\flutter\bin\flutter.bat analyze`
- [ ] `D:\dev\flutter\bin\flutter.bat test test/unit/ test/widget/`
- [ ] 人工冒烟说明（无法在此环境跑真机）：列出用户需手动验证的三条——大文件 WebDAV 上传 >60s 不断开；进度显示新格式；SMB 默认行为不变。
