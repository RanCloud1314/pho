import 'dart:convert';
import 'package:shared_preferences/shared_preferences.dart';
import 'package:img_syncer/setting_storage_route.dart';
import 'dart:async';
import 'dart:io';
import 'dart:math';
import 'dart:typed_data';
import 'package:grpc/grpc.dart';
import 'package:img_syncer/logger/logger.dart';
import 'package:img_syncer/proto/img_syncer.pbgrpc.dart';
import 'package:date_format/date_format.dart';
import 'package:img_syncer/state_model.dart';
import 'package:path/path.dart';
import 'package:photo_manager/photo_manager.dart';
import 'package:img_syncer/global.dart';
import 'package:http/http.dart' as http;
import 'package:async_locks/async_locks.dart';
import 'package:path_provider/path_provider.dart';
import 'package:img_syncer/storage/thumbnail_cache.dart';
import 'package:flutter/services.dart';
import 'package:flutter_image_compress/flutter_image_compress.dart';
import 'package:img_syncer/util.dart';
import 'package:img_syncer/storage/storage_interface.dart';

/// 将 EncryptionType 转为 HTTP 头字符串
String encryptionTypeName(EncryptionType type) {
  switch (type) {
    case EncryptionType.none:
      return 'None';
    case EncryptionType.aesCfb:
      return 'AES_128_CFB';
    case EncryptionType.aesGcm:
      return 'AES_256_GCM';
  }
}

bool reportForegroundUploadProgress = true;

RemoteStorage storage = RemoteStorage("127.0.0.1", 10000);

/// 可注入的缩略图缓存 loader；path_provider 不可用（测试环境）时返回 null 禁用缓存。
Future<ThumbnailCache?> Function() thumbnailCacheLoader =
    _defaultThumbnailCache;
Future<ThumbnailCache?> _defaultThumbnailCache() async {
  try {
    final dir = await getApplicationSupportDirectory();
    return ThumbnailCache(Directory('${dir.path}/thumbnails'));
  } catch (_) {
    return null;
  }
}

/// 上传完成后是否删除本地源文件：仅 iOS 且上传成功时删除。
/// 原 finally 块无条件删除，同步失败时源文件会丢失（issue #83/#82）。
bool shouldDeleteLocalFileAfterUpload({
  required bool isIOS,
  required bool uploadSucceeded,
}) =>
    isIOS && uploadSucceeded;

/// 测试注入用的 override，非 null 时替换 storage 全局变量
RemoteStorageClient? _storageOverride;
RemoteStorageClient get storageClient => _storageOverride ?? storage;
void setStorageForTest(RemoteStorageClient v) => _storageOverride = v;

Future<String> remoteAlbumNamespace() async {
  final prefs = await SharedPreferences.getInstance();
  final drive = prefs.getString('drive') ?? 'SMB';
  final keys = <Drive, List<String>>{
    Drive.smb: ['addr', 'username', 'share', 'rootPath'],
    Drive.webDav: ['webdav_url', 'webdav_username', 'webdav_root_path'],
    Drive.nfs: ['nfs_url', 'nfs_root_path'],
    Drive.s3: [
      's3_endpoint',
      's3_region',
      's3_access_key_id',
      's3_bucket',
      's3_root'
    ],
  }[getDrive(drive)]!;
  return jsonEncode([
    drive,
    ...keys.map(prefs.get),
  ]);
}

Map<String, String> remoteMediaHeaders() => {
      if (settingModel.enableEncrypt) ...{
        'Image-Encrypt-Type': encryptionTypeName(settingModel.encryptionType),
        'Image-Encrypt-Password': settingModel.encryptionPassword,
      },
    };

class _UploadRequest extends http.BaseRequest {
  _UploadRequest(Uri uri, this.body) : super('POST', uri);

  final Stream<List<int>> body;

  @override
  http.ByteStream finalize() {
    super.finalize();
    return http.ByteStream(body);
  }
}

class RemoteStorage implements RemoteStorageClient {
  int bufferSize = 1024 * 1024;

  /// 共享 HTTP 客户端，复用连接减少 TCP 握手开销
  http.Client httpClient = http.Client();
  ImgSyncerClient cli = ImgSyncerClient(ClientChannel(
    "127.0.0.1",
    port: 50051,
    options: const ChannelOptions(
      credentials: ChannelCredentials.insecure(),
      keepAlive: ClientKeepAliveOptions(
        pingInterval: Duration(seconds: 20),
        permitWithoutCalls: true,
      ),
    ),
  ));
  RemoteStorage(String addr, int port) {
    final channel = ClientChannel(
      addr,
      port: port,
      options: const ChannelOptions(
        credentials: ChannelCredentials.insecure(),
      ),
    );
    cli = ImgSyncerClient(channel);
  }

