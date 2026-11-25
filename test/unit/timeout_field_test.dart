import 'package:flutter_test/flutter_test.dart';
import 'package:img_syncer/storageform/timeout_field.dart';

void main() {
  group('parseTimeoutSeconds', () {
    test('空/非法输入回退默认值', () {
      expect(parseTimeoutSeconds('', 60), 60);
      expect(parseTimeoutSeconds('abc', 60), 60);
      expect(parseTimeoutSeconds(' ', 0), 0);
    });

    test('负数按 0 处理', () {
      expect(parseTimeoutSeconds('-5', 60), 0);
    });

    test('超大数字 clamp 到 int32 上限，不抛异常', () {
      expect(parseTimeoutSeconds('99999999999', 60), 2147483647);
      expect(parseTimeoutSeconds('2147483648', 0), 2147483647);
    });

    test('正常值原样返回', () {
      expect(parseTimeoutSeconds('600', 60), 600);
      expect(parseTimeoutSeconds('0', 60), 0);
    });
  });
}
