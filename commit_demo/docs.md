## nerdctl commit 实现详解

本文梳理 `nerdctl commit` 的完整实现路径与核心逻辑，包括命令注册与参数解析、容器解析与调度、以及底层镜像提交（生成 layer、写入 manifest/config、创建镜像并解包）。文末给出关键类型与格式/压缩处理要点。

### 调用链总览

- CLI 命令与参数定义：`cmd/nerdctl/container/container_commit.go`
  - `CommitCommand` 注册子命令与 flags
  - `commitOptions` 解析并校验参数，生成 `types.ContainerCommitOptions`
  - `commitAction` 建立 containerd 客户端并调用高层 `container.Commit`
- 业务层调度：`pkg/cmd/container/commit.go`
  - 解析镜像引用与 change 指令
  - 通过 `containerwalker` 定位容器
  - 组装 `commit.Opts` 并调用底层 `commit.Commit`
- 底层提交实现：`pkg/imgutil/commit/commit.go`
  - 生成差异层（可选 eStargz / zstd:chunked 转换）
  - 生成新的 image config 与 manifest（支持 docker / oci）
  - 将内容写入 content store，创建/更新镜像并 `Unpack`

---

### 1) 命令注册与参数解析（cmd/nerdctl/container/container_commit.go）

- `CommitCommand`
  - 用法：`commit [flags] CONTAINER REPOSITORY[:TAG]`
  - 主要 flags：
    - `--author, -a`：作者
    - `--message, -m`：提交说明
    - `--change, -c`：应用 Dockerfile 指令（当前支持 `CMD`, `ENTRYPOINT`，JSON 数组格式）
    - `--pause, -p`：提交时是否暂停容器（默认 true）
    - `--compression`：`gzip` 或 `zstd`
    - `--format`：`docker` 或 `oci`
    - `--estargz` 及其压缩/分块参数
    - `--zstdchunked` 及其压缩/分块参数（与 `--estargz` 互斥）

- `commitOptions`
  - 读取全局选项（namespace、address、data-root 等）
  - 校验 `--compression` 与 `--format` 的枚举值
  - 校验 `--estargz` 与 `--zstdchunked` 互斥
  - 返回 `types.ContainerCommitOptions`

- `commitAction`
  - 创建 containerd 客户端与上下文
  - 调用 `container.Commit(ctx, client, REPOSITORY[:TAG], CONTAINER, options)`

---

### 2) 容器解析与调度（pkg/cmd/container/commit.go）

- `Commit(ctx, client, rawRef, req, options)`：
  1. `referenceutil.Parse(rawRef)` 解析目标镜像引用（目标 repository[:tag]）。
  2. `parseChanges(options.Change)` 解析 `--change` 指令（仅支持 `CMD` 与 `ENTRYPOINT`，均为 JSON 数组）。
  3. 组装 `commit.Opts`（作者、消息、目标引用、是否暂停、变更、压缩、镜像格式、estargz/zstdchunked 选项）。
  4. 用 `containerwalker.ContainerWalker` 根据 `req`（容器名/ID 前缀）定位容器：
     - 命中多个前缀时报错；
     - 命中后调用底层 `commit.Commit(ctx, client, found.Container, opts, options.GOptions)`；
     - 将返回的镜像配置 `digest` 写到 `Stdout`。

`parseChanges` 细节：
- 仅识别两类指令：
  - `CMD [json-array]`
  - `ENTRYPOINT [json-array]`
- 重复指定同一指令时，以最后一次为准；未知指令报错。

---

### 3) 底层镜像提交实现（pkg/imgutil/commit/commit.go）

函数：`commit.Commit(ctx, client, container, opts, globalOptions) (digest.Digest, error)`

核心步骤：
1. 读取容器标签并定位状态目录：
   - 从容器标签中取 `labels.StateDir`，若不存在则通过 `containerutil.ContainerStateDirPath` 推导；
   - 对状态目录加文件锁，保证提交过程的并发安全。
2. 识别基础镜像与平台：
   - 读取容器 `Info`，从 `info.Image` 获取基础镜像；
   - 从 `labels.Platform` 获取平台字符串（缺省则用 `platforms.DefaultString()` 并警告），通过 `containerd.NewImageWithPlatform` 包装为定平台镜像；
   - 读取基础镜像的配置（`imgutil.ReadImageConfig`）。
3. 确保基础层齐全：
   - 调用 `image.EnsureAllContent`，缺失层会尝试拉取；失败仅告警（对后续 save/push 可能有影响）。
4. 根据 `--pause` 选择性暂停容器：
   - 通过 `container.Task` 获取任务并查看状态；若正在运行则 `Pause`，并在提交结束后 `Resume`。
5. 创建租约与同步：
   - `client.WithLease(..., WithExpiration(1h))` 防止 GC 提前清理；
   - 调用 `Sync()` 确保容器文件系统写入已落盘。