  Future<void> uploadXFile(XFile file) async {
    await checkServer();
    final name = basename(file.path);
    final date = await file.lastModified();
    final dateStr =
        formatDate(date, [yyyy, ':', mm, ':', dd, ' ', HH, ':', nn, ':', ss]);
    var thumbnailSize = 200;
    if (isVideoByPath(file.path)) {
      thumbnailSize = 800;
    }
    var thumbnailData = await FlutterImageCompress.compressWithFile(
      file.path,
      minWidth: thumbnailSize,
      minHeight: thumbnailSize,
      quality: 90,
    );
    int uploaded = 0;
    final imgLen = await file.length();
    // final thumbLen = thumbnailData!.length;
    final totalLen = imgLen;
    var req = http.StreamedRequest("POST", Uri.parse("$httpBaseUrl/$name"));
    req.headers['Image-Date'] = dateStr;
    req.contentLength = imgLen;
    file.openRead().listen((chunk) {
      uploaded += chunk.length;
      req.sink.add(chunk);
    }, onDone: () {
      req.sink.close();
    }, onError: (e) {
      req.sink.close();
      throw Exception("file read error: $e");
    });
    final response = await req.send();
    if (response.statusCode != 200) {
      throw Exception("upload failed: ${response.statusCode}");
    }
    final thumbRsp = await http.post(
      Uri.parse("$httpBaseUrl/thumbnail/$name"),
      body: thumbnailData,
      headers: {
        'Image-Date': dateStr,
      },
    );
    if (thumbRsp.statusCode != 200) {
      throw Exception("upload thumbnail failed: ${thumbRsp.statusCode}");
    }
  }

  Future<void> uploadAssetEntity(AssetEntity asset) async {
    void stage(UploadPhase phase) =>
        stateModel.updateUploadProgress(asset.id, UploadProgress(phase));

    stage(UploadPhase.preparing);
    var succeeded = false;
    File? file;
    File? liveVideoFile;
    try {
      await checkServer();
      final origin = await asset.originFile;
      if (origin == null) throw Exception('asset file is null');
      file = origin;
      final name = asset.title ?? await asset.titleAsync;
      final date = asset.createDateTime.isBefore(DateTime(1990, 1, 1))
          ? asset.modifiedDateTime
          : asset.createDateTime;
      final dateStr =
          formatDate(date, [yyyy, ':', mm, ':', dd, ' ', HH, ':', nn, ':', ss]);
      final thumbnail = await asset.thumbnailDataWithSize(
          ThumbnailSize.square(asset.type == AssetType.video ? 800 : 200),
          quality: 90);
      final originSize = await origin.length();
      final headers = {
        'Image-Date': dateStr,
        'Image-Is-Live-Photo': asset.isLivePhoto.toString(),
        ...remoteMediaHeaders(),
      };
      final prefs = await SharedPreferences.getInstance();
      final base = Uri.parse(httpBaseUrl);
      final track = reportForegroundUploadProgress &&
          base.host == '127.0.0.1' &&
          getDrive(prefs.getString('drive') ?? 'SMB') == Drive.s3;

      for (var attempt = 1; attempt <= 3; attempt++) {
        try {
          liveVideoFile = null;
          if (asset.isLivePhoto) {
            liveVideoFile = await asset.originFileWithSubtype;
            if (liveVideoFile == null) {
              throw Exception('Live Photo video file is unavailable');
            }
          }
          if (thumbnail != null) {
            await _uploadComponent(asset.id, base, ['thumbnail', name],
                'thumbnail', Stream.value(thumbnail), thumbnail.length,
                headers, track);
          }
          await _uploadComponent(asset.id, base, [name], 'original',
              origin.openRead(), originSize, headers, track);
          final video = liveVideoFile;
          if (video != null) {
            final videoName = await asset.titleAsyncWithSubtype;
            await _uploadComponent(asset.id, base, ['live', videoName],
                'Live Photo video', video.openRead(), await video.length(),
                headers, track);
          }
          succeeded = true;
          break;
        } catch (error) {
          if (attempt == 3) {
            logger.addLog('upload $name failed: $error');
            rethrow;
          }
          stage(UploadPhase.retrying);
          await Future<void>.delayed(Duration(seconds: attempt));
        }
      }
    } finally {
      stateModel.finishUpload(asset.id, succeeded);
      if (shouldDeleteLocalFileAfterUpload(
          isIOS: Platform.isIOS, uploadSucceeded: succeeded)) {
        Future.delayed(const Duration(milliseconds: 200), () {
          try {
            file?.deleteSync();
            liveVideoFile?.deleteSync();
          } catch (error) {
            logger.addLog('delete file failed: $error');
          }
        });
      }
    }
  }

