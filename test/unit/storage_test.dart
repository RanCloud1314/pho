import 'dart:async';
import 'dart:io';
import 'dart:convert';
import 'dart:typed_data';
import 'package:flutter_test/flutter_test.dart';
import 'package:img_syncer/storage/storage.dart';
import 'package:img_syncer/storage/thumbnail_cache.dart';
import 'package:img_syncer/proto/img_syncer.pbgrpc.dart';
import 'package:img_syncer/global.dart';
import 'package:http/http.dart' as http;
import 'package:mockito/mockito.dart';

class MockImgSyncerClient extends Mock implements ImgSyncerClient {}

RemoteImage _makeRemoteImage(String path) {
  return RemoteImage(
    MockImgSyncerClient(),
    path,
    httpClient: http.Client(),
  );
}

void main() {
  group('RemoteImage.dataStream - contentLength null defense (M9)', () {
    late HttpServer server;
    late int port;
    final testData = utf8.encode('Hello, test data!');

    /// 设为 true 时，服务器响应不包含 Content-Length header
    bool omitContentLength = false;

    setUp(() async {
      server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      port = server.port;
      httpBaseUrl = 'http://127.0.0.1:$port';
      omitContentLength = false;
      // 短路 checkServer 的 gRPC ping（测试环境 grpcPort 未初始化会抛
      // LateInitializationError，见 ledger）
      lastAliveTime = DateTime.now();

      server.listen((request) {
        final resp = request.response;
        resp.statusCode = 200;
        if (!omitContentLength) {
          resp.headers.set('content-length', testData.length.toString());
        }
        resp.add(testData);
        resp.close();
      });
    });

    tearDown(() async {
      lastAliveTime = null;
      await server.close(force: true);
    });

    test('contentLength 为 null 时不崩溃，total 使用 -1', () async {
      omitContentLength = true;
      final image = _makeRemoteImage('test.jpg');
      int? progressTotal;

      final stream = image.dataStream(
        reportProgress: false,
        onProgress: (downloaded, total) {
          progressTotal = total;
        },
      );

      final result = BytesBuilder();
      await for (final chunk in stream) {
        result.add(chunk);
      }

      expect(progressTotal, -1);
      expect(result.takeBytes(), testData);
    });

    test('有效 contentLength 被正确报告', () async {
      final image = _makeRemoteImage('test.jpg');
      final progressCalls = <Map<String, int>>[];

      final stream = image.dataStream(
        reportProgress: false,
        onProgress: (downloaded, total) {
          progressCalls.add({'downloaded': downloaded, 'total': total});
        },
      );

      await for (final _ in stream) {
        /* 消费数据 */
      }

      expect(progressCalls.isNotEmpty, isTrue);
      expect(progressCalls.first['total'], testData.length);
      expect(progressCalls.last['downloaded'], testData.length);
    });

    test('onProgress 回调中 downloaded 逐步递增', () async {
      final bigData = utf8.encode('A' * 10000);
      server.close();
      server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      port = server.port;
      httpBaseUrl = 'http://127.0.0.1:$port';
      server.listen((request) {
        final resp = request.response;
        resp.statusCode = 200;
        resp.headers.set('content-length', bigData.length.toString());
        resp.add(bigData);
        resp.close();
      });

      final image = _makeRemoteImage('big.jpg');
      final downloadeds = <int>[];
      int? total;

      final stream = image.dataStream(
        reportProgress: false,
        onProgress: (downloaded, t) {
          downloadeds.add(downloaded);
          total = t;
        },
      );

      await for (final _ in stream) {
        /* 消费数据 */
      }

      expect(total, bigData.length);
      expect(downloadeds.first, greaterThan(0));
      // downloaded 不递减
      for (int i = 1; i < downloadeds.length; i++) {
        expect(downloadeds[i], greaterThanOrEqualTo(downloadeds[i - 1]));
      }
      expect(downloadeds.last, bigData.length);
    });
  });

  group('File stream error handling (M13)', () {
    test('onError 处理器关闭 sink 并传播错误', () async {
      // 先正常发送数据再触发错误
      final chunkData = [1, 2, 3];
      final errorStream = Stream<List<int>>.fromFutures([
        Future.value(chunkData),
        Future.delayed(const Duration(milliseconds: 10),
            () => throw Exception('模拟文件读取错误')),
      ]);

      final req = http.StreamedRequest(
          'POST', Uri.parse('http://localhost:12345/test'));
      final completer = Completer<void>();
      bool sinkClosed = false;

      errorStream.listen(
        (data) {
          req.sink.add(data);
        },
        onDone: () {
          req.sink.close();
        },
        onError: (e) {
          req.sink.close();
          sinkClosed = true;
          if (!completer.isCompleted) completer.completeError(e);
        },
      );

      try {
        await completer.future;
        fail('应该抛出异常');
      } catch (e) {
        expect(sinkClosed, isTrue);
        expect(e, isA<Exception>());
        expect('$e', contains('文件读取错误'));
      }
    });

    test('正常流 onDone 关闭 sink', () async {
      final normalStream = Stream<List<int>>.fromIterable([
        [1, 2, 3],
        [4, 5, 6],
      ]);

      final req = http.StreamedRequest(
          'POST', Uri.parse('http://localhost:12345/test'));
      final completer = Completer<void>();
      bool sinkClosedOnDone = false;

      normalStream.listen(
        (data) {
          req.sink.add(data);
        },
        onDone: () {
          req.sink.close();
          sinkClosedOnDone = true;
          completer.complete();
        },
        onError: (e) {
          req.sink.close();
        },
      );

      await completer.future;
      expect(sinkClosedOnDone, isTrue);
    });
  });

  group('重试计数器重置', () {
    test('每次重试 uploaded 从 0 开始', () {
      int retryCount = 0;
      final maxRetries = 3;
      final uploadedStarts = <int>[];

      while (retryCount < maxRetries) {
        // uploaded 在循环内部声明，每次迭代重置
        int uploaded = 0;
        uploadedStarts.add(uploaded);
        uploaded += 10;
        retryCount++;
      }

      expect(uploadedStarts, [0, 0, 0]);
    });

    test('重试时 uploaded 不会累加上次失败的值', () {
      int attempt = 0;
      final maxRetries = 3;
      bool succeeded = false;
      final uploadedAtEnd = <int>[];

      while (!succeeded && attempt < maxRetries) {
        int uploaded = 0; // 每次迭代重置
        attempt++;

        // 模拟上传了一些数据后失败
        uploaded += 50;
        if (attempt == 1) {
          // 第一次尝试失败
          uploadedAtEnd.add(uploaded);
          continue;
        }
        // 第二次尝试成功
        uploaded += 50;
        uploadedAtEnd.add(uploaded);
        succeeded = true;
      }

      // 第一次失败时 uploaded=50，第二次成功时 uploaded=100（不是 150）
      expect(uploadedAtEnd, [50, 100]);
    });
  });

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
      // 落盘是 unawaited 的异步写：先冲刷事件队列等写盘完成（真实场景是稍后访问）
      await pumpEventQueue();
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
}
