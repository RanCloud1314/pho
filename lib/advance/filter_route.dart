import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:img_syncer/design_tokens.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/state_model.dart';
import 'package:img_syncer/sync/background_runner.dart';
import 'package:shared_preferences/shared_preferences.dart';

/// 同步过滤：按类型、日期区间、扩展名过滤掉不需要同步的内容。
/// 过滤结果由 [shouldSyncAsset] 在计数、上传、列表展示三处统一使用。
class FilterRoute extends StatefulWidget {
  const FilterRoute({Key? key}) : super(key: key);

  @override
  State<FilterRoute> createState() => _FilterRouteState();
}

class _FilterRouteState extends State<FilterRoute> {
  /// 文件类型候选：覆盖相册里常见的图片/视频扩展名
  static const List<String> fileTypes = [
    '.jpg',
    '.jpeg',
    '.png',
    '.gif',
    '.webp',
    '.heic',
    '.dng',
    '.mp4',
    '.mov',
    '.avi',
    '.mkv',
  ];

  Future<void> _save() async {
    final prefs = await SharedPreferences.getInstance();
    await prefs.setBool('enable_filter', settingModel.enableFilter);
    await prefs.setBool('filter_no_image', settingModel.filterNoImage);
    await prefs.setBool('filter_no_video', settingModel.filterNoVideo);
    await _saveDate(prefs, 'filter_date_after', settingModel.filterDateAfter);
    await _saveDate(prefs, 'filter_date_before', settingModel.filterDateBefore);
    await prefs.setString(
        'filter_type_map', jsonEncode(settingModel.filterTypeMap));
  }

  Future<void> _saveDate(
      SharedPreferences prefs, String key, DateTime? value) async {
    if (value == null) {
      await prefs.remove(key);
    } else {
      await prefs.setInt(key, value.millisecondsSinceEpoch);
    }
  }

  Future<void> _pickDate({required bool after}) async {
    final initial = (after
            ? settingModel.filterDateAfter
            : settingModel.filterDateBefore) ??
        DateTime.now();
    final picked = await showDatePicker(
      context: context,
      initialDate: initial,
      firstDate: DateTime(1990),
      lastDate: DateTime(2100),
    );
    if (picked == null) return;
    setState(() {
      if (after) {
        settingModel.setFilterAfter(picked);
      } else {
        settingModel.setFilterBefore(picked);
      }
    });
    await _save();
  }

  Future<void> _clearDate({required bool after}) async {
    setState(() {
      if (after) {
        settingModel.setFilterAfter(null);
      } else {
        settingModel.setFilterBefore(null);
      }
    });
    await _save();
  }

  String _formatDate(DateTime? date) {
    if (date == null) return l10n.tapToSet;
    return '${date.year}-${date.month.toString().padLeft(2, '0')}-'
        '${date.day.toString().padLeft(2, '0')}';
  }

  @override
  Widget build(BuildContext context) {
    final textTheme = Theme.of(context).textTheme;
    final colorScheme = Theme.of(context).colorScheme;
    final enabled = settingModel.enableFilter;
    return Scaffold(
      appBar: AppBar(title: Text(l10n.syncFilter)),
      body: ListView(
        padding: const EdgeInsets.symmetric(vertical: AppSpacing.sm),
        children: [
          SwitchListTile(
            secondary:
                Icon(Icons.filter_alt_outlined, color: colorScheme.primary),
            title: Text(l10n.enableSyncFilter, style: textTheme.titleMedium),
            subtitle: Text(l10n.syncFilterDesc,
                style: textTheme.bodySmall
                    ?.copyWith(color: colorScheme.onSurfaceVariant)),
            value: enabled,
            onChanged: (value) {
              setState(() {
                settingModel.setFilterSwitch(value);
              });
              _save();
            },
          ),
          const Divider(),
          if (enabled) ...[
            SwitchListTile(
              title: Text(l10n.syncFilterNoPhoto,
                  style: textTheme.titleSmall),
              value: settingModel.filterNoImage,
              onChanged: (value) {
                setState(() {
                  settingModel.setFilterNoImage(value);
                });
                _save();
              },
            ),
            SwitchListTile(
              title: Text(l10n.syncFilterNoVideo,
                  style: textTheme.titleSmall),
              value: settingModel.filterNoVideo,
              onChanged: (value) {
                setState(() {
                  settingModel.setFilterNoVideo(value);
                });
                _save();
              },
            ),
            ListTile(
              title: Text(l10n.syncFilterDateAfter,
                  style: textTheme.titleSmall),
              subtitle: Text(_formatDate(settingModel.filterDateAfter)),
              trailing: settingModel.filterDateAfter == null
                  ? null
                  : IconButton(
                      icon: const Icon(Icons.clear),
                      onPressed: () => _clearDate(after: true),
                    ),
              onTap: () => _pickDate(after: true),
            ),
            ListTile(
              title: Text(l10n.syncFilterDateBefore,
                  style: textTheme.titleSmall),
              subtitle: Text(_formatDate(settingModel.filterDateBefore)),
              trailing: settingModel.filterDateBefore == null
                  ? null
                  : IconButton(
                      icon: const Icon(Icons.clear),
                      onPressed: () => _clearDate(after: false),
                    ),
              onTap: () => _pickDate(after: false),
            ),
            Padding(
              padding: const EdgeInsets.fromLTRB(AppSpacing.md, AppSpacing.md,
                  AppSpacing.md, AppSpacing.xs),
              child: Text(l10n.syncFilterFileType,
                  style: textTheme.titleSmall
                      ?.copyWith(color: colorScheme.onSurfaceVariant)),
            ),
            for (final type in fileTypes)
              CheckboxListTile(
                dense: true,
                title: Text(type),
                value: isExtensionSynced(settingModel.filterTypeMap, type),
                onChanged: (value) {
                  setState(() {
                    settingModel.setFilterType(type, value ?? true);
                  });
                  _save();
                },
              ),
          ] else
            Padding(
              padding: const EdgeInsets.fromLTRB(AppSpacing.md, AppSpacing.lg,
                  AppSpacing.md, AppSpacing.lg),
              child: Text(l10n.syncFilterDesc,
                  style: textTheme.bodySmall
                      ?.copyWith(color: colorScheme.onSurfaceVariant)),
            ),
        ],
      ),
    );
  }
}