6. 生成差异层（rootfs diff）：
   - 若启用 `zstd:chunked`，强制压缩算法为 `zstd`；
   - 调用 `createDiff`：
     - 基于 `opts.Format` 与压缩算法选择 layer MediaType：
       - `oci`：`ocispec.MediaTypeImageLayerGzip` 或 `ocispec.MediaTypeImageLayerZstd`
       - `docker`：`images.MediaTypeDockerSchema2LayerGzip` 或 `images.MediaTypeDockerSchema2LayerZstd`
     - 用 `rootfs.CreateDiff` 生成新层 `Descriptor`；从 content 信息的 `containerd.io/uncompressed` 标签读取 `diffID`；
     - 若 `--estargz`：通过 stargz-snapshotter 的转换函数将层转换为 eStargz，并更新 `diffID`；
     - 若 `--zstdchunked`：通过转换函数将层转为 zstd:chunked，并更新 `diffID`。
7. 生成新的镜像配置（Image Config）：
   - `generateCommitImageConfig` 在基础配置上：
     - 应用 `Changes.CMD/Entrypoint`；
     - 设置 `Author`（缺省沿用基础镜像作者）、`Message`、`CreatedBy`（来自容器 `Spec.Process.Args`）；
     - 归一化 `arch/os`（若基础配置缺失则用运行时平台并给出警告）；
     - 将新的 `diffID` 追加到 `RootFS.DiffIDs`；
     - 在 `History` 追加一条记录，并根据 `diffID` 是否为 `emptyGZLayer` 设置 `EmptyLayer`。
8. 将差异层应用到快照器（可选，但本实现会先应用一遍）：
   - `applyDiffLayer`：基于父链 `ChainID(baseConfig.RootFS.DiffIDs)` 准备临时快照，执行 `differ.Apply`，并 `Commit`。
9. 写入 config 与 manifest 到 Content Store：
   - `writeContentsForImage`：
     - 根据 `--format` 选择 Config 与 Manifest 的 MediaType：
       - `oci`：`ocispec.MediaTypeImageConfig` / `ocispec.MediaTypeImageManifest`
       - `docker`：`images.MediaTypeDockerSchema2Config` / `images.MediaTypeDockerSchema2Manifest`
     - 将新 `config` 与 `manifest` 写为 blob：
       - `manifest` 写入时用 `containerd.io/gc.ref.content.N` 标签引用 `config` 与各层，确保 GC 关联；
       - `config` 写入时用 `containerd.io/gc.ref.snapshot.<snapshotter>` 标签引用新 `ChainID`。
10. 创建/更新镜像并解包：
    - 通过 `ImageService.Update/Create` 使镜像名指向新 `manifest`；
    - `containerd.NewImage(...).Unpack(ctx, snapshotterName)` 将镜像解包到指定快照器。
11. 返回值：
    - 返回新 `config` 的 `digest`，上层会打印该值。

注意与限制：
- 若容器不是由 nerdctl 创建（例如 moby 方式），提交会失败（基础镜像缺失等）。
- `--estargz` 与 `--zstdchunked` 互斥。

---

### 关键类型与文件位置

- CLI 命令与参数：`cmd/nerdctl/container/container_commit.go`
  - `CommitCommand`
  - `commitOptions`
  - `commitAction`

- 高层调度：`pkg/cmd/container/commit.go`
  - `Commit`
  - `parseChanges`

- 底层实现：`pkg/imgutil/commit/commit.go`
  - `Commit`
  - `createDiff`
  - `generateCommitImageConfig`
  - `writeContentsForImage`
  - `applyDiffLayer`

- 选项与枚举：`pkg/api/types/container_types.go`
  - `ContainerCommitOptions`（包含 `Author`、`Message`、`Change`、`Pause`、`Compression`、`Format`、`EstargzOptions`、`ZstdChunkedOptions`）
  - `CompressionType`：`gzip` / `zstd`
  - `ImageFormat`：`docker` / `oci`

---

### 媒体类型与压缩映射（要点）

- 当 `--format=oci`：
  - Config：`ocispec.MediaTypeImageConfig`
  - Manifest：`ocispec.MediaTypeImageManifest`
  - Layer：`ocispec.MediaTypeImageLayerGzip` 或 `ocispec.MediaTypeImageLayerZstd`

- 当 `--format=docker`：
  - Config：`images.MediaTypeDockerSchema2Config`
  - Manifest：`images.MediaTypeDockerSchema2Manifest`
  - Layer：`images.MediaTypeDockerSchema2LayerGzip` 或 `images.MediaTypeDockerSchema2LayerZstd`

---

### `--change` 示例

仅支持如下两类（JSON 数组）：

```bash
nerdctl commit \
  -c 'CMD ["/bin/sh","-c","echo hello"]' \
  -c 'ENTRYPOINT ["/usr/bin/my-entrypoint"]' \
  my-container myrepo/myimg:latest
```

---

### 简要流程（顺序）

1. 解析命令与 flags，构造 `ContainerCommitOptions`
2. 解析镜像引用、`--change`，定位容器
3. 可选暂停容器任务
4. 生成差异层（可选转换为 eStargz 或 zstd:chunked）
5. 生成新的 image config 与 manifest
6. 将 blobs 与引用标签写入 content store
7. 更新/创建镜像并解包
8. 输出新镜像配置 `digest`


