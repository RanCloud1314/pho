import 'package:flutter/material.dart';
import 'package:img_syncer/design_tokens.dart';
import 'package:img_syncer/global.dart';

/// 高级设置里的超时输入块，三个存储表单共用。
Widget timeoutSettingField(TextEditingController controller) {
  return Container(
    padding: EdgeInsets.symmetric(
        horizontal: AppSpacing.paddingLarge, vertical: AppSpacing.paddingSmall),
    child: TextFormField(
      controller: controller,
      keyboardType: TextInputType.number,
      autovalidateMode: AutovalidateMode.onUserInteraction,
      decoration: InputDecoration(
        border: const OutlineInputBorder(),
        labelText: l10n.timeoutSeconds,
        helperText: l10n.timeoutUnlimitedHint,
      ),
    ),
  );
}

/// 解析超时秒数：空/非法 → fallback（表单默认值），负数按 0。
int parseTimeoutSeconds(String text, int fallback) {
  final v = int.tryParse(text.trim());
  if (v == null) return fallback;
  return v < 0 ? 0 : v;
}

/// 高级设置折叠区。
Widget advancedSettingsTile(Widget child) {
  return ExpansionTile(
    title: Text(l10n.advancedSettings),
    subtitle: null,
    children: [child],
  );
}
