# 对话滚动契约

[English](TRANSCRIPT_SCROLL_CONTRACT.md)

修改对话视口、虚拟化、测量或滚动行为时使用本契约。
`desktop/frontend/src/components/Transcript.tsx` 由
`TimelineProjection` 和带 generation 隔离的 `TranscriptKernel` 管理。

## 身份、调度和写入

- 一个完整 turn 是投影、锚点和虚拟化单元。Block key 来自后端 entry/user
  身份，不能使用数组下标；前插和内容更新不能改变已挂载块的身份。
- 几何或范围更新期间可点击的控件保留同一个 DOM host，以状态控制可见性，
  不跨视口提交重新挂载。
- 切换 session/surface 时递增 generation。延迟测量、timer、动画帧和写请求
  都携带 generation，过期工作零写入。异步分页绑定源 session 请求身份；
  问题跳转从请求到定位终态绑定 generation 和交互 revision。
  原生输入接管会取消跳转，但不会取消有效的源数据加载；
  旧 completion/finally 只能释放同一个请求。
- 只有 `desktop/frontend/src/lib/transcriptViewportWriter.ts` 能改变原生滚动
  位置。完整 DOM、TanStack、Markdown、选择、问题导航、前插、输入框 resize
  和尾部跟随都向 Kernel 提交事务。`check-single-scroll-writer.mjs` 必须拒绝旁路。
- Writer 写入偏移后，保留来源标记，直到匹配的原生 scroll 事件被消费，
  或不同偏移证明是用户输入。No-op 和续租不能消费标记；无输入 owner 的布局
  滚动、结构几何事务保留逻辑意图。触摸惯性和滚动条释放保留 quiet-period
  lease，直到最后一次原生进展。新手势不能把延迟 writer 事件改称原生输入；
  顶部分页只响应原生拥有的向上运动。
- 每个事务必须 committed、cancelled 或 expired。用户输入和选择优先；
  问题跳转高于 display/prepend/restore/resize，后者高于 tail follow。
- 全部布局、ResizeObserver、paint、auto-fill 共用 Kernel 可取消帧。
  Observer 注册时绑定 generation，断开后排队通知仍要拒绝。
  健康检查和纠正合并到一个几何入口；第二次故障进入安全模式前，
  只允许一次由同一时钟调度的重新观察。
- 滚动逻辑使用 Kernel 可注入时钟，包括动画帧、时间和 timer；
  不使用真实 sleep 或额外重试时钟。目标偏移已经到达时提交 no-op，
  不能再次赋值 `scrollTop`。

## 范围和原生几何

- 原生几何是权威来源：距底部为
  `scrollHeight - scrollTop - clientHeight <= 4`。
  TanStack 只计算 prefix 大小与挂载范围，关闭其测量补偿和滚动旁路。
- Window Adapter 只绘制覆盖当前原生视口的候选范围。候选过期时保留最后一个
  覆盖快照：items、完整 prefix、总 extent、scroll margin 是同一个原子值。
  原生跳转让候选和旧快照都失效时，从 prefix ledger 重建一次，
  保留受保护块；仍无法覆盖时，在 paint 前进入共享 full-DOM 安全 renderer。
- 挂载预算按原生运动方向分配：保留反向缓冲，将余量用于前进方向。
  Resident 和 protected 块也计入整个 adapter 的 completed-block 上限。
  先退休已测量 resident prefix、缩减可选 overscan，不能扩大上限掩盖竞态。
- 使用 `Array.from` 固化第三方 lazy cache；可变 typed-array 的 Proxy
  不是不可变快照。原生输入拥有不变视口时，未经批准的测量通知不能替换范围。
  已批准测量批次则必须在 paint 前同时提交完整 prefix 和覆盖范围。
- 原生视口是外部 store，React range render 读取其不可变快照，不能提交
  基于旧 compositor offset 的范围。Window item 用绝对定位 `top`，
  避免 transform 与原生滚动被分别提交。

## 测量与锚点

- DOM 测量先进入按 block key 索引的 staging ledger，再改变 TanStack prefix。
  首次 materialization 必须在首次 paint 前发布实际大小与完整 prefix，
  不能等待手势结束。
- 一个 generation 内的 window origin 可在测量前置新块时保持共同可见位置。
  同时平移位置、extent、范围查询和 publication frontier；向原生前沿连续消耗
  origin，不能到零时突跳。输入结束后，以已提交 prefix anchor 在一次 prepaint
  中清除它。普通增长保留输入捕获的 Kernel anchor；materialization 完成后
  才确认几何。
- 后续大小变化期间，只要输入拥有 reader intent，整个已绘制视口不可变。
  旧 prefix 和真实 DOM 都必须把块放在视口之后，才能作为发布边界；
  Kernel anchor 只能推迟边界。可见及前置变化继续 staging，
  只有视口后的 overscan 可发布。
- `scrollMargin` 使用原生 scroller 坐标，包含 Transcript padding 和 prefix。
  输入结束后，在 Kernel 锚点恢复事务中发布 staging 大小；
  prefix 布局与锚点修正在一次 paint 前提交，取消旧排队几何工作。
- 保留第一个阅读锚点，后续块可随真实内容增长移动；不能冻结所有旧 top
  而让内容重叠。观察绝对定位块和 projection root，局部折叠不一定改变总 extent。
  Tail intent 不细化不可见 cold history 的后续大小，首次挂载仍需真实测量。
- Ledger 只拥有大小；输入 lease 属于 Kernel，adapter 不能另建一份。
  测量准入时重读物理视口：旧绘制 prefix 与新 DOM 都要确认发布边界在视口外，
  且额外预留一个视口。这个预留不是 compositor 运动上限。
  不能累加 wheel delta 建立 pending-distance 屏障，防止瞬时积压冻结所有测量。
  Wheel、touch、selection、keyboard、滚动条使用同一个几何边界，
  lease 期间 Kernel 拒绝程序化 reader 写入。
- 先发布一个不可变 Reasonix 快照，再在同一个浏览器 task 中将同批大小写入
  TanStack keyed cache；用 layout-effect state update 完成并确认几何提交，
  不能只依赖 TanStack notification。发布测量不能调用会清空 keyed cache 的
  `measure()`，不能依赖 idle timeout、TanStack 自主 ResizeObserver 发布，
  或平台专用滚动补偿。

## Resident 尾部、安全模式与验证

- 活动 turn 和至少两个最新 completed turn 保持普通 DOM。
  Resident block 距离视口至少一个视口，且不拥有 anchor/focus/selection endpoint
  时才可迁入 windowed history。修改边界前，将连续离开的 prefix 实测到同一个
  ledger 快照，不能按估计大小迁移。Reader intent 下流式增长零滚动写入。
- 同一 generation 内出现两次 blank/invalid/correction 异常，且中间没有健康帧，
  就切换到 full DOM，直到下一个 surface generation。正常 full、windowed
  和 safety 共享 presentation 与 observer 生命周期，不另建 renderer 栈或用户开关。
- 安全模式挂载已分页块，保留可信 cold-prefix 坐标、原生 keyed host、focus、
  selection 和 reader offset，不做结构性滚动写入；停止 eviction，
  不强制加载屏外历史和大正文。
- 修复共同的 ownership、projection、measurement 或 presentation 不变量，
  避免叠加场景重试、平台补偿或放松验收。
- 滚动行为变更必须在 `transcript-kernel.test.ts` 增加确定性事件序列，
  必要时补 viewport/projection 用例。提交前在 `desktop/frontend/`
  运行 `pnpm test:transcript`。
