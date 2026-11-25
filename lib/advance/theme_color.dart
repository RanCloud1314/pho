import 'package:flutter/material.dart';
import 'package:img_syncer/design_tokens.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/theme.dart';
import 'package:shared_preferences/shared_preferences.dart';

/// 主题颜色：选择种子色，M3 配色由种子色生成。
class ThemeColorRoute extends StatefulWidget {
  const ThemeColorRoute({Key? key}) : super(key: key);

  @override
  State<ThemeColorRoute> createState() => _ThemeColorRouteState();
}

class _ThemeColorRouteState extends State<ThemeColorRoute> {
  late Color _selected;

  @override
  void initState() {
    super.initState();
    _selected = seedColor ?? seedThemeColors[0];
  }

  Future<void> _select(Color color) async {
    setState(() {
      _selected = color;
    });
    final prefs = await SharedPreferences.getInstance();
    await prefs.setInt('seed_color', color.value);
    seedColor = color;
    await applySeedColor(context, color);
  }

  @override
  Widget build(BuildContext context) {
    final textTheme = Theme.of(context).textTheme;
    final colorScheme = Theme.of(context).colorScheme;
    return Scaffold(
      appBar: AppBar(title: Text(l10n.themeColor)),
      body: ListView(
        padding: const EdgeInsets.all(AppSpacing.md),
        children: [
          Text(l10n.themeColorDesc,
              style: textTheme.bodySmall
                  ?.copyWith(color: colorScheme.onSurfaceVariant)),
          const SizedBox(height: AppSpacing.md),
          Wrap(
            spacing: AppSpacing.md,
            runSpacing: AppSpacing.md,
            children: [
              for (final color in seedThemeColors)
                InkWell(
                  borderRadius: BorderRadius.circular(AppRadius.large),
                  onTap: () => _select(color),
                  child: Container(
                    width: 48,
                    height: 48,
                    decoration: BoxDecoration(
                      color: color,
                      shape: BoxShape.circle,
                      border: Border.all(
                        color: color.value == _selected.value
                            ? colorScheme.onSurface
                            : Colors.transparent,
                        width: 3,
                      ),
                    ),
                    child: color.value == _selected.value
                        ? Icon(
                            Icons.check,
                            size: 24,
                            // 深色色块用白勾、浅色色块用黑勾
                            color: color.computeLuminance() > 0.5
                                ? const Color(0xFF000000)
                                : const Color(0xFFFFFFFF),
                          )
                        : null,
                  ),
                ),
            ],
          ),
        ],
      ),
    );
  }
}
