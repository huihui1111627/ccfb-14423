# 部署与设计说明（站点侧）

## 镜像怎么打包

```bash
# 1) 普通的 docker/OCI 层已经是 tar(.gz) 格式；先拿到未压缩的 tar
#    这里用 docker save 导出单个镜像，逐层取 tar。
docker save my/service:v2 -o svc.tar
#    解包后每个 <digest>/layer.tar 就是一个镜像层。
# 2) 打成 CCFLZ 层
go run ./cmd/ccflz pack layer.tar layer.ccflz
#    输出最后一行给出索引摘要，例如：
#    annotation=ccflz.index.digest=sha256:...
# 3) 把层 blob 推到仓库（内容寻址，文件名即 sha256），
#    并把索引摘要和索引长度写进镜像清单的 annotations：
#       ccfz.index.digest = sha256:<索引解压后字节的摘要>
#       ccfz.index.length = <索引 gzip member 的压缩长度>
```

清单注解里的摘要就是启动前的信任锚：客户端打开层时会把索引解压后重新算
`sha256`，与注解不符就拒绝该层，容器不会启动。

## 桩上怎么跑

* `ccflz demo` 只是演示/看板；生产里用 `internal/image` + `internal/layer` 作为库，
  接到现有的容器运行时（或先用它做预取/预热守护进程）。
* 每个 blob 摘要只有一个 `SharedLayer`：同一桩上多个服务共用一层的解压页缓存，
  最后一个容器停掉后缓存释放。
* 冷启动默认只拉：索引（一次范围请求，长度来自 `ccflz.index.length`）+ 入口文件所在块。
  实测 12 张图/36 层/1596 文件/1.9 MiB 内容对照下，冷启动下载量约为整层的 0.9%~1.0%，
  整层拉取为 100%。
* 卫星链路抖动大时，`layer.Options.MaxHole` 可调大（默认 64 KiB）：合并请求时
  多取的"空洞"字节换掉一次往返；`Stats.Holes` 会记录多取的量。
* `layer.Options.MaxParallelFetches`（默认 4）控制单层并发请求数，按链路带宽调。

## 页面

`go run ./cmd/ccflz demo -addr :8099` 后打开 `http://<桩地址>:8099/`：

* **启动阶段**：冷/热启动的就绪耗时、索引耗时、下载字节与占整层比例；
* **镜像层与取块分布**：每层一条按压缩偏移铺开的段图，已取回的块高亮，悬停看
  段类型/偏移/压缩长/解压长/取用次数；
* **文件目录**：按目录树列出文件大小与块取回百分比；
* **范围请求记录**：每次合并请求的时间、层、压缩偏移、长度、实际字节。

页面每 1.5 秒刷新；`启动容器` 走真实懒加载路径，`预热热点块` 按索引里的访问热度
取前 25% 数据段。

## 验证命令

```bash
make test        # 单元 + 集成（含端到端 TestEndToEnd）
make race        # 竞态检查
make verify      # 12 图 × 3 层，报告写 verify-out/verify-report.json
```

更重的跑法（提交里附带的报告就是这么生成的）：

```bash
go run ./cmd/verify -out verification -images 24 -layers 4 -files 150 -reads 600 -latency 2ms
```
