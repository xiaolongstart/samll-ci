# 计划：cqai（uni-app）在 512M 内存限制下完成构建

> 背景：new_slai_app 是 pnpm + turbo monorepo，cqai 为 uni-app（`unh`/vite 构建）Web 客户端。
> 约束：**512M 物理内存为硬需求**；**不允许修改 new_slai_app 项目源码**。

## 一、实测数据（本机采样）

| 项目 | 类型 | 堆 384 | 堆 768 | 堆 1024 | 堆 1536 | 无限制峰值 RSS |
|---|---|---|---|---|---|---|
| new_slai_app/cqai（uni-app，turbo） | vite | OOM | 未测 | OOM | **成功** | ~2.1GB |
| workspace/vcic-admin（普通 vite，**无 turbo**，npm ci） | vite | OOM | OOM | 未测 | **成功** | 未采样（推断同级） |

其他关键实测：
- turbo 缓存持久化：首次 40s 全量构建，二次 **`>>> FULL TURBO`（364ms）**，构建任务内存趋近 0
- 同 commit 跳过（非 turbo 通用缓存）：重复触发 **52ms** 即返，日志写明复用原因
- unh 类 CLI 吞退出码：崩溃后 exit=0 → 必须配置 `output_dir` 靠产物校验兜底（已验证 972 个产物文件正确落盘）

**结论**：重型 Vue/uni-app 项目的 vite 构建堆需求临界值 ≈ **1.5GB**（与是否使用 turbo 无关）。
512M 物理内存直接构建不可行——不依赖 swap 的"调参数"组合都到不了这个需求量级。
因此计划围绕两条线：**降低实际内存压力**（缓存/调度/跳过）+ **swap 兜底**（512M 硬需求下
不改源码的完整构建通路，Node 堆超过物理内存的部分按需分页，Rollup 官方文档确认此用法）。

## 二、阶段 1：配置层落地（samll-ci 已全部实现并实测 ✅）

1. **项目级内存覆盖**：重型项目单独设 `内存上限=1536MB`，全局默认仍 384MB
   （其他轻项目不受影响）。已在 cqai 与 vcic-admin 双项目验证构建成功。
2. **swap 兜底**（512M 硬需求的关键，真机执行）：
   ```bash
   fallocate -l 2G /swapfile && chmod 600 /swapfile
   mkswap /swapfile && swapon /swapfile
   echo '/swapfile none swap sw 0 0' >> /etc/fstab
   ```
   V8 堆超过物理内存的部分由 swap 承接。代价：变更后首次构建显著变慢
   （1 核 + 换页，预估 20-60 分钟），但**能完成**。
3. **turbo 缓存持久化**（使用 turbo 的项目）：构建命令加
   `turbo run build:test --filter=cqai --cache-dir=<data>/cache/turbo`，缓存目录指向
   samll-ci 缓存区（默认 `.turbo` 在工作区内，构建后会被清理，无法跨次复用）。
   代码未变更时 turbo 直接命中缓存，不启动 vite。
4. **同 commit 跳过（不使用 turbo 的项目的通用缓存，✅ 已实现）**：
   项目开启「同 commit 已成功构建则跳过」后，触发构建时 samll-ci 用 `git ls-remote`
   对比远程 HEAD 与最近成功构建的 commit，一致且产物存在则 **52ms 内直接复用产物**、
   不排队不启动构建进程。依赖 `output_dir` 已配置且产物落盘。
5. **CLI 吞退出码防御**：项目配置 `output_dir`（如 `apps/cqai/dist/build/h5`），打包阶段
   强制校验产物存在——即使构建 CLI 崩溃后谎报 exit 0，缺失产物会让构建正确标记 failed。
6. **构建超时**：swap 路径下首次构建调大项目 `timeout`（建议 7200s）。

## 三、阶段 2：构建参数注入（仍不改源码，降低峰值）

samll-ci 自动注入（构建时环境变量，对项目透明；Shell 与 Docker 两种模式均生效）：

