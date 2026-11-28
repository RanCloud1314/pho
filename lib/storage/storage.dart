import 'dart:convert';
import 'package:shared_preferences/shared_preferences.dart';
import 'package:img_syncer/setting_storage_route.dart';
import 'dart:async';
import 'dart:io';
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
    await checkServer();
    final file = await asset.originFile;
    if (file == null) {
      throw Exception("asset file is null");
    }
    String? name = asset.title;
    name ??= await asset.titleAsync;
    // print("upload $name");
    var date = asset.createDateTime;
    if (date.isBefore(DateTime(1990, 1, 1))) {
      date = asset.modifiedDateTime;
    }
    final dateStr =
        formatDate(date, [yyyy, ':', mm, ':', dd, ' ', HH, ':', nn, ':', ss]);
    var thumbnailSize = const ThumbnailSize.square(200);
    if (asset.type == AssetType.video) {
      thumbnailSize = const ThumbnailSize.square(800);
    }
    final thumbnailData =
        await asset.thumbnailDataWithSize(thumbnailSize, quality: 90);
    // 缩略图生成失败（视频/HEIC 等）时降级：跳过缩略图继续上传原件（issue #64/#57）
    File? liveVideoFile;
    final imgLen = await file.length();
    final thumbLen = thumbnailData?.length ?? 0;
    int totalLen = imgLen + thumbLen;
    stateModel.updateUploadProgress(asset.id, 1, totalLen);
    int maxRetries = 3;
    int retryCount = 0;
    bool succeeded = false;
    while (!succeeded) {
      int uploaded = 0;
      try {
        // live 视频在重试循环内重新获取：iCloud 优化存储下视频可能未下载，
        // 首轮拿不到（返回 null），重试时可能已下载完成。
        // 拿不到视频时不静默跳过——抛异常走 catch，失败则不加入 syncedIDs。
        liveVideoFile = null;
        if (asset.isLivePhoto) {
          liveVideoFile = await asset.originFileWithSubtype;
          if (liveVideoFile == null) {
            throw Exception(
                "live video file not available (iCloud not downloaded?)");
          }
          totalLen = imgLen + thumbLen + await liveVideoFile.length();
        }
        // upload thumbnail（缩略图缺失时跳过，不中止上传）
        if (thumbnailData != null) {
          final thumbRsp = await http.post(
            Uri.parse("$httpBaseUrl/thumbnail/$name"),
            body: thumbnailData,
            headers: {
              'Image-Date': dateStr,
              'Image-Is-Live-Photo': asset.isLivePhoto ? "true" : "false",
              'Image-Encrypt-Type': settingModel.enableEncrypt
                  ? encryptionTypeName(settingModel.encryptionType)
                  : "None",
              'Image-Encrypt-Password': settingModel.enableEncrypt
                  ? settingModel.encryptionPassword
                  : "",
            },
          );
          stateModel.updateUploadProgress(
              asset.id, uploaded + thumbLen, totalLen);
          if (thumbRsp.statusCode != 200) {
            stateModel.finishUpload(asset.id, false);
            logger.addLog(
                "upload $name thumbnail failed: ${thumbRsp.statusCode} ${thumbRsp.body}");
            throw Exception(
                "upload thumbnail failed: [${thumbRsp.statusCode}] ${thumbRsp.body}");
          }
        }
        // upload origin image
        var req = http.StreamedRequest("POST", Uri.parse("$httpBaseUrl/$name"));
        req.headers['Image-Date'] = dateStr;
        req.headers['Image-Is-Live-Photo'] =
            asset.isLivePhoto ? "true" : "false";
        if (settingModel.enableEncrypt) {
          req.headers['Image-Encrypt-Type'] =
              encryptionTypeName(settingModel.encryptionType);
          req.headers['Image-Encrypt-Password'] =
              settingModel.encryptionPassword;
        }
        req.contentLength = await file.length();
        file.openRead().listen((chunk) {
          uploaded += chunk.length;
          stateModel.updateUploadProgress(
              asset.id, uploaded + thumbLen, totalLen);
          req.sink.add(chunk);
        }, onDone: () {
          req.sink.close();
        }, onError: (e) {
          req.sink.close();
          throw Exception("file read error: $e");
        });
        final response = await req.send();
        if (response.statusCode != 200) {
          stateModel.finishUpload(asset.id, false);
          final body = await response.stream.bytesToString();
          throw Exception("upload failed: [${response.statusCode}] $body");
        }
        // upload Live Photo video
        if (asset.isLivePhoto && liveVideoFile != null) {
          final videoName = await asset.titleAsyncWithSubtype;
          var req = http.StreamedRequest(
              "POST", Uri.parse("$httpBaseUrl/live/$videoName"));
          req.headers['Image-Date'] = dateStr;
          req.headers['Image-Is-Live-Photo'] =
              asset.isLivePhoto ? "true" : "false";
          if (settingModel.enableEncrypt) {
            req.headers['Image-Encrypt-Type'] =
                encryptionTypeName(settingModel.encryptionType);
            req.headers['Image-Encrypt-Password'] =
                settingModel.encryptionPassword;
          }
          req.contentLength = await liveVideoFile.length();
          liveVideoFile.openRead().listen((chunk) {
            uploaded += chunk.length;
            stateModel.updateUploadProgress(
                asset.id, uploaded + thumbLen, totalLen);
            req.sink.add(chunk);
          }, onDone: () {
            req.sink.close();
          }, onError: (e) {
            req.sink.close();
            throw Exception("file read error: $e");
          });
          final response = await req.send();
          if (response.statusCode != 200) {
            stateModel.finishUpload(asset.id, false);
            final body = await response.stream.bytesToString();
            throw Exception("upload failed: [${response.statusCode}] $body");
          }
        }
        stateModel.finishUpload(asset.id, true);
        succeeded = true;
      } catch (e) {
        stateModel.finishUpload(asset.id, false);
        retryCount++;
        if (retryCount >= maxRetries) {
          logger.addLog("upload $name failed: $e");
          rethrow;
        }
      } finally {
        if (shouldDeleteLocalFileAfterUpload(
            isIOS: Platform.isIOS, uploadSucceeded: succeeded)) {
          Future.delayed(const Duration(milliseconds: 200), () {
            try {
              file.deleteSync();
              liveVideoFile?.deleteSync();
            } catch (e) {
              logger.addLog("delete file failed: $e");
            }
          });
        }
      }
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
