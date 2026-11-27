# 云端相册冷启动 214 并发优化（S3 路径）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 冷启动打开云端相册时 S3 并发 GET 从 214 降到 ≤8，二次冷启动 S3 缩略图流量 ≈ 0。

**Architecture:** 三处修改：客户端 `RemoteImage.thumbnail()` 加 6 并发信号量（async_locks，已有依赖）+ 指数退避；缩略图磁盘缓存（新文件 ThumbnailCache，FNV-1a 键，best-effort）；服务端 S3 drive 加 8 并发 GET 上限（semBody 在 body Close 时释放槽位）+ 空闲连接池修正。

**Tech Stack:** Flutter/Dart（async_locks、path_provider）、Go（aws-sdk-go-v2）。

**Spec:** `docs/superpowers/specs/2026-11-27-cold-start-s3-concurrency-design.md`

## Global Constraints

- 前置：先提交 `server/drive/s3/` 下未提交的分片下载改动（上一轮工作，已含测试），避免与本计划的 diff 混淆
- 不新增任何依赖：客户端信号量用已有 `async_locks`（`lib/sync/background_runner.dart:120` 已有同款用法）；哈希手写 FNV-1a，不引 crypto
- `flutter analyze` 0 error；`flutter test` 全绿。现状：`test/unit/storage_test.dart` 有 3 个红测试（`checkServer` 里 `grpcPort` 未初始化抛 `LateInitializationError`），Task 1 一并修复
- Go 侧 `go build ./...`、`go vet ./server/drive/s3/` 通过；`sem_test.go` 单测无需 Docker；S3 集成测试仅在设 `S3_TEST_ENDPOINT=http://127.0.0.1:9000`（先 `docker compose -f test/docker-compose.yml up -d minio`）时执行
- 磁盘缓存 best-effort：任何缓存异常不得导致缩略图加载失败
- FNV-1a 64 位运算仅 Dart VM（移动/桌面）有效；`storage.dart` 已依赖 dart:io，不涉 web

## Review Focus

1. **Dart 信号量异常路径泄漏**（网络段抛错时未释放 → 后续缩略图永久饿死）→ Task 1 退避测试走满 3 次失败重试全链路，断言第 3 次成功；并发测试断言 20 个全返回
2. **Go semBody 双 Close / 错误路径泄漏**（slot 未释放 → 后续下载永久阻塞）→ Task 3 `TestSemBodyCloseReleasesOnce`（Close 两次只释放一次）+ `TestDownloadErrorReleasesSlot`（出错即释放）
3. **缓存毒化**（不可解码字节落盘 → 该图每次冷启动永久 broken）→ Task 2「不可解码字节不落缓存」断言第二次仍走网络
4. **缓存 key 失效与碰撞**（源图变了 key 没变 / 哈希不稳）→ Task 2 FNV-1a 已知向量（''、'a'、'foobar'）+ size 变化即 miss
5. **path_provider 不可用时缓存必须降级**（测试/纯 VM 环境抛 MissingPluginException 若未吞 → 缩略图全部失败）→ Task 2「默认 loader 降级 null」在 flutter test 环境实测

---

### Task 1: 客户端信号量限流 + 指数退避（+ 修复现有红测试）

**Files:**
- Modify: `lib/storage/storage.dart`（`RemoteImage.thumbnail()`，当前 361-388 行）
- Test: `test/unit/storage_test.dart`

**Interfaces:**
- Consumes: `Semaphore`（`package:async_locks/async_locks.dart`，`acquire()`/`release()`）
- Produces: `thumbnail()` 签名不变（`Future<Uint8List?>`）；模块级 `final Semaphore _thumbnailSemaphore = Semaphore(6);`；网络段 acquire/release 包 try/finally；退避 `Duration(milliseconds: 100 << retryCount)`。Task 2 要在同函数内插入缓存读写，保留此结构。

- [ ] **Step 1: 修复现有 3 个红测试**

`test/unit/storage_test.dart` 的 `RemoteImage.dataStream - contentLength null defense (M9)` 组，setUp 末尾加 `lastAliveTime = DateTime.now();`（短路 `checkServer` 的 gRPC ping，该文件已 import `global.dart`），tearDown 里恢复 `lastAliveTime = null;`。

