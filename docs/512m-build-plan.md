# 计划：cqai（uni-app）在 512M 内存限制下完成构建

> 背景：new_slai_app 是 pnpm + turbo monorepo，cqai 为 uni-app（`unh`/vite 构建）Web 客户端。
> 约束：**512M 物理内存为硬需求**；**不允许修改 new_slai_app 项目源码**。

## 一、实测数据（本机采样）

| 测试 | 结果 |
|---|---|
| 无内存限制直接构建 | 成功，但峰值 RSS **~2.1GB**，耗时 ~27s |
| `--max-old-space-size=384` | **OOM**（`Ineffective mark-compacts near heap limit`） |
| `--max-old-space-size=1024` | **OOM**（且 unh CLI 吞掉崩溃退出码，exit=0，CI 误判成功） |
| `--max-old-space-size=1536` | 成功，`DONE Build complete.` |
| 产物 | `apps/cqai/dist/build/h5/`（972 个文件，已通过 samll-ci 打包阶段验证） |

**结论**：cqai 的 V8 堆需求临界值 ≈ **1.5GB**。512M 物理内存直接构建不可行——任何不依赖 swap 的
"调参数"组合都到不了这个需求量级。因此计划围绕两条线：**降低实际内存压力**（缓存/调度）+
**swap 兜底**（512M 硬需求下唯一不改源码的完整构建通路）。

## 二、阶段 1：配置层落地（samll-ci 已全部实现，立即可用）

1. **项目级内存覆盖**：`new-slai-app` 项目单独设 `内存上限=1536MB`，全局默认仍 384MB
   （其他轻项目不受影响）。已验证：覆盖后 cqai 构建成功。
2. **swap 兜底**（512M 硬需求的关键，真机执行）：
   ```bash
   fallocate -l 2G /swapfile && chmod 600 /swapfile
   mkswap /swapfile && swapon /swapfile
   echo '/swapfile none swap sw 0 0' >> /etc/fstab
   ```
   V8 堆超过物理内存的部分由 swap 承接（Node 会按需分页，Rollup 官方故障排查文档确认此用法）。
   代价：变更后首次构建显著变慢（1 核 + 换页，预估 20-60 分钟），但**能完成**。
3. **turbo 缓存持久化**（未变更构建零内存秒过）：
   ```
   npx -y pnpm@10.33.0 exec turbo run build:test --filter=cqai --cache-dir=<data>/cache/turbo
   ```
   缓存目录指向 samll-ci 缓存区（工作区构建后会被清理，默认 `.turbo` 无法跨次复用）。
   代码未变更时 turbo 直接命中缓存，不启动 vite，内存占用趋近于 0。
4. **unh 吞退出码防御**：项目配置 `output_dir=apps/cqai/dist/build/h5`，打包阶段强制校验产物存在——
   即使 unh 崩溃后谎报 exit 0，缺失产物会让构建正确标记为 failed（已验证）。
5. **构建超时**：项目 `timeout=7200`（swap 路径下的首次构建预留 2 小时）。

## 三、阶段 2：构建参数注入（仍不改源码，降低峰值）

samll-ci 自动注入（构建时环境变量，对项目透明）：

| 注入项 | 作用 | 状态 |
|---|---|---|
| `NODE_OPTIONS=--max-old-space-size=<N>` | 堆上限（项目级覆盖优先） | ✅ 已实现 |
| `NODE_OPTIONS += --max-semi-space-size=16` | 小新生代降 RSS 峰值 20-30%（GC 更频繁，时间换内存） | ✅ 已实现（设置页可调） |
| `GOMEMLIMIT=<N>MiB` | Go 类构建工具软上限 | ✅ 已实现 |
| CPU 核数限制 | Docker `--cpus` / Shell Linux `taskset` 绑核 | ✅ 已实现 |
| pnpm store / npm registry 缓存 | 安装阶段免重复下载 | ✅ 已实现 |

> 这些手段组合后实测仍不能把堆需求从 1.5G 压到 384M——它们的作用是**缩短 swap 路径的耗时**
> 并**保证非首次构建走缓存零内存**。

## 四、阶段 3：判定与升级路径（如实说明）

按上述配置在真机（1c/512M + 2G swap）验收：
- ✅ **通过判据**：首次（冷缓存）构建在 2 小时内 success 且产物落入 outputs/；未变更二次构建秒级 success
- ⚠️ 若 swap 路径不可接受（太慢/不稳定），512M 硬需求与 cqai 构建需求（1.5G+ 堆）物理矛盾，
  可选项只剩（均超出当前约束，列出供决策）：
  1. 项目侧一行改动：`build:test` 关闭 sourcemap（省 30-50% 内存，仍可能不足 512M，需实测）
  2. 构建节点内存 ≥2G（打破 512M 硬需求）
  3. samll-ci 增加远程构建节点（超出单机定位，设计文档明确不做分布式）

## 五、当前状态

- [x] samll-ci 项目级内存覆盖 + semi-space 注入 + CPU/内存护栏（本机实测通过）
- [x] cqai 构建在 1536MB 堆下闭环成功（构建 → 打包 972 文件 → outputs）
- [x] unh 吞退出码问题通过产物校验兜底
- [ ] 真机（1c/512M + swap）端到端验收——需要部署到目标机执行
