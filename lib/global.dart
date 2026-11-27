import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:img_syncer/l10n/app_localizations.dart';
import 'package:img_syncer/logger/logger.dart';
import 'package:img_syncer/proto/img_syncer.pbgrpc.dart';
import 'package:img_syncer/run_server.dart';
import 'package:img_syncer/state_model.dart';
import 'package:img_syncer/storage/storage.dart';
import 'package:img_syncer/sync_timer.dart';
import 'package:img_syncer/setting_storage_route.dart';
import 'package:img_syncer/util.dart';
import 'package:photo_manager/photo_manager.dart';
import 'package:shared_preferences/shared_preferences.dart';
import 'dart:io';

import 'event_bus.dart';

late String httpBaseUrl;
late int grpcPort;
late int httpPort;
bool useRemoteServer = false;
bool isDebug = false;

Color? seedColor;

class Global {
  static Future init() async {
    assert(() {
      if (!Platform.isIOS) {
        isDebug = true;
      }
      return true;
    }());
    runServer().then((portsStr) async {
      final ports = portsStr.split(",");
      if (ports.length != 2) {
        logger.addLog("grpc server start failed");
        return;
      }
      httpBaseUrl = "http://127.0.0.1:${ports[1]}";
      grpcPort = int.parse(ports[0]);
      httpPort = int.parse(ports[1]);
      storage = RemoteStorage("127.0.0.1", int.parse(ports[0]));
      if (useRemoteServer) {
        httpBaseUrl = "http://192.168.100.213:8000";
        storage = RemoteStorage("192.168.100.213", 50051);
      }

      final prefs = await SharedPreferences.getInstance();
      if (isDesktop()) {
        final galleryColumCount = prefs.getInt("galleryColumCount");
        if (galleryColumCount != null) {
          settingModel.setGalleryColumCount(galleryColumCount);
        } else {
          settingModel.setGalleryColumCount(10);
        }
        var enableEncrypt = prefs.getBool("enable_encrypt");
        // 从旧 key (enalble_encrypt) 迁移
        if (enableEncrypt == null) {
          enableEncrypt = prefs.getBool("enalble_encrypt");
          if (enableEncrypt != null) {
            prefs.setBool("enable_encrypt", enableEncrypt);
            prefs.remove("enalble_encrypt");
          }
        }
        if (enableEncrypt != null) {
          settingModel.setEncryptSwitch(enableEncrypt);
        }
        final encryptionType = prefs.getInt("encryption_type");
        if (encryptionType != null) {
          settingModel.setEncryptionType(EncryptionType.values[encryptionType]);
        }
        final encPassword = prefs.getString("encryption_password");
        if (encPassword != null) {
          settingModel.setEncryptionPassword(encPassword);
        }
        await loadAdvancedPrefs();
        await initDrive();
        return;
      }
      var enableEncrypt = prefs.getBool("enable_encrypt");
      // 从旧 key (enalble_encrypt) 迁移
      if (enableEncrypt == null) {
        enableEncrypt = prefs.getBool("enalble_encrypt");
        if (enableEncrypt != null) {
          prefs.setBool("enable_encrypt", enableEncrypt);
          prefs.remove("enalble_encrypt");
        }
      }
      if (enableEncrypt != null) {
        settingModel.setEncryptSwitch(enableEncrypt);
      }
      final encryptionType = prefs.getInt("encryption_type");
      if (encryptionType != null) {
        settingModel.setEncryptionType(EncryptionType.values[encryptionType]);
      }
      final encPassword = prefs.getString("encryption_password");
      if (encPassword != null) {
        settingModel.setEncryptionPassword(encPassword);
      }
      await loadAdvancedPrefs();
      final seedColorValue = prefs.getInt("seed_color");
      if (seedColorValue != null) {
        seedColor = Color(seedColorValue);
      }
      final galleryColumCount = prefs.getInt("galleryColumCount");
      if (galleryColumCount != null) {
        settingModel.setGalleryColumCount(galleryColumCount);
      }

      final localFolder = prefs.getString("localFolder");
      if (localFolder != null && localFolder != "") {
        settingModel.setLocalFolder(localFolder);
        if (localFolder != "") {
          eventBus.fire(LocalRefreshEvent(refreshUnSync: false));
        }
      } else {
        await requestPermission(alert: false);
        if (Platform.isIOS) {
          settingModel.setLocalFolder("Recents");
          await prefs.setString("localFolder", "Recents");
          eventBus.fire(LocalRefreshEvent(refreshUnSync: true));
        } else {
          final List<AssetPathEntity> paths =
              await PhotoManager.getAssetPathList(
                  type: RequestType.common, hasAll: true);
          final Map<AssetPathEntity, int> assetCountMap = {
            for (final p in paths) p: await p.assetCountAsync,
          };
          paths.sort((a, b) =>
              (assetCountMap[b] ?? 0).compareTo(assetCountMap[a] ?? 0));
          if (paths.isNotEmpty) {
            settingModel.setLocalFolder(paths[0].name);
            await prefs.setString("localFolder", paths[0].name);
            eventBus.fire(LocalRefreshEvent(refreshUnSync: true));
          }
        }
      }
      final lastRefreshUnsyncTime = prefs.getInt("last_refersh_unsync");
      if (lastRefreshUnsyncTime != null) {
        stateModel.updateLastRefreshUnsyncTime(
            DateTime.fromMillisecondsSinceEpoch(lastRefreshUnsyncTime));
      }
      await assetModel.loadTitleCache();
      await loadUnsynchronizedPhotos();
      await initDrive();
      reloadAutoSyncTimer();
    });
  }
}

