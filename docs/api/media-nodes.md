# 媒体节点只读巡检

管理端入口为“平台运维 → 媒体节点”，选择节点后按概览、媒体流、网络会话、RTP 服务、代理任务和运行诊断六个分区查看。沿用现有管理端登录鉴权，不直接向浏览器暴露节点 Secret。

本版不提供 ZLM 配置编辑、服务重启、断开连接、关闭媒体流或代理任务增删。节点查询不会触发配置同步或重连。原有节点连接配置 API 保持兼容。

## 路由

- `GET /media_servers`：已有的分页节点列表，包含内存快照中的在线状态与最近心跳。
- `GET /media_servers/:id`：节点档案、连接地址、服务端口和心跳，不含连接 Secret。
- `GET /media_servers/:id/inspect/:resource`：查询指定节点的实时运行数据。

| resource | 对应 ZLM 查询接口 | 数据与用途 |
| --- | --- | --- |
| `version` | `version` | 构建时间、分支与提交 |
| `streams` | `getMediaList` | 所有协议输出、码率、观看数、录像状态、来源与轨道 |
| `sessions` | `getAllSession` | 网络会话的连接标识、两端 IP、端口与类型 |
| `rtp_servers` | `listRtpServer` | 当前打开的 RTP 收流端口、SSRC、传输方式与轨道过滤 |
| `pull_proxies` | `listStreamProxy` | 拉流代理、连接状态、流量与重拉次数 |
| `push_proxies` | `listStreamPusherProxy` | 推流代理、连接状态、流量与重推次数 |
| `ffmpeg` | `listFFmpegSource` | FFmpeg 任务的源地址、目的地址与模板标识 |
| `threads` | `getThreadsLoad` | 网络事件线程负载、调度延迟与可用的附加字段 |
| `work_threads` | `getWorkThreadsLoad` | 后台工作线程负载与调度延迟 |
| `statistics` | `getStatistic` | 节点对象和资源计数 |
| `apis` | `getApiList` | 节点实际提供的 API 路径，仅展示、不执行 |
| `stream_info` | `getMediaInfo` | 指定协议输出的详细信息及可用的轨道丢包率 |
| `stream_players` | `getMediaPlayerList` | 指定协议输出的观看连接 |
| `rtp_info` | `getRtpInfo` | 指定 RTP 流的实际接收状态与设备发流地址 |

`stream_info`、`stream_players` 必须带查询参数 `schema`、`vhost`、`app`、`stream`。`rtp_info` 必须带 `stream_id`，可带 `vhost`、`app`。其他资源读取完整快照；请求不能覆盖节点 Secret，不能指定任意上游接口或执行参数。

示例：

```http
GET /media_servers/zlm-node/inspect/stream_info?schema=rtsp&vhost=__defaultVhost__&app=rtp&stream=channel-id
Authorization: Bearer <平台登录令牌>
```

## 返回状态

```json
{
  "resource": "rtp_info",
  "status": "ok",
  "data": { "exist": false },
  "fetched_at": "2026-11-05T08:00:00+08:00"
}
```

- `ok`：查询成功；列表为空表示确实未返回记录。`rtp_info.exist=false` 表示未检测到接收流。
- `unsupported`：节点驱动、版本或编译选项未提供该接口。界面单独提示，不把它当作空列表。
- `unavailable`：超时、鉴权失败或节点业务错误，通过 `message` 说明。节点鉴权失败不会转换成平台登录失效。

无效资源、缺少必要参数和不存在的节点按平台原有错误响应返回。节点查询超时为 5 秒。上游单次响应最多 16 MiB，超过时显式失败，不静默截断。ZLM 没有分页的查询在平台内按完整快照进行搜索、分类过滤和 20/50/100 条分页；不会因单页展示限制漏掉后续记录。

响应保留节点返回的其他字段供“全部字段”展开查看，但统一隐藏嵌套凭据、命令行、URL 认证信息和 URL 查询参数。节点实际返回字段随版本、编译选项及媒体协议变化，缺失字段显示 `—`。

## 界面数据口径

- 媒体源按 `vhost/app/stream` 去重；协议输出数包含同源的多种协议。
- 源总观看数取同源各协议记录的最大 `totalReaderCount` 后累加，避免跨协议重复统计。缺失计数显示未知，不伪造为零。
- 实时详情中的观看连接属于选定的协议输出，可能少于源总观看数。
- 代理状态 `0` 为在线、`1` 为未连接；拉流代理同时展示节点返回的状态说明。
- 码率由节点的 `bytesSpeed`（字节/秒）换算为 bit/s；轨道 `loss=-1` 显示未知。
- RTP 服务数是当前收流服务数量，不代表端口容量或全部 RTP 发送会话。已开端口不代表已经收到设备媒体流。
- 线程负载不是整机 CPU 使用率；对象统计不是宿主机磁盘或系统资源统计。录像文件查询和回放沿用录像中心。
- 自动刷新默认关闭，可选择每 15 秒刷新当前分区；浏览器隐藏时暂停。单路实时详情单独刷新，所有结果标明采集时间。

人工验收可优先核对实际节点版本的查询支持情况，以及播放前后媒体流、观看连接、RTP 收流状态的变化。
