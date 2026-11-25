# Extension Host 与插件前端设计规范

本规范适用于独立宿主的管理页（`web/`）、仓库内带 Web 页面的示例插件（`examples/`），以及基于 Extension Host 交付的插件前端。新增或修改界面时，以 GoDesk 的设计 Token、应用布局、组件与交互要求为基线。GoDesk 内置的「扩展」管理页是独立管理页的交互参考。

## 设计来源

同级工作区中的 `../goDesk/frontend/docs/ui-design/` 是设计规范的来源，`../goDesk/frontend/src/styles/theme.css` 是已实现的 CSS 变量参考。本页依据 GoDesk 提交 `d221276` 整理。更新界面时先对照目标 GoDesk 版本的对应章节。共享 Token、断点或交互语义有调整时，同步更新受影响的宿主页、插件及本页基线。

| 主题 | GoDesk 参考 |
| --- | --- |
| 原则与全套规范索引 | [01 设计原则](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/docs/ui-design/01-design-principles.md) |
| 颜色、排版、尺寸和动效 | [02 设计 Token](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/docs/ui-design/02-design-tokens.md)、[theme.css](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/src/styles/theme.css) |
| 桌面与窗口职责 | [03 桌面外壳](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/docs/ui-design/03-desktop-shell.md)、[04 窗口](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/docs/ui-design/04-window-guidelines.md) |
| 应用内容与组件 | [05 应用布局](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/docs/ui-design/05-application-layout.md)、[06 基础组件](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/docs/ui-design/06-component-guidelines.md) |
| 键盘、响应式与主题 | [07 交互](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/docs/ui-design/07-interaction-guidelines.md)、[08 响应式](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/docs/ui-design/08-responsive-guidelines.md)、[09 主题](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/frontend/docs/ui-design/09-theme-guidelines.md) |

## 宿主与插件职责

| 界面 | 职责与边界 |
| --- | --- |
| GoDesk 桌面 | 管理桌面、开始菜单、任务栏、窗口标题栏、窗口大小、层叠和跨窗口焦点。「扩展」管理插件生命周期，插件通过同源 iframe 显示业务页面。 |
| `ext` 管理页 | 作为普通网页提供安装、配置、启用、启动、停止、禁用、卸载和打开插件的入口。当前实现从列表在新标签页打开插件。 |
| 插件页面 | 提供业务导航、工具栏、工作区和插件内的反馈。嵌入时使用宿主标题栏，页面标题说明当前业务任务。菜单和确认弹窗限制在插件文档内，保留宿主窗口控制入口。 |

