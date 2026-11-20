# CCFLZ — 沙漠光伏/气象桩的容器镜像按需懒加载

桩上几百个小服务共用一条按流量计费的卫星链路。每次升级都从镜像仓库整层下载，
一个桩要等半个多小时。CCFLZ 把镜像层打成**可按范围访问**的格式：容器启动时只取
索引和入口文件所在的块，其余字节在真正读到时才按块取回；取回的块在同层的多个
容器之间共用，最后一个容器停止时整层内存释放。

## 1. 层格式

```
[头部段][数据段…][tar 结尾段][索引段]
   ↑ 每个 tar 成员的头（含 PAX 扩展头）单独压成一段
         ↑ 每个文件体按 256 KiB 切成固定块，每块单独压成一段
                              ↑ tar 的两个全零块
                                        ↑ 索引，最后一个 gzip member
```

* 每一段都是**独立的 gzip member**，从该段自己的偏移开始就能单独解压，
  不需要从层首解起。
* 所有段解压后按顺序拼接，与原始 tar 归档**逐字节相同**，所以 `tar -xf`、
  `gzip -dc`、bsdtar、python `tarfile` 都能当普通 `.tar.gz` 整层解开；
  索引段位于 tar 结束标记之后，普通工具会忽略它（`gzip -t` 会提示尾部有额外数据，
  这是 gzip 处理多 member 流的正常行为，解压结果完整）。
* 索引里每个文件记录：
  * `hoff` / `aoff`：成员头、文件体在**解压后归档**里的偏移；
  * `hseg` / `hcoff`：成员头所在段的编号和**压缩流**偏移；
  * `segs` / `coffs` / `coff`：每个数据块的段编号、压缩流偏移。
  因此任意一个块都能只凭索引里的偏移单独取回并解压。
* 索引摘要写进镜像清单注解 `ccflz.index.digest`；启动前校验，被改过的层直接拒绝。
  索引压缩长度写在 `ccflz.index.length`，冷启动一次 tail 请求即可取全索引。

## 2. 元数据

`archive/tar` 读取每个成员时保留：路径、类型（普通/目录/符号链接/硬链接/FIFO/
字符设备/块设备）、mode、uid/gid、用户名/组名、纳秒 mtime、链接目标、设备号、
`SCHILY.xattr.*` / `LIBARCHIVE.xattr.*` 扩展属性（含符号链接自身的属性）。

合并规则照搬按层顺序 `tar -x`：
* `.wh.<name>` 删除同名路径（连同子树），`.wh..wh..opq` 清空目录已有子项；
* 后一层同路径成员覆盖前一层；
* 硬链接与目标共享 inode、内容与元数据（写一个名字，另一个名字看到同样内容）；
* 目录成员自身带 mode/owner/mtime/xattr，物化时最后统一恢复。

`internal/materialize` 把视图写成真实目录树，并逐路径产出规范化元数据清单；
`internal/extract` 是"整层下载后普通解开"的参考实现，两者逐项对照。

## 3. 共享与释放

`image.Registry` 对每个 blob 摘要只打开一个 `layer.SharedLayer`（并发启动用
single-flight 合并）。每启动一个容器 `Acquire()` 一次，停止时 `Release()`；
最后一个引用释放时清空该层的解压页缓存。并发取同一块时按段做 in-flight 合并，
网络请求不会重复。

## 4. 写时复制

容器写入落在可写目录。首次修改懒加载层里的文件时，`view.CopyUp` 先把**整个文件**
取全再落盘，之后的写与读都走副本；硬链接组一起切换。

## 5. 范围请求合并

`layer.plan` 把相邻缺块合成一次请求：
* `MaxHole`（默认 64 KiB）限制合并时顺带拉取的"空洞"压缩字节上限；
* `MaxCoalesce`（默认 8）限制一次请求覆盖的段数；
* 合并后的空洞字节记入 `Stats.Holes`，会在报告里体现；
* 一个层的独立请求组按 `MaxParallelFetches`（默认 4）并发，冷启动不被串行拖慢。

## 6. 运行与验证

