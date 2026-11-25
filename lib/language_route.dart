import 'package:flutter/material.dart';
import 'package:img_syncer/design_tokens.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/state_model.dart';
import 'package:provider/provider.dart';
import 'package:shared_preferences/shared_preferences.dart';

/// 界面语言：null 表示跟随系统，其余按语言码切换（pref: app_locale）。
class LanguageRoute extends StatelessWidget {
  const LanguageRoute({Key? key}) : super(key: key);

  static const List<Locale> languages = [Locale('en'), Locale('zh')];

  Future<void> _select(Locale? locale) async {
    final prefs = await SharedPreferences.getInstance();
    if (locale == null) {
      await prefs.remove('app_locale');
    } else {
      await prefs.setString('app_locale', locale.languageCode);
    }
    settingModel.setAppLocale(locale);
  }

  String _name(Locale locale) {
    switch (locale.languageCode) {
      case 'zh':
        return '简体中文';
      case 'en':
        return 'English';
      default:
        return locale.languageCode;
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: Text(l10n.language)),
      body: Consumer<SettingModel>(
        builder: (context, model, child) => ListView(
          padding: const EdgeInsets.symmetric(vertical: AppSpacing.sm),
          children: [
            RadioListTile<Locale?>(
              value: null,
              groupValue: model.appLocale,
              onChanged: (_) => _select(null),
              title: Text(l10n.followSystem),
            ),
            for (final locale in languages)
              RadioListTile<Locale?>(
                value: locale,
                groupValue: model.appLocale,
                onChanged: (_) => _select(locale),
                title: Text(_name(locale)),
              ),
          ],
        ),
      ),
    );
  }
}
