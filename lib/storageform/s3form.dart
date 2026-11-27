import 'package:flutter/material.dart';
import 'package:img_syncer/event_bus.dart';
import 'package:img_syncer/proto/img_syncer.pbgrpc.dart';
import 'package:img_syncer/state_model.dart';
import 'package:img_syncer/storage/storage.dart';
import 'package:shared_preferences/shared_preferences.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/design_tokens.dart';

class S3Form extends StatefulWidget {
  const S3Form({Key? key}) : super(key: key);

  @override
  S3FormState createState() => S3FormState();
}

class S3FormState extends State<S3Form> {
  @protected
  final GlobalKey _formKey = GlobalKey<FormState>();
  TextEditingController? endpointController;
  TextEditingController? regionController;
  TextEditingController? accessKeyController;
  TextEditingController? secretKeyController;
  TextEditingController? bucketController;
  TextEditingController? rootController;
  bool testSuccess = false;
  String? errormsg;
  bool pathStyle = true;
  bool follow302 = true;
  bool force302Https = false;

  @override
  void initState() {
    super.initState();
    endpointController = TextEditingController();
    regionController = TextEditingController();
    accessKeyController = TextEditingController();
    secretKeyController = TextEditingController();
    bucketController = TextEditingController();
    rootController = TextEditingController();
    SharedPreferences.getInstance().then((prefs) {
      endpointController!.text = prefs.getString('s3_endpoint') ?? "";
      regionController!.text = prefs.getString('s3_region') ?? "us-east-1";
      accessKeyController!.text = prefs.getString('s3_access_key_id') ?? "";
      secretKeyController!.text = prefs.getString('s3_secret_access_key') ?? "";
      bucketController!.text = prefs.getString('s3_bucket') ?? "";
      rootController!.text = prefs.getString('s3_root') ?? "";
      setState(() {
        pathStyle = prefs.getBool('s3_path_style') ?? true;
        follow302 = prefs.getBool('s3_follow_302') ?? true;
        force302Https = prefs.getBool('s3_force_302_https') ?? false;
      });
    });
  }

  String get endpoint {
    final raw = endpointController!.text.trim();
    if (raw.isEmpty || raw.contains("://")) {
      return raw;
    }
    return "https://$raw";
  }

  SetDriveS3Request get request => SetDriveS3Request(
        endpoint: endpoint,
        region: regionController!.text.trim(),
        accessKeyId: accessKeyController!.text.trim(),
        secretAccessKey: secretKeyController!.text,
        bucket: bucketController!.text.trim(),
        root: rootController!.text.trim(),
        pathStyle: pathStyle,
        follow302: follow302,
        force302Https: force302Https,
      );

  Future<void> testStorage() async {
    setState(() {
      testSuccess = false;
    });
    try {
      final rsp = await storage.cli.setDriveS3(request);
      if (!rsp.success) {
        setState(() {
          errormsg = rsp.message;
        });
        return;
      }
      setState(() {
        testSuccess = true;
      });
    } catch (e) {
      setState(() {
        errormsg = e.toString();
      });
      return;
    }
  }

  void showErrorDialog(String msg) {
    showDialog<String>(
      context: context,
      builder: (BuildContext context) => AlertDialog(
        title: Text(l10n.connectFailed),
        content: Text(msg),
        actions: <Widget>[
          TextButton(
            onPressed: () => Navigator.pop(context),
            child: const Text('OK'),
          ),
        ],
      ),
    );
  }

  Widget testStorageButtun() {
    return Container(
      width: 180,
      padding: EdgeInsets.symmetric(
          horizontal: AppSpacing.paddingLarge, vertical: AppSpacing.paddingSmall),
      child: FilledButton.tonal(
        onPressed: () {
          testStorage().then((value) {
            if (testSuccess) {
              SnackBarManager.showSnackBar(l10n.testSuccess);
            } else {
              showErrorDialog(errormsg!);
            }
          });
        },
        child: Text(l10n.testStorage),
      ),
    );
  }