DateTime? lastAliveTime;
Future<void> checkServer() async {
  if (useRemoteServer) {
    return;
  }
  if (lastAliveTime != null &&
      DateTime.now().difference(lastAliveTime!) < const Duration(seconds: 60)) {
    return;
  }
  try {
    await storage.cli.ping(PingRequest());
    lastAliveTime = DateTime.now();
  } catch (e) {
    logger.addLog("ping 127.0.0.1:$grpcPort failed: $e");
    logger.addLog("reboot server");
    final portsStr = await runServer();
    final ports = portsStr.split(",");
    if (ports.length != 2) {
      logger.addLog("grpc server start failed");
      return;
    }
    httpBaseUrl = "http://127.0.0.1:${ports[1]}";
    grpcPort = int.parse(ports[0]);
    httpPort = int.parse(ports[1]);
    storage = RemoteStorage("127.0.0.1", int.parse(ports[0]));
    await initDrive();
  }
}

Future<void> initDrive() async {
  final prefs = await SharedPreferences.getInstance();
  var drive = prefs.getString("drive");
  drive ??= "SMB";
  switch (getDrive(drive)) {
    case Drive.smb:
      final addr = prefs.getString("addr");
      final username = prefs.getString("username");
      final password = prefs.getString("password");
      final share = prefs.getString("share");
      final root = prefs.getString("rootPath");
      if (addr != null &&
          username != null &&
          password != null &&
          share != null &&
          root != null) {
        final rsp = await storage.cli.setDriveSMB(SetDriveSMBRequest(
          addr: addr,
          username: username,
          password: password,
          share: share,
          root: root,
          timeoutSeconds: prefs.getInt('smb_timeout') ?? 0,
        ));
        if (rsp.success) {
          logger.addLog("set drive smb success");
          settingModel.setRemoteStorageSetted(true);
          eventBus.fire(RemoteRefreshEvent(refreshUnSync: false));
        } else {
          settingModel.setRemoteStorageSetted(false);
          assetModel.remoteLastError = rsp.message;
        }
      }
      break;
    case Drive.webDav:
      final url = prefs.getString('webdav_url');
      final username = prefs.getString('webdav_username');
      final password = prefs.getString('webdav_password');
      final root = prefs.getString('webdav_root_path');
      final insecure = prefs.getBool('webdav_insecure') ?? true;
      if (url != null && root != null) {
        final rsp = await storage.cli.setDriveWebdav(SetDriveWebdavRequest(
          addr: url,
          username: username,
          password: password,
          root: root,
          insecure: insecure,
          timeoutSeconds: prefs.getInt('webdav_timeout') ?? 60,
        ));
        if (rsp.success) {
          logger.addLog("set drive webdav success");
          settingModel.setRemoteStorageSetted(true);
          eventBus.fire(RemoteRefreshEvent(refreshUnSync: false));
        } else {
          settingModel.setRemoteStorageSetted(false);
          assetModel.remoteLastError = rsp.message;
        }
      }
      break;
    case Drive.nfs:
      final addr = prefs.getString('nfs_url');
      final root = prefs.getString('nfs_root_path');
      if (addr != null && root != null) {
        final rsp = await storage.cli.setDriveNFS(SetDriveNFSRequest(
          addr: addr,
          root: root,
          timeoutSeconds: prefs.getInt('nfs_timeout') ?? 60,
        ));
        if (rsp.success) {
          logger.addLog("set drive nfs success");
          settingModel.setRemoteStorageSetted(true);
          eventBus.fire(RemoteRefreshEvent(refreshUnSync: false));
        } else {
          settingModel.setRemoteStorageSetted(false);
          assetModel.remoteLastError = rsp.message;
        }
      }
      break;
    case Drive.s3:
      final endpoint = prefs.getString('s3_endpoint');
      final region = prefs.getString('s3_region');
      final accessKey = prefs.getString('s3_access_key_id');
      final secretKey = prefs.getString('s3_secret_access_key');
      final bucket = prefs.getString('s3_bucket');
      if (endpoint != null &&
          region != null &&
          accessKey != null &&
          secretKey != null &&
          bucket != null) {
        final rsp = await storage.cli.setDriveS3(SetDriveS3Request(
          endpoint: endpoint,
          region: region,
          accessKeyId: accessKey,
          secretAccessKey: secretKey,
          bucket: bucket,
          root: prefs.getString('s3_root') ?? "",
          pathStyle: prefs.getBool('s3_path_style') ?? true,
          follow302: prefs.getBool('s3_follow_302') ?? true,
          force302Https: prefs.getBool('s3_force_302_https') ?? false,
        ));
        if (rsp.success) {
          logger.addLog("set drive s3 success");
          settingModel.setRemoteStorageSetted(true);
          eventBus.fire(RemoteRefreshEvent(refreshUnSync: false));
        } else {
          settingModel.setRemoteStorageSetted(false);
          assetModel.remoteLastError = rsp.message;
        }
      }
      break;
  }
  await applyDirectoryType();
}