Run: `flutter test test/unit/storage_test.dart`
Expected: 现存 3 个红的 dataStream 测试转绿。

- [ ] **Step 2: 写失败测试（并发上限 + 退避重试）**

在 `test/unit/storage_test.dart` 追加两个 group（沿用现有本地 `HttpServer` + `httpBaseUrl` 模式）：

```dart
group('RemoteImage.thumbnail 并发限流', () {
  late HttpServer server;
  int active = 0;
  int maxActive = 0;

  setUp(() async {
    active = 0;
    maxActive = 0;
    server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    httpBaseUrl = 'http://127.0.0.1:${server.port}';
    lastAliveTime = DateTime.now();
    server.listen((request) async {
      active++;
      if (active > maxActive) maxActive = active;
      await Future.delayed(const Duration(milliseconds: 100));
      active--;
      final resp = request.response;
      resp.statusCode = 200;
      resp.headers.set('content-length', '4');
      resp.add([1, 2, 3, 4]);
      await resp.close();
    });
  });

  tearDown(() async {
    lastAliveTime = null;
    await server.close(force: true);
  });

  test('20 个并发缩略图全部成功且同时在途 ≤6', () async {
    final images = List.generate(
        20,
        (i) => RemoteImage(MockImgSyncerClient(), 'img$i.jpg',
            size: 100, httpClient: http.Client()));
    final results = await Future.wait(images.map((e) => e.thumbnail()));
    expect(results.where((r) => r == null), isEmpty);
    expect(maxActive, lessThanOrEqualTo(6));
    expect(maxActive, greaterThan(1)); // 不能退化成全串行
  });
});

group('RemoteImage.thumbnail 失败指数退避', () {
  late HttpServer server;
  final requestCount = <String, int>{};

  setUp(() async {
    requestCount.clear();
    server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    httpBaseUrl = 'http://127.0.0.1:${server.port}';
    lastAliveTime = DateTime.now();
    server.listen((request) {
      final path = request.uri.path;
      final n = (requestCount[path] ?? 0) + 1;
      requestCount[path] = n;
      final resp = request.response;
      if (n <= 2) {
        resp.statusCode = 500;
        resp.close();
        return;
      }
      resp.statusCode = 200;
      resp.headers.set('content-length', '4');
      resp.add([1, 2, 3, 4]);
      resp.close();
    });
  });

  tearDown(() async {
    lastAliveTime = null;
    await server.close(force: true);
  });

  test('失败两次后第三次成功，退避 ≥300ms', () async {
    final image = RemoteImage(MockImgSyncerClient(), 'retry.jpg',
        size: 100, httpClient: http.Client());
    final sw = Stopwatch()..start();
    final result = await image.thumbnail();
    sw.stop();
    expect(result, isNotNull);
    expect(requestCount['/thumbnail/retry.jpg'], 3);
    expect(sw.elapsedMilliseconds, greaterThanOrEqualTo(290)); // 100+200ms 退避
  });
});
```

- [ ] **Step 3: 跑测试确认失败**

Run: `flutter test test/unit/storage_test.dart`
Expected: 新增两测试 FAIL（并发组 maxActive 达 ~20；退避组 elapsed < 290ms）。

- [ ] **Step 4: 实现**

`lib/storage/storage.dart`：顶部加 `import 'package:async_locks/async_locks.dart';`；模块级加 `final Semaphore _thumbnailSemaphore = Semaphore(6);`。`thumbnail()` 重写为（结构 Task 2 要沿用）：

