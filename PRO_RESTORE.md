# Pho 1.6.1 Pro 功能逆向与开源版还原记录

样本：`C:\Users\22938\Downloads\Pho_1.6.1.apks`（bundletool 拆分包，versionCode/1.6.1）
分析对象：`split_config.arm64_v8a.apk!/lib/arm64-v8a/libapp.so`（Dart AOT snapshot，10.2 MB）

## 1. 逆向方法与证据来源

| 证据 | 说明 |
| --- | --- |
| snapshot 字符串表 | 从 `libapp.so` 提取单字节字符串（ASCII/拉丁）与双字节字符串（UTF-16LE，中文文案），共 30 576 + 142 994 条 |
| 动态调用选择器名 | AOT 保留 `get:` / `set:` / `init:` 选择器名，可直接读出 SettingModel 与各路由的字段/方法名 |
| `package:img_syncer/...` 路径串 | 得到 Pro 版完整模块清单 |
| shared_preferences key（snake_case 串） | 得到 Pro 版持久化字段名，可与开源版直接对齐 |
| l10n 键（`get:xxx` + 中英文文案） | 得到 Pro 版设置页每一行的标题/副标题原文 |
| 开源版 Go server（`server/imgmanager/encrypt.go`、`server/api/http.go`） | 加密由服务端执行，客户端只需下发 HTTP 头 |

Pro 版新增模块（开源版被剥离的部分）：

```
lib/advance/about.dart          lib/advance/advance_route.dart
lib/advance/dir_route.dart      lib/advance/encrypt_route.dart
lib/advance/filter_route.dart   lib/advance/release.dart
lib/advance/theme_color.dart    lib/buy_route.dart
lib/iap.dart                    lib/language_route.dart
lib/storage/profile.dart        lib/storage/profile_manager.dart
lib/storage/thumbnail_cache.dart
lib/storageform/google_drive.dart  lib/storageform/s3form.dart
lib/storageform/root_path_picker.dart
lib/sync/sync_persistence.dart
lib/widgets/gallery_day_group.dart  lib/widgets/gallery_group_header.dart
lib/widgets/gallery_selection_bar.dart  lib/widgets/gallery_tile.dart
```

## 2. Pro 门控逻辑（原版）

原版用 `IAPModel`（`lib/iap.dart`）做 Google Play 内购校验，门控点只有一处：**高级功能入口的可见/可用性**，
功能本身在服务端与 `SettingModel` 里都是现成的。

```
productId: phopro_lifetime / phopro_yearly_sub / phopro_monthly_sub
pref: subscription_type, subscription_expiry, subscription_expiry_source,
      subscription_last_check_date, subscription_last_verified, pro_lastcheck
IAP 报错串: consume_purchase_failed / purchase_error / restore_transactions_failed
关键日志: "IAP] purchasePro: 发起购买 productId=", "IAP] restorePro: 用户手动触发恢复购买",
          "IAP] 订阅有效: type=", "IAP] 订阅已过期: expiry="
l10n: buyPhoPro / unlockAllAdvancedFeatures / manageSubscription / retorePurchase
      monthlyPlan / yearlyPlan / lifetimePlan / subscribe
```

结论：**门控是纯客户端本地校验（prefs 里的订阅到期时间 + Play 回执），功能代码无额外授权**。
开源版不需要购买链路，本次直接移除门控、功能全开。

## 3. Pro 功能 → 开源版还原映射

持久化 key 与 Pro 版**完全一致**，所以覆盖安装后原 Pro 用户的配置能直接生效。