  Widget saveButtun() {
    return Container(
      width: 150,
      padding: EdgeInsets.symmetric(
          horizontal: AppSpacing.paddingLarge, vertical: AppSpacing.paddingSmall),
      child: FilledButton(
        onPressed: testSuccess
            ? () {
                SharedPreferences.getInstance().then((value) {
                  value.setString('s3_endpoint', endpointController!.text.trim());
                  value.setString('s3_region', regionController!.text.trim());
                  value.setString(
                      's3_access_key_id', accessKeyController!.text.trim());
                  value.setString(
                      's3_secret_access_key', secretKeyController!.text);
                  value.setString('s3_bucket', bucketController!.text.trim());
                  value.setString('s3_root', rootController!.text.trim());
                  value.setBool('s3_path_style', pathStyle);
                  value.setBool('s3_follow_302', follow302);
                  value.setBool('s3_force_302_https', force302Https);
                  value.setString('drive', driveName[Drive.s3]!);
                });
                settingModel.setRemoteStorageSetted(true);
                assetModel.remoteLastError = null;
                eventBus.fire(RemoteRefreshEvent(refreshUnSync: true));
                Navigator.pop(context);
              }
            : null,
        child: Text(l10n.save),
      ),
    );
  }

  Widget input(String label, TextEditingController? c, {String? helperText}) {
    return Container(
      padding: EdgeInsets.symmetric(
          horizontal: AppSpacing.paddingLarge, vertical: AppSpacing.paddingSmall),
      child: TextFormField(
        controller: c,
        obscureText: false,
        autovalidateMode: AutovalidateMode.onUserInteraction,
        decoration: InputDecoration(
          border: const OutlineInputBorder(),
          labelText: label,
          helperText: helperText,
        ),
      ),
    );
  }

  Future<void> setBool(String key, bool value) async {
    final prefs = await SharedPreferences.getInstance();
    prefs.setBool(key, value);
  }

  @override
  Widget build(BuildContext context) {
    List<Widget> children = [];
    children.add(input("端点（例如 s3.amazonaws.com）", endpointController,
        helperText: "eg: s3.amazonaws.com 或 minio.local:9000；可带 https:// 前缀"));
    children.add(input("Access Key", accessKeyController));
    children.add(input("Secret Key", secretKeyController));
    children.add(input("区域（例如 us-east-1）", regionController));
    children.add(SwitchListTile(
      title: const Text('Path-Style 寻址'),
      subtitle: const Text('R2 / MinIO / OSS 通常需要开启'),
      value: pathStyle,
      onChanged: (v) {
        setState(() => pathStyle = v);
        setBool('s3_path_style', v);
      },
    ));
    children.add(SwitchListTile(
      title: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Text('跟随 302'),
          IconButton(
            icon: const Icon(Icons.help_outline, size: 18),
            tooltip: '部分服务端（如 openlist）会返回 302 重定向链接，'
                '开启此开关，将跟随 302 重定向',
            onPressed: () => showDialog(
              context: context,
              builder: (context) => AlertDialog(
                content: const Text('部分服务端（如 openlist）会返回 302 重定向链接，'
                    '开启此开关，将跟随 302 重定向'),
                actions: [
                  TextButton(
                    onPressed: () => Navigator.pop(context),
                    child: const Text('知道了'),
                  ),
                ],
              ),
            ),
          ),
        ],
      ),
      value: follow302,
      onChanged: (v) {
        setState(() => follow302 = v);
        setBool('s3_follow_302', v);
      },
    ));
    if (follow302) {
      children.add(CheckboxListTile(
        title: const Text('强制验证 302 链接 https'),
        value: force302Https,
        onChanged: (v) {
          if (v == null) return;
          setState(() => force302Https = v);
          setBool('s3_force_302_https', v);
        },
      ));
    }
    children.add(input("存储桶", bucketController));
    children.add(input("储存根目录（照片会储存在此前缀下，可留空）", rootController));
    children.add(Row(
      mainAxisAlignment: MainAxisAlignment.center,
      children: [
        testStorageButtun(),
        saveButtun(),
      ],
    ));
    return Form(
      key: _formKey,
      child: Column(
        children: children,
      ),
    );
  }
}
