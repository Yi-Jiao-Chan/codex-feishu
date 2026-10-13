# 执行过程折叠标题与记录间距

日期：2026-10-13。范围：核对当前 `taskHistory` JSON、飞书 Card JSON 2.0 官方字段、第一方示例代码与已安装 Go SDK v3.12.0。只写本研究；未改 renderer、配置或服务，未发送卡片。用户截图由主代理查看，本研究没有独立进行客户端渲染验收。

## 可直接采用的字段

**折叠标题的字号和颜色放在 `header.title` 文本对象里：`text_size:"notation"`、`text_color:"grey"`。不是 `header.text_size`，也不由 children 的字号继承。** 官方普通文本规格列出 `notation` / `small` 为 12px，`x-small` 为 10px，`normal` / `medium` 为 14px；颜色字段叫 `text_color`，仅对 `plain_text` 生效。[普通文本官方规格](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/plain-text.md)

证据边界：折叠面板专页的 `header.title` 表只展开 `tag` / `content`，没有单独列出字号和颜色。因此不能仅从该表宣称全部通用文本属性都适用于标题。第一方 `larksuite/openclaw-lark` 在固定提交 `dde0be3680d6fd5443cab426c8f4b3216266346a` 中，确实把 `text_color:'grey'`、`text_size:'notation'` 放在折叠面板的 `header.title` 内，且把该卡转换为 schema 2.0；这提供了准确字段层级的第一方实现依据。它仍不是本用户客户端的现场验证。[第一方 builder，738–747 行](https://github.com/larksuite/openclaw-lark/blob/dde0be3680d6fd5443cab426c8f4b3216266346a/src/card/builder.ts#L738)、[另一个折叠面板，830–839 行](https://github.com/larksuite/openclaw-lark/blob/dde0be3680d6fd5443cab426c8f4b3216266346a/src/card/builder.ts#L830)、[schema 2.0 转换](https://github.com/larksuite/openclaw-lark/blob/dde0be3680d6fd5443cab426c8f4b3216266346a/src/card/builder.ts#L766)

| 目的 | 正确路径／值 | 已核实含义 |
| --- | --- | --- |
| 弱化折叠标题 | `header.title.text_size:"notation"` | 12px；也可用 `small`，建议与现有 notation 命名一致 |
| 灰色标题 | `header.title.text_color:"grey"`，`tag:"plain_text"` | 灰色 token 随浅／深主题取值；不是任意 CSS `color` |
| 展开记录字号 | `elements[i].text_size:"notation"`，`tag:"markdown"` | Markdown 组件的直接字段；12px，正文优先时可保留 |
| 单条记录外距 | `elements[i].margin:"0px"` | 组件外距；不控制该 Markdown 内部段落 |
| 不同记录间距 | 面板自身 `vertical_spacing:"4px"` 或 `"0px"` | 不同 children 之间的间距，合法范围包含 0；不是 `header.vertical_spacing` |
| 面板／标题内距 | 面板 `padding:"0px"`、`header.padding:"0px"` | 分别控制内容容器和标题区域 |
| 面板外距 | 面板 `margin:"0px"` | 与其他卡片元素的外距 |

上述子组件字号、margin 见 [Markdown 官方规格](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/rich-text.md)；面板与标题的 padding、margin、vertical_spacing 见 [折叠面板官方规格](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/collapsible-panel.md)。官方折叠页的 vertical_spacing 表格存在“水平间距”措辞与 JSON 示例“垂直间距”不一致，本研究按字段名、垂直 direction 及示例解释为垂直 children 间距，未从笔误推导额外属性。

`grey` 是合法主题颜色 token，普通浅色主题值为 `#646a73`，深色主题值为 `#a6a6a6`。Markdown 字段表没有直接 `text_color` / `font_color` 字段；其文字颜色采用文档支持的 `<font color=...>` 内容语法。不要把普通文本的颜色字段直接复制到 Markdown 对象，也不要给 `<font>` 编造字号属性。本例保留记录正文默认颜色。[颜色枚举](https://open.feishu.cn/document/feishu-cards/enumerations-for-fields-related-to-color.md)、[Markdown 语法](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/rich-text.md)

## 当前实现为什么显得疏

调查时的 [taskHistory](../../internal/cards/task.go) 没有设置 `header.title` 字号／颜色；只在其唯一的 Markdown child 上设了 notation。标题因此不能据 child 的 12px 推定为 12px。

记录用 `strings.Join(parts,"\n\n")` 合成**一个** Markdown。记录间的空行变成 Markdown 段落；面板的 `vertical_spacing:"8px"` 只控制 children 间距，不能压缩这一个 child 内的段落。当前官方 Markdown 字段表未提供 `paragraph_spacing`、`line_spacing`、`line_height` 或 CSS style 属性，不能把这些名字当有效协议字段。[Markdown 字段表与语法](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/rich-text.md)

需要明确控制**记录之间**的间距时，用每个公开 record 一个 Markdown child，并由 panel 的 `vertical_spacing` 控制；保留每个 record 原有 Markdown 内容及内部换行。不要对完整记录进行 TrimSpace 或全局空行替换，以免改坏代码块、列表或引用。拆 child 后仍须遵守已有总字节预算，并计算组件数量；官方创建卡片接口的 300305 说明 Card JSON 2.0 上限为 200 个元素／组件（包括 plain_text 元素），不能把无界历史直接拆为无界 children。这里引用数量约束，不建议改用 Cardkit 发送流程。[官方 300305 与组件上限](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/cardkit-v1/card/create.md)

## 单行省略与逐条展开

截至本次核对，折叠面板官方字段表没有 `header.title_lines`，也没有明确声明 `header.title.lines` 控制标题溢出。普通文本组件确实有 `div.text.lines`，超出行数显示省略；但不能把普通 div 的行数契约无条件移植到折叠标题。上述第一方面板代码证明了字号／颜色层级，没有使用 lines，也不补足标题单行省略的证据。此处结论是“折叠标题的行数控制尚未核实”，不是“飞书所有文本都没有 lines”。[折叠面板字段表](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/collapsible-panel.md)、[普通文本 lines 字段](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/plain-text.md)

用户要求每条记录默认短摘要、点击展开全文且完成后仍能逐条展开。可在外层 execution_history 中放每条记录自己的 nested collapsible_panel；外层与每个内层都设置 `expanded:false`，完成后只更新外层状态文案，不移除或改为展开内层。两层容器位于官方最多五层嵌套限制内；组件总数和字节预算仍要同时遵守。[折叠面板嵌套规则](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/collapsible-panel.md)

本期建议生成**无换行、长度受限的 plain_text 摘要**，超长明确附加省略号，完整原文保留在该条 child Markdown 中。不填未核实的 title_lines / title.lines，不用 CSS ellipsis。无换行加手工截短只保证摘要字符串简短，不是客户端单行布局证明；窄窗口、不同设备和字体缩放仍可能折行，需要用户当前客户端验收。只对显示摘要归并空白，不改变展开全文。

## 最小 JSON（逐条默认折叠）

以下是可放入现有 `body.elements` 的外层及一条记录示例；为每条 record 重复其 nested panel。两级标题 12px 灰色，展开全文 12px 默认正文色，记录容器之间 4px，无额外内／外边距。保留稳定外层面板 ID。真实记录必须继续通过原有正文安全编码；需要投递标记的 Markdown child 不应预占固定 element_id。

```json
{
  "tag": "collapsible_panel",
  "element_id": "execution_history",
  "expanded": false,
  "padding": "0px",
  "margin": "0px",
  "vertical_spacing": "4px",
  "header": {
    "title": {
      "tag": "plain_text",
      "content": "执行过程",
      "text_size": "notation",
      "text_color": "grey"
    },
    "padding": "0px",
    "icon": {
      "tag": "standard_icon",
      "token": "down-small-ccm_outlined",
      "color": "grey",
      "size": "12px 12px"
    },
    "icon_position": "right"
  },
  "elements": [
    {
      "tag": "collapsible_panel",
      "expanded": false,
      "padding": "0px",
      "margin": "0px",
      "vertical_spacing": "0px",
      "header": {
        "title": {
          "tag": "plain_text",
          "content": "检查配置…",
          "text_size": "notation",
          "text_color": "grey"
        },
        "padding": "0px",
        "icon": {
          "tag": "standard_icon",
          "token": "down-small-ccm_outlined",
          "color": "grey",
          "size": "12px 12px"
        },
        "icon_position": "right"
      },
      "elements": [
        {
          "tag": "markdown",
          "text_size": "notation",
          "margin": "0px",
          "content": "检查配置。\n\n保留本条记录原有的完整公开正文和 Markdown。"
        }
      ]
    }
  ]
}
```

若希望记录之间没有额外间距，只将外层面板 `vertical_spacing` 改为 `"0px"`。这不抹去记录自身的段落，不能承诺任意公开 Markdown 都具有相同行高。工具统计是否显示是主任务产品决定，不需要新增排版字段；本示例不包含工具统计。实际渲染应保留已有的状态、等待／未知提示与省略记录提示。

## SDK 核对与验证边界

安装版本 `github.com/larksuite/oapi-sdk-go/v3@v3.12.0` 的 `card/model.go` 仍使用根级 `elements` / `i18n_elements` 的传统 MessageCard builder；没有找到 Card JSON 2.0 collapsible_panel 的强类型结构。项目使用 map 构造 2.0 JSON，再经 SDK transport 发送，SDK 编译通过不能证明这些 typography 字段的服务端支持或客户端视觉效果。[固定版本 SDK MessageCard](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/card/model.go#L37)、[项目发送入口](../../internal/feishu/gateway.go)

本研究用官方 `.md` 正文核对字段，并用第一方固定提交补充标题属性层级；未依赖搜索结果中的第三方转载。尚须主代理在用户实际客户端确认标题大小、记录间距及展开／收起效果。已有 IM GET 会省略部分字号和布局字段，不能用读回缺字段或 JSON marshal 成功替代视觉验收，参见 [已有读回边界](feishu-card-readback-normalization-2026-10-13.md)。
