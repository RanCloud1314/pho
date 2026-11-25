import 'package:flutter/material.dart';
import 'package:img_syncer/design_tokens.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/proto/img_syncer.pbgrpc.dart';
import 'package:img_syncer/state_model.dart';
import 'package:provider/provider.dart';
import 'package:shared_preferences/shared_preferences.dart';

/// 云端目录结构：决定之后上传的照片在云端的落盘路径，不影响已上传的文件。
class DirRoute extends StatelessWidget {
  const DirRoute({Key? key}) : super(key: key);

  Future<void> _select(DirectoryType type) async {
    final prefs = await SharedPreferences.getInstance();
    await prefs.setInt('directory_type', type.value);
    settingModel.setDirType(type);
    await applyDirectoryType();
  }

  @override
  Widget build(BuildContext context) {
    final textTheme = Theme.of(context).textTheme;
    final colorScheme = Theme.of(context).colorScheme;
    return Scaffold(
      appBar: AppBar(title: Text(l10n.dirStructure)),
      body: Consumer<SettingModel>(
        builder: (context, model, child) => ListView(
          padding: const EdgeInsets.symmetric(vertical: AppSpacing.sm),
          children: [
            Padding(
              padding: const EdgeInsets.fromLTRB(AppSpacing.md, AppSpacing.sm,
                  AppSpacing.md, AppSpacing.md),
              child: Text(
                l10n.dataDirWarning,
                style: textTheme.bodySmall
                    ?.copyWith(color: colorScheme.onSurfaceVariant),
              ),
            ),
            RadioListTile<DirectoryType>(
              value: DirectoryType.DIRECTORY_TYPE_01,
              groupValue: model.dirType,
              onChanged: (type) => _select(type!),
              title: Text(l10n.dirType01),
              subtitle: const Text('YYYY/MM/DD/photo_name.jpg'),
            ),
            RadioListTile<DirectoryType>(
              value: DirectoryType.DIRECTORY_TYPE_02,
              groupValue: model.dirType,
              onChanged: (type) => _select(type!),
              title: Text(l10n.dirType02),
              subtitle: const Text('YYYYMMDD/photo_name.jpg'),
            ),
          ],
        ),
      ),
    );
  }
}
