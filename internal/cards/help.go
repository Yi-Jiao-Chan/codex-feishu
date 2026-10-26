package cards

import "strings"

// CommandHelp describes chat commands without reading or changing their state.
// Keep disabled features visible so every command has a discoverable help topic.
func CommandHelp(topic string, creationEnabled, previewEnabled bool) string {
	creationState, previewState := "已启用", "已启用"
	if !creationEnabled {
		creationState = "未启用"
	}
	if !previewEnabled {
		previewState = "未启用"
	}
	commands := []struct {
		name, summary, details string
	}{
		{"help", "列出全部指令，或查看某条指令的用法。",
			"用法：`/help` 或 `/help 指令名`。\n\n" +
				"- `/help`：逐条列出全部飞书指令。\n" +
				"- `/help model`：查看模型设置用法。\n" +
				"- `/help /notify`：指令名也可以带 `/`。\n\n" +
				"操作者和通知接收者均可查看；一次查询一条指令。"},
		{"new", "选择目录并创建 Codex 任务（本部署" + creationState + "）。",
			"用法：`/new [任务描述]`，例如 `/new 检查项目中的测试失败原因`。\n\n" +
				"仅操作者可用。私聊机器人、在授权入口群或已绑定任务群/话题中发送，随后在卡片中填写任务、选择目录和跟进位置，点击创建。\n\n" +
				"支持附件的部署可随命令携带图片或文件。已绑定会话中，普通消息继续原会话，`/new` 创建另一项任务。\n\n" +
				"本部署的飞书创建功能：" + creationState + "。"},
		{"model", "打开卡片，查看和修改模型、思考强度与 Fast。",
			"用法：`/model`，不需要参数。\n\n" +
				"- 卡片默认显示当前会话设置。\n" +
				"- 选择模型、思考强度及 Fast，点击「保存」一起提交。\n" +
				"- 点击「取消」丢弃本次修改。\n" +
				"- 保存成功或取消后，原卡片收起表单，显示当前设置；再次修改时重新发送 `/model`。\n\n" +
				"仅操作者可在对应任务群或话题中使用；卡片只接受发起人的操作。修改用于后续轮次，正在执行的轮次保持原设置。模型和强度以可用选项为准；模型未提供 Fast 时无法开启。"},
		{"stop", "请求停止当前执行轮次。",
			"用法：`/stop`，不接受参数。\n\n" +
				"仅操作者可在对应的已绑定任务群或话题中使用。提交后以任务状态更新为准；有未核验的操作时，请等待核验完成。"},
		{"continue", "请求继续执行当前会话的待处理队列。",
			"用法：`/continue`，不接受参数。\n\n" +
				"仅操作者可在对应的已绑定任务群或话题中使用。用于停止后继续处理已有队列；没有队列时不会启动新任务。"},
		{"mute-dir", "管理禁止自动建群的目录规则。",
			"用法：`/mute-dir 绝对目录 [--recursive]` 或 `/mute-dir list`。\n\n" +
				"- `/mute-dir /workspace/project`：添加精确目录规则。\n" +
				"- `/mute-dir /workspace/project --recursive`：同时覆盖子目录。\n" +
				"- `/mute-dir list`：列出本部署的全部目录规则。\n\n" +
				"仅操作者可用，目录须在远程主机上存在且位于授权范围。路径含空格时加引号，例如 `/mute-dir \"/workspace/my project\"`。规则只影响未来自动建群，已绑定会话继续同步。"},
		{"unmute-dir", "移除指定目录的自动建群静默规则。",
			"用法：`/unmute-dir 绝对目录 [--recursive]`。\n\n" +
				"- `/unmute-dir /workspace/project`：删除该路径的精确规则。\n" +
				"- `/unmute-dir /workspace/project --recursive`：删除该路径的递归规则。\n\n" +
				"仅操作者可用，目录须存在且位于授权范围；路径含空格时加引号。两类规则分别删除，其他祖先规则仍可能生效，可用 `/mute-dir list` 查看。解除后不会为静默期间的历史会话补建群。"},
		{"notify", "管理你自己的 @ 提醒。",
			"用法：`/notify at 子命令`。\n\n" +
				"- `/notify at status`：查看规则、当前是否提醒及适用时区。\n" +
				"- `/notify at off`：长期关闭 @ 提醒。\n" +
				"- `/notify at on`：开启并清除当前暂停；保留每日免打扰。\n" +
				"- `/notify at snooze 2h`：暂停 2 小时；也可用 `30m`、`1h30m`。\n" +
				"- `/notify at until YYYY-MM-DD HH:mm`：暂停到指定未来时间。\n" +
				"- `/notify at quiet 23:00-08:00`：设置每日免打扰，可跨午夜。\n" +
				"- `/notify at quiet off`：清除每日免打扰。\n\n" +
				"仅通知接收者可修改自己的设置，跨本部署全部任务群生效；时间按个人通知时区解释。暂停到期或清除免打扰不会解除长期关闭，正文继续同步。"},
		{"preview", "私聊获取文件网页预览口令（本部署" + previewState + "）。",
			"用法：`/preview`，不接受参数。\n\n" +
				"仅部署所有者可私聊机器人获取口令；打开文件预览链接后，在浏览器登录页输入。\n\n" +
				"本部署的文件网页预览：" + previewState + "。"},
		{"pair", "在安装配对流程中确认本人身份。",
			"用法：`/pair 配对口令`。\n\n" +
				"安装流程发起配对后，由本人私聊机器人，原样发送该流程提供的命令。口令在有效期内仅可使用一次；已配置所有者时仅接受原所有者。"},
		{"verify", "在连接验证流程中确认私聊收消息能力。",
			"用法：`/verify 验证令牌`。\n\n" +
				"部署所有者收到连接验证卡后，点击卡片按钮，再在 5 分钟有效期内私聊机器人发送卡片中的完整命令。此操作记录连接验证结果。"},
		{"verify-group", "在连接验证流程中确认测试群收消息能力。",
			"用法：`/verify-group 请求ID`。\n\n" +
				"部署所有者在验证流程创建的原测试群中 @机器人，发送该流程提供的请求ID。此操作记录该测试群的收消息结果。"},
	}
	if topic != "" {
		name := strings.TrimPrefix(topic, "/")
		for _, command := range commands {
			if command.name == name {
				return Message("指令帮助", "/"+command.name, "**/"+command.name+"**\n\n"+command.details+"\n\n群聊中通常需要 @机器人；已绑定任务群只有你和机器人时，操作者可免 @。未响应时请 @机器人。")
			}
		}
		return Message("指令帮助", "本部署", "未找到该指令的帮助。发送 `/help` 查看全部指令，例如 `/help model` 或 `/help /notify`。")
	}
	var body strings.Builder
	body.WriteString("**飞书指令**\n\n")
	for _, command := range commands {
		body.WriteString("- `/" + command.name + "`：" + command.summary + "\n")
	}
	if !creationEnabled {
		body.WriteString("\n这里暂不创建新任务，请从 Codex App 发起任务。\n")
	}
	body.WriteString("\n发送 `/help 指令名` 查看具体语法、示例和适用范围，例如 `/help model`；也支持 `/help /model`。\n\n群聊中通常需要 @机器人；已绑定任务群只有你和机器人时，操作者可免 @。未响应时请 @机器人。会话设置和任务控制请进入对应任务群或话题。")
	return Message("指令帮助", "本部署", body.String())
}