  Future<void> _uploadComponent(
      String assetId,
      Uri base,
      List<String> path,
      String component,
      Stream<List<int>> body,
      int length,
      Map<String, String> headers,
      bool track) async {
    void update(UploadProgress progress) =>
        stateModel.updateUploadProgress(assetId, progress);

    update(const UploadProgress(UploadPhase.uploading));
    var active = true;
    var polling = false;
    final random = track ? Random.secure() : null;
    final id = random == null
        ? null
        : List.generate(
            16, (_) => random.nextInt(256).toRadixString(16).padLeft(2, '0'),
          ).join();
    final uri = base.replace(
      pathSegments: ['_pho', 'upload-progress', if (id != null) id],
    );
    final progressClient = track ? http.Client() : null;
    final timer = progressClient == null
        ? null
        : Timer.periodic(const Duration(seconds: 1), (_) async {
            if (polling) return;
            polling = true;
            try {
              final response = await progressClient
                  .get(uri)
                  .timeout(const Duration(seconds: 1));
              if (!active) return;
              if (response.statusCode == 200) {
                final data = jsonDecode(response.body) as Map<String, dynamic>;
                update(UploadProgress(
                  data['phase'] == 'confirming'
                      ? UploadPhase.confirming
                      : UploadPhase.uploading,
                  confirmed: data['confirmed'] as int,
                  total: data['total'] as int,
                  rateBps: (data['rate'] as num).toDouble(),
                ));
              } else {
                update(const UploadProgress(UploadPhase.uploading));
              }
            } catch (_) {
              if (active) update(const UploadProgress(UploadPhase.uploading));
            } finally {
              polling = false;
            }
          });
    try {
      final stream = body.transform(
        StreamTransformer<List<int>, List<int>>.fromHandlers(
          handleDone: (sink) {
            if (active && !track) {
              update(const UploadProgress(UploadPhase.confirming));
            }
            sink.close();
          },
        ),
      );
      final request = _UploadRequest(base.replace(pathSegments: path), stream)
        ..contentLength = length
        ..headers.addAll(headers);
      if (id != null) request.headers['Image-Upload-ID'] = id;
      final response = await httpClient.send(request);
      if (response.statusCode != 200) {
        final reason = (await response.stream.bytesToString()).trim();
        throw HttpException(
            reason.isEmpty ? 'HTTP ${response.statusCode}' : reason);
      }
      await response.stream.drain<void>();
    } catch (error) {
      throw Exception(
          '$component · ${error is HttpException ? error.message : error}');
    } finally {
      active = false;
      timer?.cancel();
      progressClient?.close();
    }
  }

  // @protected
  // Stream<UploadRequest> uploadStream(Stream<List<int>> dataReader,
  //     Stream<Uint8List> thumbnailReader, String name, date) async* {
  //   yield UploadRequest(name: name, date: date);
  //   await for (var data in dataReader) {
  //     yield UploadRequest(data: data);
  //   }
  //   await for (var data in thumbnailReader) {
  //     yield UploadRequest(thumbnailData: data);
  //   }
  // }

  Future<List<RemoteImage>> listImages(String date, int offset, maxReturn,
      {String afterPath = ''}) async {
    final namespace = await remoteAlbumNamespace();
    final rsp = await cli.listByDate(
      ListByDateRequest(
        date: date,
        offset: offset,
        maxReturn: maxReturn,
        afterPath: afterPath,
      ),
      options: CallOptions(timeout: const Duration(seconds: 60)),
    );
    if (!rsp.success) {
      throw Exception("list images failed: ${rsp.message}");
    }
    return rsp.infos
        .map((e) => RemoteImage(cli, e.path.replaceAll('\\', '/'),
            size: e.size.toInt(),
            isLivePhoto: e.isLivePhoto,
            httpClient: httpClient,
            cacheNamespace: namespace))
        .toList();
  }
}

class RemoteImage {
  ImgSyncerClient cli;
  String path;
  int? size;
  final String? cacheNamespace;
  Uint8List? thumbnailData;
  bool isLivePhoto = false;

  /// 共享 HTTP 客户端，复用连接减少 TCP 握手开销
  late http.Client httpClient;

  /// 远程缩略图全局并发上限：冷启动窗口预加载 + 刷新轮次叠加曾把
  /// S3 打出 214 个并发 GET（spec: 冷启动优化）
  static final Semaphore _thumbnailSemaphore = Semaphore(6);

  RemoteImage(
    this.cli,
    this.path, {
    this.thumbnailData,
    this.cacheNamespace,
    this.size,
    this.isLivePhoto = false,
    required this.httpClient,
  });

  bool isVideo() {
    return isVideoByPath(path);
  }