```bash
# 端到端验证（默认 3 图 × 3 层，报告写到 verify-out/verify-report.json）
go run ./cmd/verify -out verify-out -images 12 -layers 3 -files 120 -reads 400 \
    -latency 2ms -bandwidth 0

# 单元/集成测试
go test ./...
go test -race ./internal/layer

# 格式工具
go run ./cmd/ccflz pack plain.tar layer.ccflz
go run ./cmd/ccflz index layer.ccflz
tar -xzf layer.ccflz -C out/            # 普通工具可直接整层解开

# 面板（生成演示镜像并在 :8099 起页面）
go run ./cmd/ccflz demo -addr :8099 -images 4 -layers 3 -latency 25ms
```

`cmd/verify` 每个镜像会做：

1. 独立解压每个段并核对摘要，断言所有段拼接后与原 tar 完全一致；
2. `tar -tf` / `tar -xf` / `gzip -dc` 直读打包后的层，内容与归档一致；
3. 三个容器共享同一份层实例，引用计数为 3，最后停止后页缓存清零；
4. 并发随机读文件、列目录几百上千次，每次结果与"整层解开"的目录逐字节对照；
5. 运行中写入既有文件（触发整文件 copy-up）、新建、删除，之后把懒加载视图物化成
   真实目录树，与同样操作过的参考树逐路径比较内容与元数据；
6. 元数据两层对照：
   * **索引 vs 普通 `tar.Reader`**：每个归档成员的类型、mode、uid/gid、纳秒 mtime、
     大小、链接目标、设备号、全部扩展属性，逐项比较（覆盖设备节点这类非 root
     无法落盘的条目）；
   * **懒加载物化 vs 普通整层解包**：逐路径比较内容摘要、mode、mtime、链接、设备号、
     xattr、目录子项，并对硬链接检查同一个 inode；
7. 模拟仓库返回被篡改的块时，摘要校验必须报错；
8. 统计冷启动等待时间、启动阶段下载字节占整层的比例，并给出懒加载 vs 整层拉取的对比。

非 root 环境下设备节点无法创建（内核 `EPERM`，与 `tar` 行为一致），这类路径在
元数据报告中记为权限跳过，其余项照常逐项对照。

## 7. 附带验证报告

仓库里的 `verification/verify-report.json` 由下面这条命令生成：

```bash
go run ./cmd/verify -out verification -images 24 -layers 4 -files 180 -reads 800 -latency 2ms
```

结果（24 个镜像 / 96 层 / 4632 个文件 / 19327 次随机读 / 17404 次列目录 /
1589 次写入，7.1 MiB 内容逐字节对照）：

| 项目 | 结果 |
| --- | --- |
| 内容差异 | 0 |
| 元数据差异（索引 vs tar、物化 vs 解包） | 0 |
| 冷启动下载占整层 | 0.93% ~ 1.00% |
| 整层拉取对照 | 100%（约 3.1 s，冷启动 1.4~1.5 s 含每层 2 ms 请求延迟） |
| 层共享 | 每层 3 个引用，最后一个容器停止后缓存清零 |
| 篡改块检测 | 全部被摘要/校验和拒绝 |

## 8. 目录

```
internal/format      层与索引的数据结构、摘要与注解
internal/pack        打包器：tar → 可寻址段 + 索引
internal/layer       懒加载读取器：取块、合并、缓存、引用计数、摘要校验
internal/repo        内容寻址仓库 + 可注入延迟/带宽/损坏的模拟仓库
internal/image       清单解析、索引注解校验、层注册表
internal/gen         测试镜像生成器（几十个镜像、几千个文件、各类元数据）
internal/view        多层合并视图、白化、硬链接、可写层、copy-up
internal/fsmeta      真实文件系统元数据落盘
internal/materialize 视图 → 真实目录树 + 规范化元数据清单 + 对比
internal/extract     普通整层解包参考实现
internal/container   容器启动/停止、共享层、启动计时
internal/verify      端到端验证与报告
internal/dash        面板服务与页面
cmd/ccflz            打包/索引/解包/演示命令
cmd/verify           验证程序
```