```dart
Future<Uint8List?> thumbnail() async {
  if (thumbnailData != null) {
    return thumbnailData;
  }
  int maxRetries = 3;
  int retryCount = 0;
  while (retryCount < maxRetries) {
    try {
      await _thumbnailSemaphore.acquire();
      Uint8List bytes;
      try {
        final currentData = BytesBuilder();
        await for (final d in thumbnailStream()) {
          currentData.add(d);
        }
        bytes = currentData.takeBytes();
      } finally {
        _thumbnailSemaphore.release();
      }
      if (bytes.isEmpty) {
        throw Exception("empty thumbnail");
      }
      thumbnailData = bytes;
      return bytes;
    } catch (e) {
      logger.addLog("get $path thumbnail failed: $e");
      // 指数退避：100/200/400ms
      await Future.delayed(Duration(milliseconds: 100 << retryCount));
      retryCount++;
    }
  }
  // 失败不写入缓存，交给调用方重试（issue #89）
  return null;
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `flutter test test/unit/storage_test.dart && flutter analyze`
Expected: 全绿，0 error。

- [ ] **Step 6: Commit**

```bash
git add lib/storage/storage.dart test/unit/storage_test.dart
git commit -m "fix(storage): 远程缩略图并发限流6与指数退避；修复 dataStream 单测红"
```

---

### Task 2: 客户端缩略图磁盘缓存

**Files:**
- Create: `lib/storage/thumbnail_cache.dart`
- Modify: `lib/storage/storage.dart`（`thumbnail()` 插入缓存读写 + `thumbnailCacheLoader` 全局）
- Modify: `lib/util.dart`（`isValidImage` 从 asset.dart 移入）
- Modify: `lib/asset.dart`（删除本地 `isValidImage` 定义）
- Test: `test/unit/thumbnail_cache_test.dart`（新）、`test/unit/storage_test.dart`（追加组）

**Interfaces:**
- Consumes: Task 1 的 `thumbnail()` 结构（在 `if (thumbnailData != null)` 之后插缓存读；在 `thumbnailData = bytes; return bytes;` 前插缓存写）
- Produces: `class ThumbnailCache { ThumbnailCache(Directory baseDir); static int fnv1a64(String s); Future<Uint8List?> read(String path, int? size); Future<void> write(String path, int? size, Uint8List bytes); }`；全局 `Future<ThumbnailCache?> Function() thumbnailCacheLoader`（默认 `_defaultThumbnailCache`）；`Future<bool> isValidImage(Uint8List)` 移至 `lib/util.dart`（asset.dart 已 import util.dart，调用点不改）

- [ ] **Step 1: 写失败测试（thumbnail_cache_test.dart 全新文件）**

```dart
import 'dart:io';
import 'dart:typed_data';
import 'package:flutter_test/flutter_test.dart';
import 'package:img_syncer/storage/thumbnail_cache.dart';

