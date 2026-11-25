# 上传超时可配置 + 进度真实化 + 进度/速率显示 设计稿

日期：2026-11-25。来源：issue「大文件上传超时」（WebDAV 60s 固定超时）。

## 问题（已核实）

- `server/drive/webdav/webdav.go:41` 硬编码 `SetTimeout(60 * time.Second)`，慢速上行下大文件必然 60 秒被掐断（nginx 记 499）。
- 上传进度按本地已读入 sink 的字节计算（`lib/storage/storage.dart:202-206`），会先显示 100% 再失败，卡 99% 的观感来自服务端确认滞后。
- 无超时配置项；同步列表（`lib/sync_body.dart:327-333`）只有进度条，无百分比/字节数/速率。

## 方案（用户已确认）

1. **超时配置（WebDAV/NFS/SMB 三表单）**：每个表单加「高级设置」折叠区（ExpansionTile），含超时秒数输入（数字键盘，默认值见下，0 = 不限制）。存 `SharedPreferences: {webdav,nfs,smb}_timeout`，经各 `SetDrive*Request` 新增字段 `timeout_seconds` 传入 Go：
   - **WebDAV**（默认 60，维持现状）：`webdav.go` `SetTimeout(timeout)`，0 = 无超时。
   - **NFS**（默认 60，维持现状）：`NewNfsDrive` 及重连处（`nfs.go:43,49,112,118`）的 `SetTimeout` 改用配置值。
   - **SMB**（默认 0 = 维持现状，目前本就无请求超时）：go-smb2 无请求级超时，采用**读写deadline 续期**：`Smb` 保存底层 `net.Conn`，上传/下载时把源 reader 包一层「每读一块就把 conn deadline 续到 now+timeout」的 wrapper——无进展超时才失败，慢速长传不会被掐断。拨号超时保持 30s 不动。
2. **进度真实化**：`getUploadPercent` 封顶 99%，`finishUpload` 成功后才显示「已上传」，失败显示错误原因（区分网络超时/中断）。
3. **进度显示**：同步列表 ListTile 的 subtitle 改为 Column：LinearProgressIndicator + 一行文字 `30% 30/100MB · 1.2MB/s`。百分比等宽数字、次要色、不改变 ListTile 高度节奏（仅在进度>0 时出现，与现状一致）。
4. **速率**：`transmitState` 增加 `rate` 字段，`updateUploadProgress` 按字节增量/时间差计算并用 EMA 平滑（避免每 chunk 跳动），UI 显示 MB/s。

## 不做

- 断点续传/分片上传（大工程，issue 建议项，暂缓）。
- 按服务端确认字节计算进度（需改协议，不值得）。
- SMB 拨号超时配置化（保持 30s）。

## 触达面

- proto：`SetDrive{Webdav,SMB,NFS}Request` 各加 `timeout_seconds`（Dart/Go 两侧重新生成，`make protobuf`）。
- Go：`webdav.go`（构造函数加 timeout 参数）、`nfs.go`（NewNfsDrive/重连用配置值）、`smb.go`（保存 conn + deadline 续期 reader）、`server/api/{webdav,smb,nfs}.go`（传参）。
- Dart：三个表单（高级设置 UI + 保存）、`storage.dart`（启动恢复配置处传 timeout）、`state_model.dart`（rate + 封顶 99%）、`sync_body.dart`（显示）。
- i18n：新增字符串（超时、不限制、高级设置等）。

## 验收

- WebDAV 设超时 600 后，大文件上传超过 60 秒不再断开；超时=0 时 gowebdav client 无超时。
- NFS 超时配置传入 mount/target 的 SetTimeout。
- SMB：无进展超过配置秒数时报错；慢速长传（有进展）不超时；默认 0 完全不设限。
- 进度文字显示 `30% 30/100MB · 1.2MB/s`，传输中不跳到 100%。
- `go build ./...`、`flutter analyze` 通过；表单默认值、输入 0 保存生效。