| 功能（设置页文案取自 APK） | Pro 版 key / RPC | 开源版实现 |
| --- | --- | --- |
| 并行上传数「同步照片时并行上传的数量,可以加快同步速度」 | `paralleUploadCount`（仅内存/设置项） | `SettingModel.paralleUploadCount`（1–10）+ `parallel_upload_count`；`runSyncOnce(parallelCount:)` 由 `sync_body` / `sync_timer` / 后台同步三处接入 |
| 加密储存「加密后再上传,避免数据被审查/泄漏」 | `enable_encrypt`、`encryption_type`、`encryption_password`、`enalble_encrypt`(迁移) | `lib/advance/encrypt_route.dart`；加密方式 `AES_256_GCM` / `AES_128_CFB`，密码空值拦截；上传/下载沿用 `Image-Encrypt-Type` / `Image-Encrypt-Password` 头（服务端 AES 分块加密 + `.aes` 后缀） |
| 云端目录结构「选择你上传照片时的云端目录结构」 | `directory_type` + `ImgSyncer.SetDirectoryType` | `lib/advance/dir_route.dart`；`DIRECTORY_TYPE_01`(YYYY/MM/DD) / `_02`(YYYYMMDD)，`applyDirectoryType()` 在 `initDrive()` 后下发（server 重启需重发） |
| 同步过滤「过滤掉不需要同步的内容」 | `enable_filter`、`filter_no_image`、`filter_no_video`、`filter_date_after`、`filter_date_before`、`filter_type_map` | `lib/advance/filter_route.dart` + `shouldSyncAsset()`（`lib/sync/background_runner.dart`）统一在计数/上传/列表三处生效；扩展名 map 为「缺省即同步」语义 |
| 主题颜色「修改为你喜欢的主题颜色」 | `seed_color`（+ `seedThemeColors`） | `lib/advance/theme_color.dart`；`theme.dart:applySeedColor()` 立即重建明暗两套 M3 主题；`main.dart` 用户选定种子色后优先于系统取色 |
| 语言 | `app_locale` | `lib/language_route.dart` + `MaterialApp.locale = SettingModel.appLocale`（null=跟随系统） |
| 释放储存空间「删除已同步的照片」 | `releaseStorage` 系列 l10n | `lib/advance/release.dart`；仅删除 `stateModel.syncedIDs` 命中的本地资产 |

设置页按 1.6.1 的分组重排：基础项 → `advancedSetting`/高级功能（5 项）→ `settingsUtilities`/实用工具（释放空间、清缓存、关于）。
这一版的 `settingsPro`（"Pro 功能"）标题改名为「高级功能」，因为开源版不再有购买入口。

## 4. 未还原（本轮范围外）

- `lib/iap.dart` / `lib/buy_route.dart` 购买与订阅恢复链路（按需求直接放弃门控）。
- Pro 版的 Google Drive / S3 / 百度网盘驱动、`storage/profile*` 多存储 profile、`thumbnail_cache`。
- 相册「仅相机拍摄」过滤（`proOnlyCaremaTitle/Desc`，Pro 版内置算法剔除截屏与下载图）。
- 桌面端（`lib/desktop/settings_route.dart`）未加入高级功能入口；加密/过滤的 prefs 在桌面端同样会被读取。

## 5. 验证

```
flutter gen-l10n                 # 新增 30 个 l10n 键（zh/en）
flutter analyze                  # 0 error
flutter test test/unit test/widget/sync_body_test.dart
  # 还原前：test/unit/sync_logic_test.dart 因缺 Pro API 无法编译；
  #         test/widget/sync_body_test.dart 的 5 个过滤用例全红
  # 还原后：以上全部通过（仅剩仓库原有的 3 个 storage_test + 3 个 theme golden/gate 既有失败，
  #         已在 HEAD 干净 worktree 复现确认与本次改动无关）
flutter build apk --debug --target-platform android-arm64   # 构建成功
```

## 6. 已知取舍

- 过滤判定签名 `shouldSyncAsset(asset, id, uploadedIds, extensionStr)` 保持不变，测试与三个调用方零改动。
- `filter_type_map` 用 JSON 存 `{扩展名: bool}`；`bool` 为 `false` 表示跳过，未出现的扩展名视为同步。
- 加密密码存于 SharedPreferences（与原开源版一致）；Pro 版曾用 `flutter_secure_storage`（`encryptedSharedPreferences`），本项目未引入该依赖。
