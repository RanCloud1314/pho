import 'dart:io';
import 'package:flutter/material.dart';
import 'package:img_syncer/asset.dart';
import 'package:chewie/chewie.dart';
import 'package:video_player/video_player.dart';
import 'package:img_syncer/global.dart';
import 'package:img_syncer/storage/storage.dart';
import 'package:img_syncer/design_tokens.dart';

class VideoRoute extends StatefulWidget {
  const VideoRoute({
    Key? key,
    this.asset,
    this.filePath,
  }) : super(key: key);
  final Asset? asset;
  final String? filePath;

  @override
  _VideoRouteState createState() => _VideoRouteState();
}

class _VideoRouteState extends State<VideoRoute> {
  VideoPlayerController? videoPlayerController;
  ChewieController? chewieController;
  Object? _error;
  bool isInitialized = false;
  String? _tempFilePath;

  @override
  void initState() {
    super.initState();
    initializePlayer().catchError((Object error) {
      if (mounted) setState(() => _error = error);
    });
  }

  @override
  void dispose() {
    chewieController?.dispose();
    videoPlayerController?.dispose();
    if (_tempFilePath != null) {
      final file = File(_tempFilePath!);
      if (widget.filePath == null) {
        file.parent.delete(recursive: true).ignore();
      } else {
        file.delete().ignore();
      }
    }
    super.dispose();
  }

  Future<void> initializePlayer() async {
    bool initialized = false;
    if (widget.filePath != null) {
      videoPlayerController =
          VideoPlayerController.file(File(widget.filePath!));
      _tempFilePath = widget.filePath;
    } else if (widget.asset!.hasLocal) {
      final file = await widget.asset!.local!.originFile;
      if (!mounted) return;
      videoPlayerController = VideoPlayerController.file(file!);
    } else if (widget.asset!.hasRemote) {
      await checkServer();
      if (!mounted) return;
      videoPlayerController = VideoPlayerController.networkUrl(
        widget.asset!.remote!.mediaUri(),
        httpHeaders: remoteMediaHeaders(),
      );
      try {
        await videoPlayerController!.initialize();
        initialized = true;
      } catch (e) {
        if (!mounted) return;
        // 流式播放失败，回退到完整下载后播放
        final filePath = await widget.asset!.downloadToTmpFilePath();
        if (!mounted) {
          await File(filePath).parent.delete(recursive: true);
          return;
        }
        _tempFilePath = filePath;
        await videoPlayerController?.dispose();
        if (!mounted) return;
        videoPlayerController = VideoPlayerController.file(File(filePath));
        await videoPlayerController!.initialize();
        initialized = true;
        SnackBarManager.showSnackBar(l10n.streamFallbackDownload);
      }
    }
    if (!initialized) {
      await videoPlayerController!.initialize();
    }
    if (!mounted) return;
    Widget customControls = const MaterialControls();
    var controlsSafeAreaMinimum = const EdgeInsets.all(0);
    if (Platform.isIOS || Platform.isMacOS) {
      controlsSafeAreaMinimum = const EdgeInsets.fromLTRB(0, 30, 0, 20);
      customControls = const CupertinoControls(
          backgroundColor: AppColors.videoRouteBg, iconColor: Colors.white);
    }
    chewieController = ChewieController(
      videoPlayerController: videoPlayerController!,
      autoPlay: true,
      looping: false,
      showControlsOnInitialize: false,
      showOptions: false,
      customControls: customControls,
      allowFullScreen: false,
      allowMuting: false,
      controlsSafeAreaMinimum: controlsSafeAreaMinimum,
    );
    if (mounted) {
      setState(() {
        isInitialized = true;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
        backgroundColor: Colors.black,
        body: Stack(
          children: [
            Center(
              child: isInitialized
                  ? Chewie(
                      controller: chewieController!,
                    )
                  : _error == null
                      ? const CircularProgressIndicator()
                      : Text(l10n.downloadFailed,
                          style: const TextStyle(color: Colors.white)),
            ),
            Positioned(
              top: 0,
              left: 0,
              right: 0,
              child: AppBar(
                backgroundColor: Colors.transparent,
                iconTheme: const IconThemeData(color: Colors.white),
              ),
            ),
          ],
        ));
  }
}