void main() {
  late Directory tmp;
  setUp(() {
    tmp = Directory.systemTemp.createTempSync('thumb_cache');
  });
  tearDown(() {
    tmp.deleteSync(recursive: true);
  });

  group('fnv1a64 已知向量', () {
    test('空串 = offset basis', () => expect(ThumbnailCache.fnv1a64(''), 0xcbf29ce484222325));
    test('a', () => expect(ThumbnailCache.fnv1a64('a'), 0xaf63dc4c8601ec8c));
    test('foobar', () => expect(ThumbnailCache.fnv1a64('foobar'), 0x85944171f73967e8));
  });

  group('read/write', () {
    test('写后读命中（目录不存在则自动创建）', () async {
      final c = ThumbnailCache(Directory('${tmp.path}/sub'));
      await c.write('2024/03/05/a.jpg', 1024, Uint8List.fromList([1, 2, 3]));
      expect(await c.read('2024/03/05/a.jpg', 1024), [1, 2, 3]);
    });
    test('size 变化即 miss', () async {
      final c = ThumbnailCache(tmp);
      await c.write('a.jpg', 1, Uint8List.fromList([1]));
      expect(await c.read('a.jpg', 2), isNull);
    });
    test('未写过返回 null', () async {
      expect(await ThumbnailCache(tmp).read('none.jpg', 1), isNull);
    });
  });
}
```

- [ ] **Step 2: 写失败测试（storage_test.dart 追加缓存行为组）**

```dart
group('RemoteImage.thumbnail 磁盘缓存', () {
  late HttpServer server;
  final requestCount = <String, int>{};
  late Future<ThumbnailCache?> Function() origLoader;
  late Directory tmp;
  // 组级可切换的响应体；默认为真实可解码图片（仓库现有 Go 测试夹具）
  late Uint8List responseBody;
  final pic = File('test/static/test_pic_01.jpg').readAsBytesSync();

  setUp(() async {
    responseBody = pic;
    origLoader = thumbnailCacheLoader;
    tmp = Directory.systemTemp.createTempSync('thumb_inject');
    thumbnailCacheLoader = () async => ThumbnailCache(tmp);
    requestCount.clear();
    server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    httpBaseUrl = 'http://127.0.0.1:${server.port}';
    lastAliveTime = DateTime.now();
    server.listen((request) {
      final path = request.uri.path;
      requestCount[path] = (requestCount[path] ?? 0) + 1;
      final resp = request.response;
      resp.statusCode = 200;
      resp.headers.set('content-length', '${responseBody.length}');
      resp.add(responseBody);
      resp.close();
    });
  });

  tearDown(() async {
    thumbnailCacheLoader = origLoader;
    lastAliveTime = null;
    await server.close(force: true);
    tmp.deleteSync(recursive: true);
  });

  test('新实例命中缓存不再走网络（刷新轮次同理）', () async {
    final img1 = RemoteImage(MockImgSyncerClient(), 'a.jpg',
        size: 10, httpClient: http.Client());
    final r1 = await img1.thumbnail();
    // refreshRemote 会重建实例，内存缓存清零——模拟新实例
    final img2 = RemoteImage(MockImgSyncerClient(), 'a.jpg',
        size: 10, httpClient: http.Client());
    final r2 = await img2.thumbnail();
    expect(requestCount['/thumbnail/a.jpg'], 1);
    expect(r2, r1);
  });

  test('不可解码字节不落缓存（防毒化）', () async {
    responseBody = Uint8List.fromList(utf8.encode('not an image'));
    final img1 = RemoteImage(MockImgSyncerClient(), 'b.jpg',
        size: 10, httpClient: http.Client());
    await img1.thumbnail();
    final img2 = RemoteImage(MockImgSyncerClient(), 'b.jpg',
        size: 10, httpClient: http.Client());
    await img2.thumbnail();
    expect(requestCount['/thumbnail/b.jpg'], 2); // 无缓存，仍走网络
  });

  test('默认 loader 在无 path_provider 环境降级为 null', () async {
    // origLoader 是 setUp 里保存的默认实现；flutter test 环境
    // getApplicationSupportDirectory 抛 MissingPluginException → 应返回 null
    expect(await origLoader(), isNull);
  });
});
```

`storage_test.dart` 需新增 import：`dart:typed_data`、`package:img_syncer/storage/thumbnail_cache.dart`（`dart:convert`、`dart:io` 已有）。

- [ ] **Step 3: 跑测试确认失败**

Run: `flutter test test/unit/thumbnail_cache_test.dart test/unit/storage_test.dart`
Expected: thumbnail_cache 组 FAIL（类不存在）；storage 缓存组 FAIL（第一次走网络 2 次、fnv 未实现）。

- [ ] **Step 4: 实现**

**`lib/storage/thumbnail_cache.dart`（新文件）：**

```dart
import 'dart:io';
import 'dart:typed_data';

/// best-effort 缩略图磁盘缓存：一切异常吞掉，缓存故障不得影响缩略图加载。
class ThumbnailCache {
  final Directory baseDir;
  ThumbnailCache(this.baseDir);

  /// FNV-1a 64 位（Dart VM 64 位整数回绕），5 千张碰撞概率 ~1e-13
  static int fnv1a64(String s) {
    var h = 0xcbf29ce484222325;
    for (final c in s.codeUnits) {
      h ^= c;
      h = (h * 0x100000001b3) & 0xFFFFFFFFFFFFFFFF;
    }
    return h;
  }

  String _file(String path, int? size) =>
      '${baseDir.path}/${fnv1a64(path)}_${size ?? 0}';

  Future<Uint8List?> read(String path, int? size) async {
    try {
      final f = File(_file(path, size));
      if (await f.exists()) {
        return await f.readAsBytes();
      }
    } catch (_) {}
    return null;
  }

