"""AI 生命周期回归测试；模拟模型和 gRPC 边界，不启动摄像头或网络服务。"""
import importlib.util
import queue
import sys
import types
from concurrent.futures import ThreadPoolExecutor
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from frame_capture import FrameCapture, LogPipe


def load_runtime():
    response = type('HealthCheckResponse', (), {
        'SERVING': 1, 'NOT_SERVING': 2,
        '__init__': lambda self, status: setattr(self, 'status', status),
    })
    deps = {
        'grpc': types.SimpleNamespace(StatusCode=types.SimpleNamespace(UNAVAILABLE='unavailable', INVALID_ARGUMENT='invalid', RESOURCE_EXHAUSTED='capacity')),
        'cv2': types.SimpleNamespace(),
        'detect': types.SimpleNamespace(MotionDetector=lambda: types.SimpleNamespace(backgrounds={}), ObjectDetector=Mock),
        'analysis_pb2': types.SimpleNamespace(
            HealthCheckResponse=response,
            StartCameraResponse=types.SimpleNamespace, StopCameraResponse=types.SimpleNamespace,
            CameraStatus=types.SimpleNamespace,
            StatusResponse=lambda: types.SimpleNamespace(cameras=[], stats=types.SimpleNamespace(active_streams=0)),
        ),
        'analysis_pb2_grpc': types.SimpleNamespace(HealthServicer=object, AnalysisServiceServicer=object),
    }
    spec = importlib.util.spec_from_file_location('owl_analysis_runtime', Path(__file__).with_name('main.py'))
    module = importlib.util.module_from_spec(spec)
    with patch.dict(sys.modules, deps):
        spec.loader.exec_module(module)
    return module


class RuntimeTests(unittest.TestCase):
    def test_health_reports_model_readiness(self):
        runtime = load_runtime()
        service = Mock()
        health = runtime.HealthServicer(service)
        for ready, expected in [(False, 2), (True, 1)]:
            service.is_ready.return_value = ready
            self.assertEqual(health.Check(None, None).status, expected)
        self.assertEqual(service.is_ready.call_count, 2)

    def test_failed_capture_and_failed_spawn_exhaust_retry_budget(self):
        for spawn_error in [False, True]:
            with self.subTest(spawn_error=spawn_error):
                capture = FrameCapture('rtsp://example.test/live', queue.Queue(), retry_limit=3)
                capture.width = capture.height = 1
                attempts = []

                def spawn(*args, **kwargs):
                    attempts.append(1)
                    # 旧实现不会耗尽预算；设置保险，确保回归失败也不会死循环。
                    if len(attempts) > 3:
                        capture._stop_event.set()
                    if spawn_error:
                        raise OSError('ffmpeg missing')
                    process = Mock()
                    process.poll.return_value = 1
                    return process

                with patch('frame_capture.subprocess.Popen', side_effect=spawn), \
                     patch('frame_capture.LogPipe') as pipe, \
                     patch.object(capture._stop_event, 'wait', return_value=False):
                    capture._capture_loop()
                    self.assertTrue(pipe.return_value.close.called)
                self.assertEqual(len(attempts), 3)
                self.assertEqual(capture.error_count, 3)
                self.assertTrue(capture.is_failed)
                self.assertTrue(capture.last_error)

    def test_log_pipe_closes_both_ends(self):
        pipe = LogPipe('test')
        pipe.close()
        self.assertFalse(pipe.is_alive())
        self.assertTrue(pipe.pipe_reader.closed)
        pipe.close()


class TaskRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.runtime = load_runtime()
        self.service = self.runtime.AnalysisServiceServicer("fake-model")
        self.service._is_ready = True
        self.context = Mock()

    def request(self, camera_id="camera"):
        return types.SimpleNamespace(
            camera_id=camera_id, rtsp_url="rtsp://example.test/live",
            callback_url="http://example.test/ai", callback_secret="", zones=[],
            labels=[], threshold=.5, detect_interval_seconds=5,
            alert_cooldown_seconds=30, retry_limit=3,
        )

    def fake_task(self, status="running", stopped=True):
        task = Mock(status=status, frames_processed=0, retry_count=0, last_error="")
        task.current_status.return_value = status
        task.stop.return_value = stopped
        task.capture.get_stream_info.return_value = (640, 480, 25)
        return task

    def test_failed_task_is_replaced_only_after_confirmed_stop(self):
        for stopped in [False, True]:
            with self.subTest(stopped=stopped):
                old = self.fake_task("error", stopped)
                self.service._camera_tasks = {"camera": old}
                fresh = self.fake_task()
                with patch.object(self.runtime, 'CameraTask', return_value=fresh) as factory:
                    response = self.service.StartCamera(self.request(), self.context)
                self.assertEqual(response.success, stopped)
                self.assertEqual(factory.call_count, int(stopped))
                self.assertIs(self.service._camera_tasks['camera'], fresh if stopped else old)
                old.stop.assert_called_once()

    def test_stop_timeout_retains_capacity_and_stop_is_idempotent(self):
        old = self.fake_task("stopping", False)
        self.service._max_tasks = 1
        self.service._camera_tasks['camera'] = old
        self.assertFalse(self.service.StopCamera(self.request(), self.context).success)
        with patch.object(self.runtime, 'CameraTask') as factory:
            self.assertFalse(self.service.StartCamera(self.request('other'), self.context).success)
            factory.assert_not_called()
        self.assertIs(self.service._camera_tasks['camera'], old)
        old.stop.return_value = True
        self.assertTrue(self.service.StopCamera(self.request(), self.context).success)
        self.assertTrue(self.service.StopCamera(self.request(), self.context).success)
        self.assertEqual(self.service._camera_tasks, {})

    def test_default_capacity_is_atomic_under_concurrent_starts(self):
        with patch.object(self.runtime, 'CameraTask', side_effect=lambda *a, **kw: self.fake_task()):
            with ThreadPoolExecutor(max_workers=12) as pool:
                results = list(pool.map(lambda i: self.service.StartCamera(self.request(str(i)), Mock()).success, range(40)))
        self.assertEqual(sum(results), 32)
        self.assertEqual(len(self.service._camera_tasks), 32)
        with patch.object(self.runtime, 'CameraTask') as factory:
            existing = next(iter(self.service._camera_tasks))
            self.assertTrue(self.service.StartCamera(self.request(existing), self.context).success)
            factory.assert_not_called()

    def test_status_counts_only_active_tasks(self):
        self.service._camera_tasks = {s: self.fake_task(s) for s in ['running', 'initializing', 'error', 'stopping', 'stopped']}
        response = self.service.GetStatus(None, None)
        self.assertEqual(response.stats.active_streams, 2)
        self.assertEqual(len(response.cameras), 5)

    def test_camera_stop_waits_for_capture_and_analysis(self):
        task = self.runtime.CameraTask('camera', 'rtsp://example.test/live', {}, Mock(), self.service.motion_detector)
        task.capture = Mock(is_failed=False)
        task._thread = Mock()
        self.service.motion_detector.backgrounds['camera'] = object()
        for capture_done, thread_alive in [(False, False), (True, True), (True, False)]:
            task.capture.stop.return_value = capture_done
            task._thread.is_alive.return_value = thread_alive
            self.assertEqual(task.stop(), capture_done and not thread_alive)
        self.assertEqual(task.status, 'stopped')
        self.assertNotIn('camera', self.service.motion_detector.backgrounds)

    def test_repeated_inference_errors_reach_terminal_state(self):
        motion = Mock()
        motion.detect.return_value = ([], True)
        task = self.runtime.CameraTask('camera', 'rtsp://example.test/live', {'retry_limit': 3}, Mock(), motion)
        task.status = 'running'
        task.capture = Mock(is_failed=False)
        task.frame_queue = queue.Queue()
        for i in range(3):
            task.frame_queue.put(i)
        task._detect_with_zones = Mock(side_effect=RuntimeError('inference failed'))
        task._send_stopped_callback = Mock()
        # 若回归导致未耗尽预算，第4次取帧时退出，避免测试挂起。
        real_get = task.frame_queue.get
        def get_frame(**kwargs):
            if task.frame_queue.empty():
                task._stop_event.set()
                raise queue.Empty()
            return real_get(**kwargs)
        with patch.object(task.frame_queue, 'get', side_effect=get_frame), patch.object(task._stop_event, 'wait'):
            task._analysis_loop()
        self.assertEqual(task.status, 'error')
        self.assertEqual(task.retry_count, 3)
        task.capture.stop.assert_called_once()
        task._send_stopped_callback.assert_called_once()


if __name__ == '__main__':
    unittest.main()
