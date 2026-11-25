import 'dart:io';

import 'package:extended_image/extended_image.dart';
import 'package:flutter/material.dart';
import 'package:img_syncer/advance/dir_route.dart';
import 'package:img_syncer/advance/encrypt_route.dart';
import 'package:img_syncer/advance/filter_route.dart';
import 'package:img_syncer/advance/release.dart';
import 'package:img_syncer/advance/theme_color.dart';
import 'package:img_syncer/background_sync_route.dart';
import 'package:img_syncer/choose_album_route.dart';
import 'package:img_syncer/design_tokens.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/language_route.dart';
import 'package:img_syncer/setting_storage_route.dart';
import 'package:img_syncer/state_model.dart';
import 'package:photo_manager/photo_manager.dart';
import 'package:shared_preferences/shared_preferences.dart';

/// 设置页：相册/云存储/后台同步 + 高级功能（并行上传、加密储存、
/// 目录结构、同步过滤、主题颜色）+ 实用工具（释放空间、清缓存、关于）。
class SettingsRoute extends StatefulWidget {
  const SettingsRoute({Key? key}) : super(key: key);

  @override
  SettingsRouteState createState() => SettingsRouteState();
}

class SettingsRouteState extends State<SettingsRoute> {
  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.settings),
      ),
      body: ListView(
        children: [
          tile(
            Icons.folder_outlined,
            l10n.chooseAlbum,
            null,
            onTap: () => Navigator.push(
              context,
              MaterialPageRoute(
                builder: (context) => const ChooseAlbumRoute(),
              ),
            ),
          ),
          tile(
            Icons.cloud_outlined,
            l10n.cloudStorage,
            null,
            onTap: () => Navigator.push(
              context,
              MaterialPageRoute(
                builder: (context) => const SettingStorageRoute(),
              ),
            ),
          ),
          if (Platform.isAndroid)
            tile(
              Icons.cloud_sync_outlined,
              l10n.backgroundSync,
              null,
              onTap: () => Navigator.push(
                context,
                MaterialPageRoute(
                  builder: (context) => const BackgroundSyncSettingRoute(),
                ),
              ),
            ),
          tile(
            Icons.language,
            l10n.language,
            null,
            onTap: () => Navigator.push(
              context,
              MaterialPageRoute(builder: (context) => const LanguageRoute()),
            ),
          ),
          sectionHeader(l10n.settingsAdvanced),
          tile(
            Icons.upload,
            l10n.paralleUploadCount,
            l10n.paralleUploadCountDesc,
            trailing: Text(
              '${settingModel.paralleUploadCount}',
              style: Theme.of(context).textTheme.titleMedium,
            ),
            onTap: () => showParallelUploadDialog(),
          ),
          tile(
            Icons.lock_outline,
            l10n.encryptedStorage,
            l10n.encryptedStorageDesc,
            onTap: () => Navigator.push(
              context,
              MaterialPageRoute(builder: (context) => const EncryptRoute()),
            ),
          ),
          tile(
            Icons.photo_library_outlined,
            l10n.dirStructure,
            l10n.dirStructureDesc,
            onTap: () => Navigator.push(
              context,
              MaterialPageRoute(builder: (context) => const DirRoute()),
            ),
          ),
          tile(
            Icons.filter_alt_outlined,
            l10n.syncFilter,
            l10n.syncFilterDesc,
            onTap: () => Navigator.push(
              context,
              MaterialPageRoute(builder: (context) => const FilterRoute()),
            ),
          ),
          tile(
            Icons.palette_outlined,
            l10n.themeColor,
            l10n.themeColorDesc,
            onTap: () => Navigator.push(
              context,
              MaterialPageRoute(builder: (context) => const ThemeColorRoute()),
            ),
          ),
          sectionHeader(l10n.settingsUtilities),
          tile(
            Icons.delete_outline,
            l10n.releaseStorage,
            l10n.deleteSynced,
            onTap: () => Navigator.push(
              context,
              MaterialPageRoute(builder: (context) => const ReleaseRoute()),
            ),
          ),
          tile(
            Icons.cleaning_services,
            l10n.clearCache,
            null,
            onTap: () => showClearCacheDialog(context),
          ),
          tile(
            Icons.info,
            l10n.about,
            null,
            onTap: () => Navigator.push(
              context,
              MaterialPageRoute(
                builder: (context) => const AboutRoute(),
              ),
            ),
          ),
        ],
      ),
    );
  }

  Widget sectionHeader(String title) {
    final colorScheme = Theme.of(context).colorScheme;
    return Container(
      padding: const EdgeInsets.fromLTRB(
          AppSpacing.paddingSmall, AppSpacing.md, AppSpacing.paddingSmall, 0),
      child: Row(
        children: [
          Container(
            width: 6,
            height: 6,
            decoration: BoxDecoration(
              color: colorScheme.primary,
              shape: BoxShape.circle,
            ),
          ),
          const SizedBox(width: AppSpacing.sm),
          Text(
            title,
            style: Theme.of(context)
                .textTheme
                .titleSmall
                ?.copyWith(color: colorScheme.primary),
          ),
        ],
      ),
    );
  }

  Widget tile(IconData icon, String title, String? subtitle,
      {Function()? onTap, Widget? trailing}) {
    final ColorScheme colorScheme = Theme.of(context).colorScheme;
    final TextTheme textTheme = Theme.of(context).textTheme;
    return Container(
      padding: const EdgeInsets.all(AppSpacing.paddingSmall),
      child: ListTile(
        leading: Icon(
          icon,
          size: 28,
          color: colorScheme.onSurfaceVariant,
        ),
        title: Text(title, style: textTheme.titleLarge),
        subtitle: subtitle != null
            ? Text(subtitle,
                style: textTheme.bodySmall?.copyWith(
                  color: colorScheme.onSurfaceVariant,
                ))
            : null,
        trailing: trailing,
        onTap: onTap,
      ),
    );
  }

  Future<void> showParallelUploadDialog() async {
    int value = settingModel.paralleUploadCount;
    final selected = await showDialog<int>(
      context: context,
      builder: (context) => StatefulBuilder(
        builder: (context, setDialogState) => AlertDialog(
          title: Text(l10n.paralleUploadCount),
          content: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(l10n.paralleUploadCountDesc),
              const SizedBox(height: AppSpacing.sm),
              Row(
                children: [
                  Expanded(
                    child: Slider(
                      value: value.toDouble(),
                      min: 1,
                      max: SettingModel.maxParallelUploadCount.toDouble(),
                      divisions: SettingModel.maxParallelUploadCount - 1,
                      label: '$value',
                      onChanged: (v) {
                        setDialogState(() {
                          value = v.round();
                        });
                      },
                    ),
                  ),
                  Text('$value',
                      style: Theme.of(context).textTheme.titleMedium),
                ],
              ),
            ],
          ),
          actions: [
            TextButton(
              onPressed: () => Navigator.of(context).pop(),
              child: Text(l10n.cancel),
            ),
            TextButton(
              onPressed: () => Navigator.of(context).pop(value),
              child: Text(l10n.yes),
            ),
          ],
        ),
      ),
    );
    if (selected == null) return;
    final prefs = await SharedPreferences.getInstance();
    await prefs.setInt('parallel_upload_count', selected);
    settingModel.setParallelUploadCount(selected);
    if (mounted) {
      setState(() {});
    }
  }

  void showClearCacheDialog(BuildContext context) {
    showDialog(
        context: context,
        builder: (context) {
          return Dialog(
            child: SizedBox(
              child: Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  Container(
                    alignment: Alignment.centerLeft,
                    padding: const EdgeInsets.fromLTRB(30, 20, 20, 5),
                    child: Text(
                      l10n.clearCache,
                      style: Theme.of(context).textTheme.titleLarge,
                    ),
                  ),
                  Container(
                    padding: const EdgeInsets.fromLTRB(30, 5, 20, 5),
                    child: Text(
                      l10n.clearCacheDescription,
                    ),
                  ),
                  Row(
                    mainAxisAlignment: MainAxisAlignment.end,
                    children: [
                      Container(
                        padding: const EdgeInsets.fromLTRB(0, 0, 20, 5),
                        child: TextButton(
                          onPressed: () {
                            Navigator.of(context).pop();
                          },
                          child: Text(l10n.cancel),
                        ),
                      ),
                      Container(
                        padding: const EdgeInsets.fromLTRB(0, 0, 20, 5),
                        child: TextButton(
                          onPressed: () {
                            clearDiskCachedImages();
                            PhotoManager.clearFileCache();
                            Navigator.of(context).pop();
                          },
                          child: Text(l10n.yes),
                        ),
                      ),
                    ],
                  ),
                ],
              ),
            ),
          );
        });
  }
}

/// 简化版关于页：显示应用名与版本（来自 pubspec）。
/// 开源版无 package_info_plus 依赖，无隐藏日志入口。
class AboutRoute extends StatelessWidget {
  const AboutRoute({Key? key}) : super(key: key);

  @override
  Widget build(BuildContext context) {
    // 版本号在构建时由 pubspec 注入，运行时读取。
    const version = '1.0.0';
    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.about),
      ),
      body: ListView(
        children: [
          Container(
            padding: const EdgeInsets.symmetric(
                horizontal: AppSpacing.paddingSmall,
                vertical: AppSpacing.xs),
            child: ListTile(
              title: Text(l10n.appVersion,
                  style: Theme.of(context).textTheme.titleLarge),
              subtitle: Text(
                'Pho - $version',
                style: TextStyle(color: Theme.of(context).colorScheme.primary),
              ),
            ),
          ),
        ],
      ),
    );
  }
}
