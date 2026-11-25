# S3 存储后端设计（泛 S3 协议）

日期：2026-11-25
状态：已获口头批准，待 builder check

## 意图与动机

Pho 目前支持 SMB/WebDAV/NFS 三种存储后端。用户希望新增「AWS S3 网络存储协议」支持，经讨论确定为**泛 S3 协议**：不仅 AWS 官方 S3，还覆盖一切 S3 兼容存储（Cloudflare R2、MinIO、阿里云 OSS S3 兼容端点、腾讯 COS、群晖等），让没有内网 NAS 的用户也能用对象存储备份照片。

## 目标

- 用户在存储类型选择页可选 S3，填 endpoint/region/ak/sk/bucket/pathStyle 即完成配置
- 上传/下载/删除/增量同步（FilterNotUploaded）/视频 Range 流播全部工作，与其他 drive 行为一致
- 大文件（视频）自动分片上传
- 集成测试用 MinIO 容器验证，纳入现有 `make test` 体系

## 非目标 / 反目标

- 不做多 bucket、bucket 内目录浏览 UI（SMB/WebDAV 也没有）
- 不做 presigned URL 直连（流量本就走本地 Go 服务转发）
- 不做 AWS IAM 临时凭证/STS、IRSA 等企业特性
- 不引入第二个 S3 SDK

## 约束

- 必须实现 `StorageDrive` 接口（`server/imgmanager/interface.go`）：Upload/IsExist/Download/DownloadWithOffset/Delete/Range/Close
- `RangeByDate` 依赖 `YYYY/MM/DD` 目录树遍历，S3 需以前缀模拟目录
- gRPC 生成依赖 `make protobuf`；gomobile 构建需 `CGO_ENABLED=0`，SDK 必须纯 Go（aws-sdk-go-v2 满足）
- 响应包络统一 `Success + Message`；drive 热插拔经 `im.SetDrive()`
- Dart 侧配置存 `shared_preferences`，键名风格对齐 `webdav_url` 等现有键

## 备选方案

1. **aws-sdk-go-v2 (service/s3)** — AWS 官方，维护最勤，原生支持自定义 endpoint + path-style + 分片上传 manager。依赖偏重但可控。**已选**。
2. **minio-go** — 轻量，同样覆盖泛 S3，但社区维护，且与 gomobile 的纯 Go 约束无额外优势。弃。
3. **手写 SigV4 + REST** — 零依赖但代码量大、边签名错误难查，无必要。弃。

SDK：aws-sdk-go-v2；目录语义：ListObjectsV2 delimiter + CommonPrefixes 模拟目录（唯一可行方案）；测试：MinIO Docker 容器（对齐现有 docker-compose 测试模式）。均为实现者已定。

## 设计

### 配置字段（SetDriveS3 RPC 请求）

| 字段 | 类型 | 说明 |
|---|---|---|
| Endpoint | string | 空 = AWS 官方；否则如 `https://s3.example.com` 或 R2 的 `https://<acct>.r2.cloudflarestorage.com` |
| Region | string | 必填；MinIO/R2 可填 `us-east-1` 或 auto |
| AccessKeyID | string | 必填 |
| SecretAccessKey | string | 必填 |
| Bucket | string | 必填 |
| PathStyle | bool | R2/OSS/COS/MinIO 通常需 true；AWS 官方可 false（virtual-host style） |
| Root | string | 可选，空 = bucket 根；非空时所有 key 加 `root/` 前缀，用于共享 bucket 场景 |

### `server/drive/s3/s3.go`

- `type S3 struct { cli *s3.Client, uploader *manager.Uploader, bucket, root string }`
- 客户端构造：`config.LoadDefaultConfig` + `aws.String(ak/sk)` 静态凭证 + 自定义 `BaseEndpoint` + `UsePathStyle`；endpoint 为空时不设，走 AWS 默认。`root` 来自请求（可选，空 = bucket 根），key 拼接统一用 `path.Join`（勿用 `filepath.Join`，Windows 反斜杠会污染 S3 key）
- `Upload(path, r, size, modtime)`：`manager.Uploader.Upload`（≥32MB 自动分片），读完后 `r.Close()`；modtime 无法设置到对象元数据之外，不额外写 metadata（保持与其他 drive 一致的最小行为）
- `IsExist(path)`：HeadObject，404 → false
- `Download(path)`：GetObject → (Body, ContentLength)
- `DownloadWithOffset(path, offset)`：GetObject 带 `Range: bytes=offset-`
- `Delete(path)`：DeleteObject（S3 删除即成功，无 NoSuchKey 报错语义）
- `Range(dir, deal)`：ListObjectsV2 分页遍历 prefix=`root+dir`、delimiter=`/`；Contents 逐个构造 `fs.FileInfo`（name=去掉前缀后的 basename，size=Size，modtime=LastModified，isDir=false）回调；CommonPrefixes 同样回调为 isDir=true 的 FileInfo（RangeByDate 只对目录递归，需此形态）。deal 返回 false 时停止分页
- `Close()`：无状态连接，返回 nil
- 所有错误原样包装返回，不吞错

### `proto/img_syncer.proto`

新增 `SetDriveS3(SetDriveS3Request) returns (SetDriveS3Response)`，字段如上表，响应 `Success + Message`。不做 ListDriveS3Dir（无目录浏览）。

### `server/api/s3.go`

`SetDriveS3`：校验非空字段 → 构造 drive → `HeadBucket` 探测（60s 超时）→ 失败返回 `Success=false, Message=err` → `a.im.SetDrive(d)`。对齐 `webdav.go` 模式。

### `lib/storageform/s3form.dart`

表单字段同上表；`shared_preferences` 键：`s3_endpoint`, `s3_region`, `s3_access_key_id`, `s3_secret_access_key`, `s3_bucket`, `s3_path_style`, `s3_root`。pathStyle 用开关。提交调 gRPC `SetDriveS3`。在存储类型选择页注册新卡片（对齐 SMB/WebDAV/NFS 入口样式）。

### 测试

`server/api/s3_test.go`：MinIO 容器（docker-compose 新增 service），testify/suite，走完整 gRPC SetDriveS3 + HTTP 上传/下载/Range/FilterNotUploaded 流程，对齐 `webdav_test.go`；Root 非空场景跑一个用例验证前缀生效。MinIO 首次启动需等 healthy（复用 `waitfile` 式轮询）。

## 风险

- **凭证明文存 shared_preferences**：与现有 WebDAV/SMB 一致，是既有模式，不单独为 S3 加密存储（如需，应是全局特性）
- **S3 ListObjectsV2 每页 1000 条**：Range 全量遍历在大 bucket 上偏慢，与现有 `RangeByDate` 全量遍历的性能短板同级，不在本次解决
- **aws-sdk-go-v2 依赖体积**：会增大 gomobile 产物体积，可接受

## 留给实现者

- s3form 的具体 UI 布局照抄 webdavform.dart
- proto 生成后 pb 字段名以生成结果为准
- MinIO 容器的镜像 tag 取当时最新稳定版
