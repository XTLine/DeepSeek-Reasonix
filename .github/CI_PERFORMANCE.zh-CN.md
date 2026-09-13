# CI 与发版执行

[English](CI_PERFORMANCE.md)

## Desktop PR 检查

`scripts/ci-paths.mjs` 是普通 CI 与内存 CI 共用的路径判定器，分别识别前端、
Go、生成协议、Electron、原生平台和打包输入。`desktop/AGENTS.md` 等明确的说明
文档不会启动构建和长测；前端目录中会进入产品的 Markdown 仍属于构建输入。
未知路径或无法获得可靠 diff 时保守失败。PR 使用 merge-base 差异，push 使用
`before..sha`；保留纯 release-notes 例外后，普通 `main-v2` push 仍运行完整验证矩阵。

`desktop-prepare` 重新生成桌面宿主契约（有漂移即失败），并在 Linux 上分别生成一次
`electron/stable` 与 `electron/canary` 前端。每份产物都带版本化 manifest，记录
checkout、workflow attempt、变体、构建输入、工具链以及每个 `dist` 文件的摘要。
Linux、macOS 和 Windows 消费者在编译或打包前校验。显式复用遇到 manifest 缺失、
过期、身份不符或文件损坏会直接失败，不会暗中重建。跨平台只共享静态前端文件，
不共享依赖目录、原生模块或 Electron 二进制。构建输入校验通过一个 Git 批处理进程
读取全部已提交 blob，不再为每个文件单独启动进程；版本 1 摘要保持逐字节兼容。

required `lint` 汇总 `lint-code` 和路径要求执行时的完整 `desktop-frontend` 结果。
动画单测保留在统一前端计划中且只执行一次。`desktop-browser-group` 将应用、设置与动画
合为一组，Transcript 独立为另一组，`max-parallel: 2`；`desktop-browser` 汇总拒绝失败、
取消和意外跳过。仅修改 Go 时继续执行协议和原生验证，不启动浏览器或内存长测。

`node desktop/frontend/scripts/run-ci-tests.mjs --list` 可以查看单测清单。
它展开原有专用脚本和生命周期钩子，自动发现新增测试，并保留每个 TypeScript
测试原有的 loader，每个套件只执行一次。未知命令语法或冲突调用会直接失败。
CI 同时运行两个隔离进程，历史性能基准在它们结束后单独运行。
本地仍可使用原有的 `pnpm test:*` 专用命令。

## 耗时报告

普通 CI 与内存工作流的 Summary 会分别显示不含排队的阶段执行时间、工作流总等待、
已记录的 job 排队时间之和及 runner 执行时长之和。前端构建、依赖与浏览器安装、
各浏览器分组和每个内存 shard 单独列出。单次数据只描述该次运行；对比应针对同一
候选各重复三次，并报告中位数和范围，避免把 runner 波动当作收益。Windows Desktop
Go 步骤保留原生非 verbose 输出，因为 Go JSON 模式会让 Windows 花费数分钟收尾
verbose 测试缓存；统一耗时报告直接从 Actions API 记录该步骤的执行时间，不再包装
测试进程。

## 内存筛查

协议 v2 仍要求三个独立进程，每个完成 128 轮 full、128 轮 windowed、
128 轮 safety 和 512 轮 mixed 往返，保持相同检查点、五份堆快照、GC、
稳定帧等待与判定阈值。只有明确启用 mock 内存 soak 的 URL 会去掉夹具人为
设置的 1.5 秒 hydration 延迟；加载仍经过异步定时器任务。
普通浏览器和原生几何测试继续使用原来的延迟路径。

采样时鼠标停在会话行之外；布局切换后先完成一次会话往返，再记录预热基线，
避免采到菜单的临时状态。报告声明协议版本，汇总拒绝旧协议。
`timings.json` 在测试宿主侧记录导航、稳定帧等待、GC、堆快照与解析的次数、
总耗时和最大耗时。短程 profiling 仅用于诊断，不能通过完整筛查门禁。
筛查通过仍不等于完成离线堆引用归因。

## 签名发版产物

Stable 的 SignPath 预检成功后，将完整原生平台矩阵的产物交给 desktop
publisher。后者重新验证授权、候选身份、签名契约及缓存和文档校验，再校验
并发布同一批签名字节，不重复构建和签名。CLI 与 npm 仍等待完整预检通过，
不会提前开始公开发布。

每个平台的 bundle 将文件大小、SHA-256 与候选及控制流程 SHA、版本、tag、
channel、签名契约指纹、run、调用和生产 attempt 绑定。
平台缺失或多余、身份冲突、符号链接、重复文件名及字节变化都会阻止发布。
这些传输哈希补充既有 Authenticode/minisign 校验，不能代替签名验证。

只重跑失败 job 时，可复用同一 run 和调用中此前成功的平台；重建的平台只
替换自身已重新验证的 bundle。新的 workflow run 重新准备整套产物。
独立恢复发版仍自行构建并验证完整矩阵。pnpm 下载依赖按锁文件缓存；签名
产物通过 artifact 交接，不放入依赖缓存。
