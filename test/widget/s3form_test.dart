import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:img_syncer/global.dart' as global;
import 'package:img_syncer/l10n/app_localizations.dart';
import 'package:img_syncer/storageform/s3form.dart';
import 'package:shared_preferences/shared_preferences.dart';

void main() {
  testWidgets('跟随302 默认开启，关闭后隐藏强制https 勾选', (tester) async {
    SharedPreferences.setMockInitialValues({});
    await tester.pumpWidget(MaterialApp(
      localizationsDelegates: AppLocalizations.localizationsDelegates,
      supportedLocales: AppLocalizations.supportedLocales,
      home: Builder(builder: (context) {
        global.initI18n(context);
        return const Scaffold(body: SingleChildScrollView(child: S3Form()));
      }),
    ));
    await tester.pumpAndSettle();

    final checkboxLabel = find.text('强制验证 302 链接 https');
    expect(checkboxLabel, findsOneWidget);
    expect(tester.widget<CheckboxListTile>(find.byType(CheckboxListTile)).value,
        isFalse);

    final followTile = find.ancestor(
        of: find.text('跟随 302'), matching: find.byType(SwitchListTile));
    expect(tester.widget<SwitchListTile>(followTile).value, isTrue);

    await tester.tap(followTile);
    await tester.pumpAndSettle();
    expect(tester.widget<SwitchListTile>(followTile).value, isFalse);
    expect(checkboxLabel, findsNothing);
  });
}