| 注入项 | 作用 | 状态 |
|---|---|---|
| `NODE_OPTIONS=--max-old-space-size=<N>` | 堆上限（项目级覆盖优先） | ✅ 已实现 |
| `NODE_OPTIONS += --max-semi-space-size=<M>` | 小新生代降 RSS 峰值（GC 更频繁，时间换内存） | ✅ 已实现（设置页可调） |
| `JAVA_TOOL_OPTIONS=-Xmx<N>m` | JVM 构建内存护栏 | ✅ 已实现 |
| `GOMEMLIMIT=<N>MiB` | Go 类构建工具软上限 | ✅ 已实现 |
| CPU 核数限制 | Docker `--cpus` / Shell Linux `taskset` 绑核 | ✅ 已实现 |
| pnpm store / npm registry / npm ci 缓存 | 安装阶段免重复下载 | ✅ 已实现 |

> 这些手段组合后实测仍不能把堆需求从 1.5G 压到 384M——它们的作用是**缩短 swap 路径的耗时**
> 并**保证非首次构建走缓存零内存**（turbo 项目走 turbo 缓存；非 turbo 项目走同 commit 跳过）。

## 四、阶段 3：真机验收 ✅（Lima 1c/512M Linux VM 等价真机已完成）

验收环境：Lima VM（**1 CPU / 512MiB 物理 / 2G swap+2G swap=4G / 12G 磁盘**，Ubuntu aarch64），
samll-ci 单二进制 + 工具链托管安装（npmmirror 真实下载 node 20.19.4）+ file:// bundle 源码。

| 验收项 | 结果 |
|---|---|
| P1 swap | 4G（RSS 峰值 1.87G，512M 物理承接 ~1.4G 换页；**2G 不够——实测内核 OOM kill**） |
| P2 cqai 完整构建 | ✅ **success，耗时 5m03s**（clone→install→vite build，用户确认时间不设限） |
| P3 产物落盘 | ✅ outputs/972 个文件，打包阶段校验生效 |
| P4 同 commit 跳过 | ✅ 重复触发 **20ms** 复用产物 |
| CLI 吞退出码防御 | ✅ 中途一次 OOM 构建被正确判 failed（未误判） |

关键实测教训：
- **swap 必须 ≥4G**：2G 时在 RSS ~1.6G 处内核 OOM kill（无日志、进程直接被杀）。
  4G 后完成。建议目标机配置：**2G zram/swap + 2G swapfile**。
- file:// 形式的 URL 无法 clone git bundle（git 走传输协议报 too large to be a .git file），
  需用路径形式 `/path/to/x.bundle`；bundle 需带 HEAD 引用（`git bundle create x.bundle HEAD main`）。

一键验收：
```bash
./scripts/verify-512m.sh http://127.0.0.1:8080 <密码> <项目ID>
```
判据：P1 swap ≥4G；P2 重型项目构建 success；P3 产物落盘；P4 同 commit 重复触发秒过。

## 五、遗留决策点（若未来 swap 路径不可接受）

512M 硬需求 + 不改源码的前提下问题已解决（上述配置）。若希望进一步**缩短构建时间**
（当前换页构建 5 分钟 vs 无限制构建 27 秒），可选项按性价比排序：
1. **zram**：2G 压缩内存盘替代部分磁盘 swap（压缩比 ~2.5:1，显著减少 IO），Linux 内核原生支持
2. 项目侧一行改动：`build:test` 关闭 sourcemap（需项目方允许，当前约束排除）
3. 构建节点内存 ≥2G（打破硬需求）

## 五、当前状态：✅ 已完成

- [x] samll-ci 项目级内存覆盖 + semi-space 注入 + CPU/内存护栏（双项目实测通过）
- [x] cqai（uni-app/turbo）1536MB 堆下闭环成功（构建 → 打包 972 文件 → outputs）
- [x] vcic-admin（普通 vite/无 turbo/npm ci）1536MB 堆下闭环成功
- [x] turbo 缓存持久化实测：二次构建 `>>> FULL TURBO`（364ms）
- [x] 同 commit 跳过实测：重复触发 20-52ms 复用产物（非 turbo 项目通用）
- [x] unh/npm 类 CLI 吞退出码问题通过产物校验兜底（VM 内一次 OOM 被正确判 failed）
- [x] 真机验收脚本 scripts/verify-512m.sh
- [x] **1c/512M Linux VM 端到端验收：cqai 构建成功（5m03s，swap 4G）**
