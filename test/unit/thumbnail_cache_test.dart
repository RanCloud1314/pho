import 'dart:io';
import 'dart:typed_data';
import 'package:flutter_test/flutter_test.dart';
import 'package:img_syncer/storage/thumbnail_cache.dart';

void main() {
  late Directory tmp;
  setUp(() {
    tmp = Directory.systemTemp.createTempSync('thumb_cache');
  });
  tearDown(() {
    tmp.deleteSync(recursive: true);
  });

  group('fnv1a64 已知向量', () {
    test('空串 = offset basis', () {
      expect(ThumbnailCache.fnv1a64(''), 0xcbf29ce484222325);
    });
    test('a', () {
      expect(ThumbnailCache.fnv1a64('a'), 0xaf63dc4c8601ec8c);
    });
    test('foobar', () {
      expect(ThumbnailCache.fnv1a64('foobar'), 0x85944171f73967e8);
    });
  });

  group('read/write', () {
    test('写后读命中（目录不存在则自动创建）', () async {
      final c = ThumbnailCache(Directory('${tmp.path}/sub'));
      await c.write('2024/03/05/a.jpg', 1024, Uint8List.fromList([1, 2, 3]));
      expect(await c.read('2024/03/05/a.jpg', 1024), [1, 2, 3]);
    });
    test('size 变化即 miss', () async {
      final c = ThumbnailCache(tmp);
      await c.write('a.jpg', 1, Uint8List.fromList([1]));
      expect(await c.read('a.jpg', 2), isNull);
    });
    test('未写过返回 null', () async {
      expect(await ThumbnailCache(tmp).read('none.jpg', 1), isNull);
    });
    test('覆写完全替换旧内容且无 .tmp 残留', () async {
      final c = ThumbnailCache(tmp);
      await c.write('a.jpg', 1, Uint8List.fromList([1, 2]));
      await c.write('a.jpg', 1, Uint8List.fromList([3, 4, 5]));
      expect(await c.read('a.jpg', 1), [3, 4, 5]);
      expect(tmp.listSync().whereType<File>().map((f) => f.path).where((p) => p.endsWith('.tmp')), isEmpty);
    });
  });
}