  Future<void> write(String path, int? size, Uint8List bytes) async {
    try {
      await baseDir.create(recursive: true);
      await File(_file(path, size)).writeAsBytes(bytes, flush: true);
    } catch (_) {}
  }
}
```

**`lib/util.dart`：** 从 `lib/asset.dart:668-675` 原样移入 `isValidImage`（需补 `import 'dart:typed_data';` 与 `import 'dart:ui' as ui;`），删除 asset.dart 中原定义（调用点不变，asset.dart 已 import util.dart）。

**`lib/storage/storage.dart`：** 加 import `package:path_provider/path_provider.dart`、`package:img_syncer/storage/thumbnail_cache.dart`；模块级加：

```dart
/// 可注入的缩略图缓存 loader；path_provider 不可用（测试环境）时返回 null 禁用缓存。
Future<ThumbnailCache?> Function() thumbnailCacheLoader = _defaultThumbnailCache;
Future<ThumbnailCache?> _defaultThumbnailCache() async {
  try {
    final dir = await getApplicationSupportDirectory();
    return ThumbnailCache(Directory('${dir.path}/thumbnails'));
  } catch (_) {
    return null;
  }
}
```

`thumbnail()` 在 Task 1 结构上插入缓存（仅标出插入点，其余不变）：

```dart
Future<Uint8List?> thumbnail() async {
  if (thumbnailData != null) {
    return thumbnailData;
  }
  // 磁盘缓存：命中不走网络、不占信号量
  final cache = await thumbnailCacheLoader();
  final cached = await cache?.read(path, size);
  if (cached != null) {
    thumbnailData = cached;
    return cached;
  }
  int maxRetries = 3;
  ... // Task 1 的信号量 + 退避逻辑不变
      thumbnailData = bytes;
      // 能解码成图片才落盘：防非法字节毒化缓存（issue #89 同思路）
      if (cache != null && await isValidImage(bytes)) {
        unawaited(cache.write(path, size, bytes));
      }
      return bytes;
```

- [ ] **Step 5: 跑测试确认通过**

Run: `flutter test test/unit/thumbnail_cache_test.dart test/unit/storage_test.dart && flutter analyze`
Expected: 全绿，0 error。

- [ ] **Step 6: 全量回归 + Commit**

Run: `flutter test`
Expected: 全绿。

```bash
git add lib/storage/thumbnail_cache.dart lib/storage/storage.dart lib/util.dart lib/asset.dart test/unit/thumbnail_cache_test.dart test/unit/storage_test.dart
git commit -m "feat(storage): 远程缩略图磁盘缓存（FNV 键 + 解码校验门禁）"
```

---

### Task 3: 服务端 S3 GET 并发上限 + 连接池

**Files:**
- Modify: `server/drive/s3/s3.go`
- Create: `server/drive/s3/sem_test.go`
- Modify: `server/drive/s3/s3_test.go`（suite 追加集成测试）

**Interfaces:**
- Produces: `S3` 新增字段 `sem chan struct{}`（cap 8，NewS3 里 `make`）；原 `download(ctx, p, offset)` 主体改名 `downloadInner`；新 `download` 为 wrapper；`type semBody struct`。`Download`/`DownloadWithOffset` 及所有调用点不需要改（仍调 `download`）。

- [ ] **Step 1: 写失败测试（sem_test.go 全新文件，无需 Docker）**

```go
package s3

import (
	"bytes"
	"io"
	"testing"
)

func TestSemBodyCloseReleasesOnce(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // 占用唯一槽位
	b := &semBody{rc: io.NopCloser(bytes.NewReader([]byte("x"))), sem: sem}
	p := make([]byte, 1)
	if _, err := b.Read(p); err != nil || p[0] != 'x' {
		t.Fatal("Read 未透传")
	}
	b.Close()
	b.Close() // 双 Close 不得二次释放
	if len(sem) != 0 {
		t.Fatalf("sem len = %d, want 0", len(sem))
	}
	select {
	case sem <- struct{}{}: // 槽位已释放，可重新占用
	default:
		t.Fatal("Close 后槽位未释放")
	}
}

func TestDownloadErrorReleasesSlot(t *testing.T) {
	// 端口 1 上无服务：连接拒绝，快速失败
	d, err := NewS3("http://127.0.0.1:1", "us-east-1", "ak", "sk", "b", "", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Download("x.jpg"); err == nil {
		t.Fatal("want error")
	}
	if len(d.sem) != 0 {
		t.Fatal("错误路径 sem 槽位泄漏")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./server/drive/s3/ -run 'TestSemBody|TestDownloadError' -v`
Expected: FAIL（`semBody`/`sem` 未定义，编译错误即预期失败）。

- [ ] **Step 3: 实现**

`server/drive/s3/s3.go`：

1. `S3` struct 加字段 `sem chan struct{}`；`NewS3` 返回值加 `sem: make(chan struct{}, 8)`。
2. 原 `download` 方法主体改名 `downloadInner`，新 `download` wrapper（算法）：

```go
// download：所有 GET 共享并发上限；body Close 时释放槽位。
func (d *S3) download(ctx context.Context, p string, offset int64) (io.ReadCloser, int64, error) {
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	rc, size, err := d.downloadInner(ctx, p, offset)
	if err != nil {
		<-d.sem
		return nil, 0, err
	}
	return &semBody{rc: rc, sem: d.sem}, size, nil
}

// semBody 在 Close 时释放并发槽位；sync.Once 防双释放。
type semBody struct {
	rc   io.ReadCloser
	sem  chan struct{}
	once sync.Once
}

func (b *semBody) Read(p []byte) (int, error) { return b.rc.Read(p) }
func (b *semBody) Close() error {
	err := b.rc.Close()
	b.once.Do(func() { <-b.sem })
	return err
}
```

（import 补 `"sync"`。`startTail` 内的 `getObjectRange`/`directGet` 尾片**不**经过 wrapper——每文件 4 连接是绕 TCP 慢启动的刻意设计，spec 已声明。）

3. 连接池：`NewS3` 里 `tr` 加 `tr.MaxIdleConnsPerHost = 16`；opts 追加 SDK 客户端同款 transport：

```go
opts = append(opts, config.WithHTTPClient(awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) error {
	t.ResponseHeaderTimeout = 60 * time.Second
	t.MaxIdleConnsPerHost = 16
	return nil
})))
```

- [ ] **Step 4: 跑单测确认通过**

Run: `go test ./server/drive/s3/ -run 'TestSemBody|TestDownloadError' -v && go vet ./server/drive/s3/ && go build ./...`
Expected: PASS（连接拒绝带 SDK 默认 3 次重试，约 1~2 秒属正常）；vet/build 无输出。

- [ ] **Step 5: 写集成测试（s3_test.go suite 追加，仅 S3_TEST_ENDPOINT 下运行）**

```go
func (s *S3TestSuite) TestConcurrentDownload() {
	d := s.newDrive("")
	key := "2026/11/25/concurrent.jpg"
	s.upload(d, key)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, _, err := d.Download(key)
			if err != nil {
				errs <- err
				return
			}
			b, err := io.ReadAll(rc)
			rc.Close() // 槽位在此释放；若泄漏，后续批次永久阻塞、测试超时失败
			if err == nil && !bytes.Equal(static.Pic1, b) {
				err = fmt.Errorf("content mismatch")
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		s.Require().NoError(err)
	}
}
```

- [ ] **Step 6: 集成测试（可选，需 Docker）**

Run: `docker compose -f test/docker-compose.yml up -d minio` 后 `S3_TEST_ENDPOINT=http://127.0.0.1:9000 go test ./server/drive/s3/ -v`
Expected: 全 PASS（含 TestConcurrentDownload）。无 Docker 时跳过，依赖 CI。

- [ ] **Step 7: Commit**

```bash
git add server/drive/s3/s3.go server/drive/s3/sem_test.go server/drive/s3/s3_test.go
git commit -m "fix(s3): GET 并发上限 8（Close 释放槽位）与空闲连接池修正"
```

---

## 收尾验证（全部任务完成后）

- [ ] `flutter analyze && flutter test && go build ./... && go vet ./server/drive/s3/`
- [ ] 手工端到端（真机 + S3 后端）：清 app 数据冷启动 → S3 监控并发 GET ≤ 8；下拉刷新 → S3 无新增缩略图 GET；二次冷启动 → S3 无缩略图 GET
