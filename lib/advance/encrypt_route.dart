import 'package:flutter/material.dart';
import 'package:img_syncer/design_tokens.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/state_model.dart';
import 'package:shared_preferences/shared_preferences.dart';

/// 加密储存：开启后上传前加密、下载前解密（加密/解密由内置 Go server 执行）。
/// 密码与加密方式随上传/下载请求的 HTTP 头下发：Image-Encrypt-Type / Image-Encrypt-Password。
class EncryptRoute extends StatefulWidget {
  const EncryptRoute({Key? key}) : super(key: key);

  @override
  State<EncryptRoute> createState() => _EncryptRouteState();
}

class _EncryptRouteState extends State<EncryptRoute> {
  late final TextEditingController _password;
  late bool _enable;
  late EncryptionType _type;
  bool _obscure = true;

  @override
  void initState() {
    super.initState();
    _enable = settingModel.enableEncrypt;
    _type = settingModel.encryptionType == EncryptionType.none
        ? EncryptionType.aesGcm
        : settingModel.encryptionType;
    _password = TextEditingController(text: settingModel.encryptionPassword);
  }

  @override
  void dispose() {
    _password.dispose();
    super.dispose();
  }

  Future<void> _save() async {
    final prefs = await SharedPreferences.getInstance();
    await prefs.setBool('enable_encrypt', _enable);
    await prefs.setInt('encryption_type', _type.index);
    await prefs.setString('encryption_password', _password.text);
    settingModel.setEncryptSwitch(_enable);
    settingModel.setEncryptionType(_type);
    settingModel.setEncryptionPassword(_password.text);
  }

  void _onEnableChanged(bool value) {
    if (value && _password.text.isEmpty) {
      SnackBarManager.showSnackBar(l10n.encryptPasswordEmpty);
      return;
    }
    setState(() {
      _enable = value;
    });
    _save();
  }

  void _onTypeChanged(EncryptionType? type) {
    if (type == null || type == EncryptionType.none) return;
    setState(() {
      _type = type;
    });
    _save();
  }

  @override
  Widget build(BuildContext context) {
    final textTheme = Theme.of(context).textTheme;
    final colorScheme = Theme.of(context).colorScheme;
    return Scaffold(
      appBar: AppBar(title: Text(l10n.encryptedStorage)),
      body: ListView(
        padding: const EdgeInsets.symmetric(vertical: AppSpacing.sm),
        children: [
          SwitchListTile(
            secondary: Icon(Icons.lock_outline, color: colorScheme.primary),
            title: Text(l10n.enableEncrypt, style: textTheme.titleMedium),
            subtitle: Text(l10n.encryptedStorageDesc,
                style: textTheme.bodySmall
                    ?.copyWith(color: colorScheme.onSurfaceVariant)),
            value: _enable,
            onChanged: _onEnableChanged,
          ),
          const Divider(),
          Padding(
            padding: const EdgeInsets.fromLTRB(AppSpacing.md, AppSpacing.sm,
                AppSpacing.md, AppSpacing.xs),
            child: Text(l10n.encryptMethod,
                style: textTheme.titleSmall
                    ?.copyWith(color: colorScheme.onSurfaceVariant)),
          ),
          RadioListTile<EncryptionType>(
            value: EncryptionType.aesGcm,
            groupValue: _type,
            onChanged: _onTypeChanged,
            title: const Text('AES-256-GCM'),
            subtitle: Text(l10n.encryptMethodDesc,
                style: textTheme.bodySmall
                    ?.copyWith(color: colorScheme.onSurfaceVariant)),
          ),
          RadioListTile<EncryptionType>(
            value: EncryptionType.aesCfb,
            groupValue: _type,
            onChanged: _onTypeChanged,
            title: const Text('AES-128-CFB'),
            subtitle: Text(l10n.legacyEncryptMethodDesc,
                style: textTheme.bodySmall
                    ?.copyWith(color: colorScheme.onSurfaceVariant)),
          ),
          const Divider(),
          Padding(
            padding: const EdgeInsets.fromLTRB(AppSpacing.md, AppSpacing.md,
                AppSpacing.md, AppSpacing.xs),
            child: TextField(
              controller: _password,
              obscureText: _obscure,
              decoration: InputDecoration(
                labelText: l10n.encryptPassword,
                errorText: _enable && _password.text.isEmpty
                    ? l10n.encryptPasswordEmpty
                    : null,
                suffixIcon: IconButton(
                  icon: Icon(
                      _obscure ? Icons.visibility_off : Icons.visibility),
                  onPressed: () {
                    setState(() {
                      _obscure = !_obscure;
                    });
                  },
                ),
              ),
              onChanged: (_) {
                setState(() {});
                _save();
              },
            ),
          ),
          Padding(
            padding: const EdgeInsets.fromLTRB(AppSpacing.md, AppSpacing.lg,
                AppSpacing.md, AppSpacing.sm),
            child: Text(l10n.encryptWarning,
                style: textTheme.titleSmall
                    ?.copyWith(color: colorScheme.onSurfaceVariant)),
          ),
          for (final content in [
            l10n.encryptWarningContent01,
            l10n.encryptWarningContent02,
            l10n.encryptWarningContent03,
            l10n.encryptWarningContent04,
          ])
            Padding(
              padding: const EdgeInsets.fromLTRB(
                  AppSpacing.md, 0, AppSpacing.md, AppSpacing.sm),
              child: Text(content, style: textTheme.bodySmall),
            ),
        ],
      ),
    );
  }
}