同一个插件 ZIP 必须能在两种宿主运行。使用包内相对路径加载构建产物，插件业务请求和事件使用共享契约下的插件路由。前端路由需要客户端切换时可使用 hash 路由。将所需样式、图标和其他资源随包交付，独立运行时可直接加载。React、Tailwind 和 WMKit 是 GoDesk 的实现选型，宿主和插件可以用原生 HTML、CSS、JS 或已有框架实现相同设计。 [GoDesk 插件契约](https://github.com/EziosWJ/GoDesk/blob/d221276ad5bc02df34fba8d5784b098d4706333d/docs/extensions.md) · [独立宿主入口](../web/app.js)

## 视觉与主题

- 在各自文档的主题样式中集中声明所用的 `--react-web-desktop-<角色>` 变量，取值对照 GoDesk 两套主题。正文使用 `surface` / `fg`，工具栏用 `subtle`，浮层用 `raised`。装饰性分隔用 1 px `divider`，依靠边线识别的控件用 `border`。大面积使用实色中性表面。主按钮用 `accent` / `on-accent`，选中用整项 `selected`，焦点使用独立的 `focus` 环。禁止在行首用加宽边框、伪元素或内阴影模拟强调色条。
- 文本按 GoDesk 的字号与行高组织，正文 14 / 20、紧凑表头 13 / 18（字重 500）、辅助文字 12 / 16（中文用字重 400）、分组标题 16 / 24、页面或弹窗标题 20 / 28。以 16 px 根字号换算为 `rem`。常规字体优先 Figtree Variable 和 Noto Sans SC Variable，路径、日志和代码优先 JetBrains Mono Variable，允许使用 GoDesk 的系统字体回退。字体随包分发时保留许可证。表格数字使用 `tabular-nums`。优先使用 Lucide 图标，行内 16 px、工具栏 20 px，图标尺寸与命中区独立。
- 间距采用 GoDesk 的 4 px 标尺，内容内边距紧凑 12 px、标准 16 px，控件圆角 6 px、浮层圆角 8 px。工具栏最小高度 44 px，精细指针控件紧凑 32 px、标准 36 px，触控目标至少 44 × 44 CSS px。数据行通常至少 40 px，密集行可用 32 px，可交互行在触控下至少 44 px。反馈与浮层过渡分别使用 120 ms、180 ms 和 `ease-out`，在减少动效模式下即时更新状态。
- 浅色和深色覆盖表面、表单、滚动条、菜单、弹窗、状态与焦点。在当前文档设置匹配的 `color-scheme`。独立管理页和直接打开的插件页默认跟随 `prefers-color-scheme`。嵌入时由 GoDesk 外壳决定主题，插件在自身样式中通过 `prefers-color-scheme` 响应 iframe 的颜色方案。iframe 有独立的 CSS 和 DOM，需在插件包内提供主题变量及字体样式。主题切换应保留选择、输入、滚动与焦点。

## 布局与交互

- 使用能完成任务的最少窗格。按内容根容器的内联尺寸重排，`<600px` 用通栏或顺序视图，`600px ≤ 宽度 < 960px` 可用导航＋主区或列表＋详情，`≥960px` 且确有对照需求时可加检查器。分栏宽度沿用 GoDesk 导航最小 192 px、主区最小 360 px、检查器最小 256 px，空间不足时减少栏数。优先用 CSS 容器查询。GoDesk 的 640 px 视口阈值用于切换桌面窗口模式。
- 工具栏只承载当前视图的动作，保留一个可见的首要操作，低频操作收进「更多」。窄时优先保留对象名、状态和主操作，检查器改为具名详情视图并提供返回入口。表格和日志需要二维浏览时在局部滚动。重排时保留筛选、选中项、输入和可恢复的滚动位置，收起焦点所在控件时将焦点移到可见的等价入口。
- 优先使用原生按钮、链接、表单和表格。字段配可见标签，错误贴近字段并与控件关联。图标按钮提供可访问名称。操作具备可见入口和键盘路径，悬停、选中、按下、焦点可区分。菜单支持方向键、执行键和 Escape，关闭后回到有效触发点。危险确认写明对象和后果，初始焦点放在安全动作，Tab 留在模态内，Escape 取消并归还焦点。确认内容过长时可在弹窗内滚动。
- 以文字和语义色表达状态，可搭配图标。加载、空列表、筛选无结果和请求失败分别给出对应反馈。异步操作显示进行中、避免重复提交，失败保留原因和可执行的下一步。插件管理界面沿用「启用」「启动」「停止」「禁用」「卸载」语义。停止保留启用意图，禁用清除启用意图，已启用但停止和启动失败均可重新启动。卸载确认说明默认保留业务数据，勾选或选择清除时明确不可恢复。
- 无障碍目标为 WCAG 2.2 AA。普通文字对比度至少 4.5:1，大文字至少 3:1，识别控件和状态所必需的非文本元素至少 3:1。按 GoDesk 规范使用 2 px 焦点环和 2 px 偏移，焦点环与相邻背景至少 3:1，且焦点保持可见。强制色彩模式下保留系统选中标识和焦点边线。

## 验收

- 改动插件页面时，用同一个 ZIP 在 GoDesk 窗口和 `ext` 中打开，检查静态资源和受影响的业务流程。改动独立管理页时，对照 GoDesk「扩展」验证同一操作的标签、状态、反馈和后果。
- 涉及布局时，在窄页和大屏小窗口验证内容重排。覆盖变更涉及的 599/600、959/960 CSS px 容器边界，并检查 320 CSS px 回流、200% 文本缩放和触控操作。确认关键动作可达，表格和日志的横向滚动限制在各自区域。
- 涉及主题或交互时，检查浅色、深色、跟随系统。在 GoDesk 中用与系统相反的主题打开插件并切换外观，检查 iframe 内文字、背景和表单是否同步。用键盘走通受影响的流程，检查焦点可见、弹窗取消后归还焦点、状态含义清晰。需要浏览器验证或截图时，优先使用已配置的 Playwright MCP。