/// 把设置里的云端目录结构下发给内置 Go server。
/// server 重启（进程被杀/手机重启）后需要重新下发。
Future<void> applyDirectoryType() async {
  try {
    final rsp = await storageClient.cli.setDirectoryType(
      SetDirectoryTypeRequest(directoryType: settingModel.dirType),
    );
    if (!rsp.success) {
      logger.addLog("set directory type failed: ${rsp.message}");
    }
  } catch (e) {
    logger.addLog("set directory type failed: $e");
  }
}

DateTime? _dateFromPrefs(int? millis) =>
    millis == null ? null : DateTime.fromMillisecondsSinceEpoch(millis);

/// 读取高级功能相关的 prefs 到 settingModel：
/// 并行上传数、云端目录结构、同步过滤、界面语言。
/// UI 进程与 iOS 后台 headless 进程共用。
Future<void> loadAdvancedPrefs() async {
  final prefs = await SharedPreferences.getInstance();
  final parallel = prefs.getInt('parallel_upload_count');
  if (parallel != null) {
    settingModel.setParallelUploadCount(parallel);
  }
  final dirType = prefs.getInt('directory_type');
  if (dirType != null &&
      dirType >= 0 &&
      dirType < DirectoryType.values.length) {
    settingModel.setDirType(DirectoryType.values[dirType]);
  }
  settingModel.setFilterSwitch(prefs.getBool('enable_filter') ?? false);
  settingModel.setFilterNoImage(prefs.getBool('filter_no_image') ?? false);
  settingModel.setFilterNoVideo(prefs.getBool('filter_no_video') ?? false);
  settingModel.setFilterAfter(_dateFromPrefs(prefs.getInt('filter_date_after')));
  settingModel
      .setFilterBefore(_dateFromPrefs(prefs.getInt('filter_date_before')));
  final typeMapJson = prefs.getString('filter_type_map');
  if (typeMapJson != null) {
    try {
      final decoded = jsonDecode(typeMapJson) as Map<String, dynamic>;
      settingModel.setFilterTypeMap(
          decoded.map((key, value) => MapEntry(key, value == true)));
    } catch (e) {
      logger.addLog("load filter_type_map failed: $e");
    }
  }
  final localeCode = prefs.getString('app_locale');
  settingModel.setAppLocale(
      (localeCode == null || localeCode.isEmpty) ? null : Locale(localeCode));
}

class SnackBarManager {
  static final SnackBarManager _instance = SnackBarManager._internal();

  factory SnackBarManager() {
    return _instance;
  }

  SnackBarManager._internal();

  static BuildContext? globalContext;

  static void init(BuildContext context) {
    globalContext = context;
  }

  static void showSnackBar(String message) {
    if (globalContext != null) {
      ScaffoldMessenger.of(globalContext!).showSnackBar(
        SnackBar(
          content: Text(message),
        ),
      );
    }
  }
}

late AppLocalizations l10n;

void initI18n(BuildContext context) {
  l10n = AppLocalizations.of(context)!;
}

Completer<bool>? requesttingPermission;
BuildContext? requestPermissionContext;
void initRequestPermission(BuildContext context) {
  requestPermissionContext = context;
}

Future<bool> requestPermission({alert = true}) async {
  if (isDesktop()) {
    return true;
  }
  bool result = false;
  if (requesttingPermission != null) {
    result = await requesttingPermission!.future;
    return result;
  }
  requesttingPermission = Completer<bool>();
  //权限申请
  final PermissionState ps = await PhotoManager.requestPermissionExtend();
  if (ps == PermissionState.authorized) {
    result = true;
  } else {
    if (alert) {
      result = false;
      if (requestPermissionContext != null) {
        showDialog(
            context: requestPermissionContext!,
            builder: (BuildContext context) => AlertDialog(
                  title: Text(l10n.needPermision),
                  content: Text(l10n.gotoSystemSetting),
                  actions: [
                    TextButton(
                      onPressed: () {
                        Navigator.of(context).pop();
                      },
                      child: Text(l10n.cancel),
                    ),
                    TextButton(
                      onPressed: () {
                        PhotoManager.openSetting();
                        Navigator.of(context).pop();
                      },
                      child: Text(l10n.openSetting),
                    ),
                  ],
                ));
      }
    }
  }
  requesttingPermission?.complete(result);
  requesttingPermission = null;
  return result;
}
