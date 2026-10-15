# 飞书创建会话：远程目录选择的开源交互研究

研究日期：2026-10-15。目标是给当前已跑通的创建链路提供交互备选，尤其减少“为了开始任务，先操作一套目录管理器”的负担。本文只研究入口和选择语义，不建议照搬其他项目的进程启动、权限或模型配置。

## 证据范围

核对了 5 个开源项目的当前源码，以下链接固定到本次读取的提交；没有把 README 宣传、截图或个人印象当作交互已经实现的证据。本文没有登录这些产品逐个做 UI 实测，因此描述的是源码可验证的流程，不提供体验评分或跨项目点击数排名。文中的“远程”指运行 agent/bridge 的机器，与手机、浏览器本机的文件选择器不同。

| 项目 | 已核验版本 | 入口类型 | 可借鉴的核心区别 |
| --- | --- | --- | --- |
| [Agent Bot](https://github.com/keyou/agent-bot/tree/d4de9ff2f593b1a8a80f0c43d52119b149575bcf) | `d4de9ff` | 飞书 `/dir` 目录卡 | 按目录浏览，在当前目录执行“新任务/新群任务” |
| [CCGram](https://github.com/jsayubi/ccgram/tree/75b38e1bab4af411b40b4e589d050786eb65ceca) | `75b38e1` | Telegram `/new [项目名]` | 已知项目优先；点项目直接启动或切换会话 |
| [HAPI](https://github.com/tiann/hapi/tree/5847b034e5cb84f2617766945c994b7905c5c03e) | `5847b03` | Web 新建表单、远程目录面板 | 最近路径只是预填；独立目录面板回到新建表单 |
| [Happy](https://github.com/slopus/happy/tree/b0fc933dd423ee562207d1788c65b4b9cc4960d1) | `b0fc933` | App/Web 新建输入器和内嵌选择面板 | 将机器、已知项目、路径与首次任务放在一处 |
| [agents-to-im](https://github.com/francize/agents-to-im/tree/92394e478801d872ada6f85d8f08bd38553946b6) | `92394e4` | 飞书 `/new` 表单卡 | 一张卡选最近工作区，再按模式按钮提交 |

本仓库旧研究提及的 `code-anywhere` 没有可唯一定位的仓库链接。本次未确认其准确开源身份，不拿同名商业 Codeanywhere 或搜索结果代替证据；OpenCode/TUI 的本地项目入口也未用作远程目录选择的直接先例。

## 已验证的交互

### 1. Agent Bot：目录浏览器是一种独立工具

`/dir` 默认打开当前任务目录，其次绑定项目目录，再次配置默认目录；指定参数可以打开其他目录。服务端只 `readdir` 当前目录，排序后切片分页；打开子目录和翻页会更新原消息卡。这里读取的是 bot 所在环境的文件系统，并非用户手机的文件列表。[目录入口及分页实现](https://github.com/keyou/agent-bot/blob/d4de9ff2f593b1a8a80f0c43d52119b149575bcf/src/proxy/ProxySessionController.ts#L6945-L7053)

卡片把“当前目录”及目录操作放在列表上方；“打开目录”和“在此新建任务”是不同动作。`directory_new`、`directory_new_group` 会立即进入新任务/新群任务执行流程，不是给另一张创建表单填值。[目录卡布局](https://github.com/keyou/agent-bot/blob/d4de9ff2f593b1a8a80f0c43d52119b149575bcf/src/feishu/CardRenderer.ts#L1622-L1644)、[动作处理](https://github.com/keyou/agent-bot/blob/d4de9ff2f593b1a8a80f0c43d52119b149575bcf/src/proxy/ProxySessionController.ts#L1131-L1184)

**设计推论：**适合“我知道目录的大致位置，需要沿层级探索”。它也说明目录浏览可成为次级工具；无需让每次创建都先进入浏览器。目录内的文件发送、新建文件夹等能力不是本次创建入口的必要条件。

### 2. CCGram：项目名快捷入口，选择动作可能已经执行

无参数 `/new` 给出最多 10 个项目按钮；有参数时先查配置根下的精确目录，再做直接子目录名称前缀匹配。唯一命中继续启动，多命中返回按钮。不是递归搜索整台机器，也不是真正的任意绝对路径目录浏览器。[命令和匹配实现](https://github.com/jsayubi/ccgram/blob/75b38e1bab4af411b40b4e589d050786eb65ceca/workspace-telegram-bot.ts#L817-L908)

按钮回调直接调用 `startProject`。其中存在“已有 session，设为默认”的分支，所以 `/new` 并不总表示新建一个独立原生会话；不能把这种确认语义直接移植到本桥接器。[已有会话分支](https://github.com/jsayubi/ccgram/blob/75b38e1bab4af411b40b4e589d050786eb65ceca/workspace-telegram-bot.ts#L911-L925)、[按钮回调](https://github.com/jsayubi/ccgram/blob/75b38e1bab4af411b40b4e589d050786eb65ceca/workspace-telegram-bot.ts#L1455-L1463)

“最近项目”列表实际混合了配置根直接子目录的修改时间、持久使用历史，并把固定项目排在前面；不是纯粹的“本人最近创建会话”。这是它的列表排序策略，不是可以用于本项目未知创建结果恢复的证据。[项目列表来源和排序](https://github.com/jsayubi/ccgram/blob/75b38e1bab4af411b40b4e589d050786eb65ceca/workspace-router.ts#L431-L501)

**设计推论：**输入短项目名比多层浏览更适合记得名字的用户。但本项目应让命中结果先成为明确的已选目录，保留最后一次“创建”确认；同名项目必须显示路径和机器，不能只显示 basename。

### 3. HAPI：已知路径建议与真实远程浏览是两种能力

新建表单同时提供路径输入和最近路径按钮；点击最近项或建议只执行 `setDirectory`，实际启动由单独的 Create 动作处理。成功后才更新最近路径。不能将“点最近项”描述成直接创建。[目录输入和最近项](https://github.com/tiann/hapi/blob/5847b034e5cb84f2617766945c994b7905c5c03e/web/src/components/NewSession/DirectorySection.tsx#L58-L138)、[选中逻辑](https://github.com/tiann/hapi/blob/5847b034e5cb84f2617766945c994b7905c5c03e/web/src/components/NewSession/index.tsx#L1474-L1486)、[成功记录](https://github.com/tiann/hapi/blob/5847b034e5cb84f2617766945c994b7905c5c03e/web/src/components/NewSession/index.tsx#L1687-L1694)、[Create 绑定](https://github.com/tiann/hapi/blob/5847b034e5cb84f2617766945c994b7905c5c03e/web/src/components/NewSession/index.tsx#L2022-L2029)

路径建议合并当前机器 session 的路径、worktree 基础路径和最近项，不扫描文件系统。最近项按机器分组，保存在浏览器 `localStorage`，每台机器最多 5 项；这不等于跨设备的用户收藏。[建议数据源](https://github.com/tiann/hapi/blob/5847b034e5cb84f2617766945c994b7905c5c03e/web/src/hooks/useDirectorySuggestions.ts#L4-L31)、[最近路径存储](https://github.com/tiann/hapi/blob/5847b034e5cb84f2617766945c994b7905c5c03e/web/src/hooks/useRecentPaths.ts#L3-L45)

另一条 Browse 路径使用指定 `machineId` 的 `list-directory` API 读取远程目录。面板的 Start Session 按钮把当前机器和目录带到 `/sessions/new`，仍然只是预填，不会在浏览面板直接 spawn。[远程目录 API](https://github.com/tiann/hapi/blob/5847b034e5cb84f2617766945c994b7905c5c03e/web/src/api/client.ts#L803-L815)、[面板加载及选择](https://github.com/tiann/hapi/blob/5847b034e5cb84f2617766945c994b7905c5c03e/web/src/components/WorkspaceBrowser.tsx#L215-L234)、[返回新建页](https://github.com/tiann/hapi/blob/5847b034e5cb84f2617766945c994b7905c5c03e/web/src/router.tsx#L990-L1006)

**设计推论：**这是“常用选择”和“完整浏览”分层的强先例。Web 输入建议可以随键盘变化，但不能据此推定飞书卡片也能每键异步搜索。浏览器本地历史也不能直接当作飞书服务端现成的数据。

### 4. Happy：先选机器，再在输入器旁选择项目/路径

内嵌 PathPicker 有自定义路径输入和已知项目/路径项。点击普通路径只更新输入值，点击项目按项目身份更新草稿；普通创建入口最终由 Send 调用 `handleSend`，其中向选定机器发送 spawn 请求。选择和执行是可区分的动作；这里描述普通手动入口，不概括代码中其他自动提交或恢复分支。[选择处理](https://github.com/slopus/happy/blob/b0fc933dd423ee562207d1788c65b4b9cc4960d1/packages/happy-app/sources/app/%28app%29/new/index.tsx#L575-L594)、[项目选择](https://github.com/slopus/happy/blob/b0fc933dd423ee562207d1788c65b4b9cc4960d1/packages/happy-app/sources/app/%28app%29/new/index.tsx#L1199-L1210)、[spawn 调用](https://github.com/slopus/happy/blob/b0fc933dd423ee562207d1788c65b4b9cc4960d1/packages/happy-app/sources/app/%28app%29/new/index.tsx#L1598-L1630)、[Send 按钮](https://github.com/slopus/happy/blob/b0fc933dd423ee562207d1788c65b4b9cc4960d1/packages/happy-app/sources/app/%28app%29/new/index.tsx#L2217-L2227)

候选由机器 session 元数据汇集，去掉归档会话，合并已知路径和项目身份；某些项目没有客户端可知的根路径，按 project ID 提供。源码中的 Recent 标签并不能证明这些项是实时远程目录搜索结果；该候选收集函数没有列盘逻辑。[候选收集实现](https://github.com/slopus/happy/blob/b0fc933dd423ee562207d1788c65b4b9cc4960d1/packages/happy-app/sources/sync/agentSessionPlaces.ts#L76-L141)

**设计推论：**用户的主要工作是写任务，项目/目录是输入器旁可修改的上下文。对本项目可借鉴布局和“选中后明确显示”，不能照搬其项目目录推断或权限选项；原生 project ID 仍需本项目的精确根目录校验。

### 5. agents-to-im：飞书一张表单卡已有直接先例

新建 Codex 卡把最近工作区 `select_static` 和提交模式按钮放在同一 form。用户先选工作区，模式按钮提交表单；处理器读取 `form_value` 后才执行 `createBoundSession`。不是点选下拉项立即建群或建会话。[选择组件](https://github.com/francize/agents-to-im/blob/92394e478801d872ada6f85d8f08bd38553946b6/src/feishu/cards/session-cards.ts#L100-L140)、[完整表单](https://github.com/francize/agents-to-im/blob/92394e478801d872ada6f85d8f08bd38553946b6/src/feishu/cards/session-cards.ts#L174-L216)、[提交处理](https://github.com/francize/agents-to-im/blob/92394e478801d872ada6f85d8f08bd38553946b6/src/feishu/handlers/session-handler.ts#L75-L99)

工作区来源是已有 binding、额外已知来源与配置默认目录，按更新时间合并、路径去重，默认上限 5；不是任意机器目录枚举。[最近工作区函数](https://github.com/francize/agents-to-im/blob/92394e478801d872ada6f85d8f08bd38553946b6/src/infra/recent-workspaces.ts#L32-L74)

**设计推论：**可作为“一张卡完成常见路径”的真实飞书实现参考。它是组件组合的源码证据，不代替当前飞书官方文档或当前 App 的实机验收；本次主代理另行核验组件约束。

## 三种不同方向

以下均为本项目的设计候选，不是上述项目已经提供的飞书功能。演示里的机器名、项目和路径必须为虚构示例。三个方向可以共享当前创建后端，但第一版应分别呈现，避免把所有入口堆进同一张卡后无法判断用户偏好。

| 方向 | 首屏与核心操作 | 适合的任务 | 代价与边界 |
| --- | --- | --- | --- |
| A：项目/常用目录就在创建卡 | 任务正文下方直接选项目或常用目录，显示完整选中路径；一个“创建”按钮；“其他目录”作为次入口 | 经常在少数已知项目发任务 | 冷启动没有常用项时需要兜底；长列表需限额，同名项不能只显示短名 |
| B：明确查询后选结果 | 输入项目名或绝对路径 → 按“搜索/前往” → 在结果中选中 → 回到创建摘要再创建 | 知道名称片段或能粘贴路径，不愿逐层点目录 | 必须解释搜索范围；卡片请求有往返等待，不能伪装成每键自动补全 |
| C：独立远程目录面板 | 创建卡点“选择目录”打开新增 H5 页面；面包屑、路径栏、目录列表；按“使用此目录”回填原草稿 | 偶尔要探索深目录、从 `/` 寻找位置 | 新增 Web 入口、鉴权、回填协议和运维面；有页面跳转及返回成本 |

### A 的演示脚本：让任务成为主角

```text
新建任务 · remote-dev
[检查上传失败的原因……                    ]

工作目录  [ ai-tools / codex-feishu        ▾ ]
          /data/home/demo/ai-tools/codex-feishu
          Codex 项目 · codex-feishu
[其他目录]

接收位置  新建专属群
                                 [创建]
```

选择只更新表单中的候选值，不触发创建；创建时一次提交正文、目录和接收位置。已确认常用目录可以直接预选，但要明确显示；没有合法默认值时显示“请选择目录”，不要猜当前 daemon cwd。项目、配置常用目录可先支撑此版；“最近使用”“收藏”“置顶”是可独立评估的数据功能，不是做出该布局的前提。参考 agents-to-im 的同卡表单，以及 HAPI/Happy 的“选择先改草稿”。

### B 的演示脚本：由用户描述目标位置

```text
选择 remote-dev 上的目录
[项目名或绝对路径：codex                  ] [搜索]

在已知项目与常用目录中找到：
( ) codex-feishu   /data/home/demo/ai-tools/codex-feishu
( ) codex-tools    /data/home/demo/tools/codex-tools

[按绝对路径前往]  [逐层浏览]        [使用选中目录]
```

“项目名搜索”最小实现只过滤已知项目/常用目录；必须标这个范围。绝对路径“前往”则在远程机器规范化并校验该路径，不能拿未完成搜索的输入直接创建。若需要列出未知目录，只读取给定父目录的直接子目录并分页；整机递归模糊搜索需要另行设计索引、成本和权限边界，本方案不默认包含。参考 CCGram 的名称入口，但保留本项目的明确选择和最后创建确认。

卡片版按显式按钮提交查询、更新原卡；原任务正文由服务端草稿保留。不要做成“输入框每敲一个字就请求后端”的假演示，也不要用“搜索目录”笼统承诺能搜遍远程机器。

### C 的演示脚本：把目录探索交给专用空间

```text
remote-dev                       [返回创建卡]
[ /data/home/demo/ai-tools             ] [前往]
/  > data > home > demo > ai-tools

目录                 状态
codex-feishu/         可使用       [进入]
shared/              仅可浏览     [进入]
tools/               可使用       [进入]

当前 /data/home/demo/ai-tools
                              [使用此目录]
```

这必须标为**新增 H5 远程目录面板**，不是现有飞书卡的扩展组件，也不是 HTML 本地文件选择器。页面请求 bridge 服务读取目标远程机器；只读父目录可以进入，但“使用此目录”要求满足最终 cwd 条件。可借鉴 HAPI 的 `machineId + list-directory` 与“回填新建页”分工。

回填应绑定原草稿、创建者、目标机器和 revision；成功后回到创建摘要，保留正文、接收位置，再按“创建”。过期/并发修改时要求刷新草稿，不能让旧页面覆盖新选择。页面认证、URL 可达性、飞书容器跳转、返回时状态恢复均是新增工作，必须在选择该方向后单独验收，不能在静态 demo 中宣称已解决。

## 演示与验收关注点

下面是设计验收要求，不是外部项目事实：

- 三版使用同一段任务、同一台虚构机器、同一批目录；演示都包括“已知项目”“未知但已知绝对路径”“只读父目录中的可写子目录”。让差异来自交互结构，不靠不同示例美化某一版。
- 每版都能清楚辨认“正在浏览”“已选中”“开始创建”三个状态；选择目录本身不产生远程会话。返回/取消目录选择不丢最初任务正文。
- 机器名和最终完整路径始终可查；同名目录显示父路径；项目身份只有在真实项目根精确匹配时展示，普通目录显示“独立目录”。
- 无结果、无权限、路径不存在、分页加载失败各有下一步；只读目录保留进入能力、隐藏/禁用最终选择。搜索结果与默认项在提交时重新校验，不能把列表快照当最终授权。
- 创建中的进度、明确拒绝后的重试、未知结果后的核验继续沿用当前状态机；目录体验改版不应带来隐式线程、自动重试或 sandbox/approval/model 覆盖。

待用户选择的是 A/B/C 的入口偏好，以及是否值得为 C 新增 Web 页面。“最近/收藏”“目录索引搜索”“更多任务接收模式”应分别讨论，不能混进本次目录体验比较成为隐含承诺。


## 飞书实现约束与本轮 demo

主代理另行核对了飞书官方当前文档。`select_static` 提供命名选项、默认项和回传语义；表单容器把一批输入在提交时统一回传，输入框文档也明确在 form 中通过提交按钮发送。因此 A/B 首版按已确认的选择 / 显式提交路径设计，不把 Web 输入联想外推成飞书逐键远程回调。独立路径摘要若需随选择改变，需配置短回调更新同卡或把完整路径放进选项标签；不能假设表单能执行任意自定义联动。[单选下拉](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/single-select-dropdown-menu)、[输入框](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/input)、[表单容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container)

本轮对话中的三版可点击预览分别命名为“项目速选”“搜索直达”“目录面板”。任务、devbox 主机名、路径及目录权限全部是预览 fixture，不读取真实远程文件树。A 用项目下拉与完整选中路径，默认显示可靠的上次选择，绝对路径作为次入口；B 按按钮查询已知项目或指定路径的直接子级，展示同名目录的完整父路径；C 用独立目录面板、面包屑、路径前往和当前目录筛选，按“使用此目录”回到任务卡后再创建。所有版本均把跟进位置收在次级选项，保留默认专属群。

C 在预览中展示的是新增 Web 页的交互结构，现有运行服务没有新增对应 Web 接口或认证入口；其鉴权、可达性和安全回填仍是待选方案的实现工作。三版都是交互原型，不作为真实飞书新卡渲染通过的证据。用户目前确认的是原创建链路功能通过，并明确要求改进体验；这是初始对比原型的状态；用户的最终组合选择和部署约束见下方补记。


本轮预览已完成浏览器渲染和自动点选检查：736px 浅色桌面、320px 浅色触屏、390px 深色触屏。三种宽度均走完项目/路径选择、同名项目区分、无结果、只读目录拒绝、中文与空格路径、取消保留选择、话题缺工作群反馈和创建摘要；检查无页面脚本异常、无横向溢出及裁切的控件，并人工查看桌面和手机渲染。该结果只验证本地交互原型，不提升真实飞书组件、H5 鉴权或完整发布矩阵。


## 用户选择与最终落地（2026-10-15）

用户要求三种能力组合，并说明没有可达 HTTPS 域名，希望复用长连接。最终保留 A 的最近 / 项目速选、B 的显式查询和 C 的路径探索能力，将 C 的承载方式改为原生飞书卡片：路径导航、绝对路径前往、当前层筛选、分页、选择后回填同一草稿。卡片动作沿用现有 WebSocket 接收，远程桥进程读取目录并更新原卡；不新增 Web 服务、域名、反向代理或 OAuth。初始 C 的 H5 原型只作为研究参考，不进入生产依赖。

主卡优先显示速选与预填任务，跟进方式默认专属群并收在折叠设置。搜索时先选用结果回到主卡，再点击创建，避免两个目录选择器同时生效。目录范围、真实路径与权限仍在服务端核验；选择不创建群、会话或首条输入。搜索 / 路径失败保留任务编辑，导航和换页保留原草稿。

合并后的主卡、非空搜索、空搜索和目录面板已在原专用验收群更新既有预览，真实飞书 API 均接受并读回。该证据覆盖组件 schema，不能代替用户手机布局、卡片回调和实际任务创建验收。