  Uri mediaUri({String prefix = ''}) => Uri.parse(httpBaseUrl).replace(
        pathSegments: [
          if (prefix.isNotEmpty) prefix,
          ...path.split('/').where((part) => part.isNotEmpty),
        ],
      );

  static const _stallTimeout = Duration(seconds: 30);

  Stream<Uint8List> thumbnailStream({Future<void>? cancel}) =>
      _mediaStream(prefix: 'thumbnail', cancel: cancel);

  Stream<Uint8List> _mediaStream({
    String prefix = '',
    Future<void>? cancel,
    void Function(int downloaded, int total)? progress,
  }) async* {
    await checkServer();
    final abort = Completer<void>();
    final request = http.AbortableRequest('GET', mediaUri(prefix: prefix),
        abortTrigger:
            cancel == null ? abort.future : Future.any([abort.future, cancel]));
    request.headers.addAll(remoteMediaHeaders());
    try {
      final response = await httpClient.send(request).timeout(_stallTimeout);
      if (response.statusCode != 200) {
        throw HttpException('HTTP ${response.statusCode}', uri: request.url);
      }
      final total = response.contentLength ?? -1;
      var received = 0;
      progress?.call(0, total);
      await for (final chunk in response.stream.timeout(_stallTimeout)) {
        received += chunk.length;
        progress?.call(received, total);
        yield chunk is Uint8List ? chunk : Uint8List.fromList(chunk);
      }
      if (total >= 0 && received != total) {
        throw const HttpException('Incomplete download');
      }
    } finally {
      abort.complete();
    }
  }

  Future<Uint8List?> thumbnail({Future<void>? cancel}) async {
    if (thumbnailData != null) {
      return thumbnailData;
    }
    // 磁盘缓存：命中不走网络、不占信号量（刷新重建实例后的二次拉取也在此命中）
    final namespace = cacheNamespace;
    final encrypted = path.toLowerCase().endsWith('.aes');
    final cache = namespace == null || encrypted || settingModel.enableEncrypt
        ? null
        : await thumbnailCacheLoader();
    final cacheKey = jsonEncode([namespace, path]);
    // 校验和由磁盘缓存检查，命中时无需重复解码。
    final cached = await cache?.read(cacheKey, size);
    if (cached != null) {
      thumbnailData = cached;
      return cached;
    }
    var cancelled = false;
    cancel?.then((_) => cancelled = true);
    int maxRetries = 3;
    int retryCount = 0;
    while (!cancelled && retryCount < maxRetries) {
      try {
        await _thumbnailSemaphore.acquire();
        Uint8List bytes;
        try {
          if (cancelled) return null;
          final currentData = BytesBuilder();
          final dataStream = thumbnailStream(cancel: cancel);
          await for (var d in dataStream) {
            currentData.add(d);
          }
          bytes = currentData.takeBytes();
        } finally {
          _thumbnailSemaphore.release();
        }
        if (bytes.isEmpty) {
          throw Exception("empty thumbnail");
        }
        // 能解码成图片才落盘：防非法字节毒化磁盘缓存（issue #89 同思路）
        if (!await isValidImage(bytes)) return null;
        thumbnailData = bytes;
        if (cache != null) unawaited(cache.write(cacheKey, size, bytes));
        return bytes;
      } on http.RequestAbortedException {
        return null;
      } on TimeoutException {
        return null;
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

  Stream<Uint8List> dataStream({
    bool reportProgress = true,
    Future<void>? cancel,
    void Function(int downloaded, int total)? onProgress,
  }) async* {
    final id = basename(path);
    var succeeded = false;
    try {
      yield* _mediaStream(
          cancel: cancel,
          progress: (received, total) {
            if (reportProgress)
              stateModel.updateDownloadProgress(id, received, total);
            onProgress?.call(received, total);
          });
      succeeded = true;
    } finally {
      if (reportProgress) stateModel.finishDownload(id, succeeded);
    }
  }

  Future<void> saveTo(File destination) async {
    final temporary = await destination.parent.createTemp('.pho-download-');
    try {
      final staged = File(join(temporary.path, 'download'));
      final output = await staged.open(mode: FileMode.write);
      try {
        await for (final chunk in dataStream()) {
          await output.writeFrom(chunk);
        }
      } finally {
        await output.close();
      }
      await staged.rename(destination.path);
    } finally {
      await temporary.delete(recursive: true);
    }
  }

  Future<Uint8List> imageData({
    bool reportProgress = true,
    Future<void>? cancel,
    void Function(int downloaded, int total)? onProgress,
  }) async {
    final bytes = BytesBuilder(copy: false);
    await for (final chunk in dataStream(
      reportProgress: reportProgress,
      cancel: cancel,
      onProgress: onProgress,
    )) {
      bytes.add(chunk);
    }
    return bytes.takeBytes();
  }
}
