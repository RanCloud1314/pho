import 'package:flutter/material.dart';
import 'package:img_syncer/design_tokens.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/state_model.dart';
import 'package:photo_manager/photo_manager.dart';
import 'package:provider/provider.dart';

/// 释放储存空间：删除已经同步到云端的本地照片。
/// 只删除 [StateModel.syncedIDs] 内的资产，不会动未同步的照片。
class ReleaseRoute extends StatelessWidget {
  const ReleaseRoute({Key? key}) : super(key: key);

  List<String> syncedLocalIDs() {
    final synced = stateModel.syncedIDs.toSet();
    return [
      for (final asset in assetModel.localAssets)
        if (asset.local != null && synced.contains(asset.local!.id))
          asset.local!.id
    ];
  }

  Future<void> _delete(BuildContext context, List<String> ids) async {
    if (ids.isEmpty) return;
    final colorScheme = Theme.of(context).colorScheme;
    final confirmed = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: Text(l10n.deleteSynced),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(l10n.cantBeUndone,
                style: TextStyle(color: colorScheme.error)),
            const SizedBox(height: AppSpacing.sm),
            Text(l10n.pleaseConfirmBeforeDelete),
          ],
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: Text(l10n.cancel),
          ),
          TextButton(
            onPressed: () => Navigator.of(context).pop(true),
            child: Text(l10n.yes),
          ),
        ],
      ),
    );
    if (confirmed != true) return;
    try {
      await PhotoManager.editor.deleteWithIds(ids);
      assetModel.removeLocalAssetsByIDs(ids);
      stateModel.removeSyncedPhotos(ids);
      await stateModel.saveSyncedIDs();
      SnackBarManager.showSnackBar(l10n.success);
    } catch (e) {
      SnackBarManager.showSnackBar('${l10n.delete} ${l10n.uploadFailed}: $e');
    }
  }

  @override
  Widget build(BuildContext context) {
    final textTheme = Theme.of(context).textTheme;
    final colorScheme = Theme.of(context).colorScheme;
    return Scaffold(
      appBar: AppBar(title: Text(l10n.releaseStorage)),
      body: Consumer2<StateModel, AssetModel>(
        builder: (context, state, assets, child) {
          final ids = syncedLocalIDs();
          return ListView(
            padding: const EdgeInsets.all(AppSpacing.md),
            children: [
              Icon(Icons.delete_outline,
                  size: 64, color: colorScheme.primary),
              const SizedBox(height: AppSpacing.md),
              Text(
                '${l10n.youHaveSynced} ${ids.length} ${l10n.photosInCloud}',
                textAlign: TextAlign.center,
                style: textTheme.titleMedium,
              ),
              const SizedBox(height: AppSpacing.sm),
              Text(l10n.canDeleteNow,
                  textAlign: TextAlign.center, style: textTheme.bodyMedium),
              const SizedBox(height: AppSpacing.xs),
              Text(l10n.canBrowserAnyTime,
                  textAlign: TextAlign.center,
                  style: textTheme.bodySmall
                      ?.copyWith(color: colorScheme.onSurfaceVariant)),
              const SizedBox(height: AppSpacing.lg),
              FilledButton(
                onPressed: ids.isEmpty ? null : () => _delete(context, ids),
                child: Text(l10n.deleteSynced),
              ),
              const SizedBox(height: AppSpacing.md),
              Text(l10n.pleaseConfirmBeforeDelete,
                  textAlign: TextAlign.center,
                  style: textTheme.bodySmall
                      ?.copyWith(color: colorScheme.error)),
            ],
          );
        },
      ),
    );
  }
}
