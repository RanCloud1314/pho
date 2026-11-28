import 'dart:async';
import 'package:flutter_test/flutter_test.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/proto/img_syncer.pbgrpc.dart';
import 'package:img_syncer/state_model.dart';
import 'package:img_syncer/storage/storage.dart';
import 'package:shared_preferences/shared_preferences.dart';

/// 创建一个模拟响应流，用于测试 receiveResponses。
Stream<T> _mockResponseStream<T>(List<T> items) async* {
  for (final item in items) {
    yield item;
  }
}

class _PagedStorage extends RemoteStorage {
  _PagedStorage() : super('127.0.0.1', 10000);

  final requests = <List<Object>>[];

  @override
  Future<List<RemoteImage>> listImages(
      String date, int offset, dynamic maxReturn,
      {String afterPath = ''}) async {
    requests.add([date, offset, maxReturn as int, afterPath]);
    return List.generate(
      offset == 0 ? maxReturn : 2,
      (i) => RemoteImage(cli, '20261127/${offset + i}.jpg',
          httpClient: httpClient),
    );
  }
}

void main() {
  test('云端相册先显示首批，下一页从最后一天继续', () async {
    SharedPreferences.setMockInitialValues({});
    final originalStorage = storageClient;
    final originalUseRemoteServer = useRemoteServer;
    final remote = _PagedStorage();
    final model = AssetModel();
    addTearDown(() {
      setStorageForTest(originalStorage);
      useRemoteServer = originalUseRemoteServer;
      remote.httpClient.close();
      model.dispose();
    });
    setStorageForTest(remote);
    useRemoteServer = true;
    final visibleCounts = <int>[];
    model.addListener(() => visibleCounts.add(model.remoteAssets.length));

    expect(await model.getRemotePhotos(), isTrue);
    expect(remote.requests, [
      ['', 0, 48, '']
    ]);
    expect(visibleCounts, contains(48));
    expect(await model.getRemotePhotos(), isTrue);
    expect(remote.requests.last,
        ['2026:11:27', 48, model.pageSize, '20261127/47.jpg']);
    expect(model.remoteAssets.length, 50);
  });

  group('receiveResponses 两阶段提交', () {
    late StateModel state;

    setUp(() async {
      SharedPreferences.setMockInitialValues({});
      state = StateModel();
    });

    test('accumulatedIDs 模式仅积累不写入 syncedIDs', () async {
      // 预置一些旧的 syncedIDs
      state.setSyncedPhotos(['old_id_1', 'old_id_2']);
      final preAccumulate = List<String>.from(state.syncedIDs);

      final accumulatedIDs = <String>[];
      final stream = _mockResponseStream([
        FilterNotUploadedResponse(
          success: true,
          uploadedIDs: ['new_id_1', 'new_id_2'],
        ),
        FilterNotUploadedResponse(
          success: true,
          uploadedIDs: ['new_id_3'],
        ),
      ]);

      await receiveResponses(stream, accumulatedIDs: accumulatedIDs);

      // syncedIDs 应保持旧值未被修改
      expect(state.syncedIDs, preAccumulate);
      // accumulatedIDs 应包含所有新 ID
      expect(accumulatedIDs, containsAll(['new_id_1', 'new_id_2', 'new_id_3']));
      expect(accumulatedIDs.length, 3);
    });

    test('accumulatedIDs 模式重复 ID 正常追加（去重在 setSyncedPhotos 调用方执行）', () async {
      state.setSyncedPhotos(['old_1']);
      final accumulatedIDs = <String>[];
      final stream = _mockResponseStream([
        FilterNotUploadedResponse(
          success: true,
          uploadedIDs: ['dup_id', 'dup_id', 'unique_id'],
        ),
      ]);

      await receiveResponses(stream, accumulatedIDs: accumulatedIDs);

      // 积累阶段不去重，由调用方处理
      expect(accumulatedIDs.length, 3);
    });

    test('accumulatedIDs 模式遇到错误时停止，保留已同步状态', () async {
      state.setSyncedPhotos(['old']);
      final accumulatedIDs = <String>[];
      final stream = _mockResponseStream([
        FilterNotUploadedResponse(
          success: false,
          message: 'simulated error',
        ),
        FilterNotUploadedResponse(
          success: true,
          uploadedIDs: ['ok_id'],
        ),
      ]);

      await expectLater(
          receiveResponses(stream, accumulatedIDs: accumulatedIDs),
          throwsStateError);

      expect(state.syncedIDs, ['old']);
      expect(accumulatedIDs, isEmpty);
    });
  });

  group('receiveResponses 去重模式', () {
    setUp(() async {
      SharedPreferences.setMockInitialValues({});
      // 直接操作顶层单例 stateModel（receiveResponses 内部引用的是它）
      stateModel.setSyncedPhotos([]);
    });

    test('无 accumulatedIDs 时直接写入并自动去重', () async {
      stateModel.setSyncedPhotos(['existing_1']);
      final stream = _mockResponseStream([
        FilterNotUploadedResponse(
          success: true,
          uploadedIDs: ['existing_1', 'existing_1', 'new_2'],
        ),
      ]);

      await receiveResponses(stream);

      // 应通过 Set 去重
      final ids = stateModel.syncedIDs;
      expect(ids.toSet().length, ids.length);
      expect(ids, containsAll(['existing_1', 'new_2']));
    });

    test('多次响应累计写入并去重', () async {
      stateModel.setSyncedPhotos([]);
      final stream = _mockResponseStream([
        FilterNotUploadedResponse(
          success: true,
          uploadedIDs: ['id_a', 'id_b'],
        ),
        FilterNotUploadedResponse(
          success: true,
          uploadedIDs: ['id_b', 'id_c'],
        ),
      ]);

      await receiveResponses(stream);

      final ids = stateModel.syncedIDs;
      expect(ids.toSet().length, 3);
      expect(ids, containsAll(['id_a', 'id_b', 'id_c']));
    });
  });

  group('syncedIDs 去重逻辑', () {
    late StateModel state;

    setUp(() async {
      SharedPreferences.setMockInitialValues({});
      state = StateModel();
    });

    test('setSyncedPhotos 后通过 toSet 去重', () {
      final idsWithDup = ['a', 'b', 'a', 'c', 'b'];
      state.setSyncedPhotos(idsWithDup.toSet().toList());
      expect(state.syncedIDs.length, 3);
      expect(state.syncedIDs.toSet().length, 3);
    });
  });

  group('上传进度', () {
    late StateModel state;

    setUp(() {
      SharedPreferences.setMockInitialValues({});
      state = StateModel();
    });

    test('进度封顶 99%，finishUpload 后归零', () {
      state.updateUploadProgress('id1', 100, 100);
      expect(state.getUploadPercent('id1'), closeTo(0.99, 0.0001));
      state.finishUpload('id1', true);
      expect(state.getUploadPercent('id1'), 0);
      expect(state.uploadProgress.containsKey('id1'), isFalse);
    });

    test('更新进度时计算速率', () async {
      state.updateUploadProgress('id1', 0, 1000);
      // 等待一点时间让两次 update 之间有时间差
      await Future<void>.delayed(const Duration(milliseconds: 50));
      state.updateUploadProgress('id1', 500, 1000);
      expect(state.uploadProgress['id1']!.rateBps, greaterThan(0));
    });

    test('同一时刻连续两次更新不产生 NaN 速率', () {
      state.updateUploadProgress('id1', 100, 1000);
      state.updateUploadProgress('id1', 200, 1000);
      expect(state.uploadProgress['id1']!.rateBps, isNotNaN);
    });
  });
}
