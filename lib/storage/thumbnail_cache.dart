import 'dart:io';
import 'dart:typed_data';

/// best-effort 缩略图磁盘缓存：一切异常吞掉，缓存故障不得影响缩略图加载。
class ThumbnailCache {
  final Directory baseDir;
  ThumbnailCache(this.baseDir);

  /// FNV-1a 64 位（Dart VM 64 位整数回绕），5 千张碰撞概率 ~1e-13
  static int fnv1a64(String s) {
    var h = 0xcbf29ce484222325;
    for (final c in s.codeUnits) {
      h ^= c;
      h = (h * 0x100000001b3) & 0xFFFFFFFFFFFFFFFF;
    }
    return h;
  }

  String _file(String path, int? size) =>
      '${baseDir.path}/${fnv1a64(path)}_${size ?? 0}';

  Future<Uint8List?> read(String path, int? size) async {
    try {
      final f = File(_file(path, size));
      if (await f.exists()) {
        return await f.readAsBytes();
      }
    } catch (_) {}
    return null;
  }

  Future<void> write(String path, int? size, Uint8List bytes) async {
    try {
      await baseDir.create(recursive: true);
      await File(_file(path, size)).writeAsBytes(bytes, flush: true);
    } catch (_) {}
  }
}
