package app

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"codex-feishu/internal/cards"
)

// NativeHandshakeIncompatible identifies a confirmed transport or initialize
// mismatch after daemon discovery. Temporary connection errors are separate.
type NativeHandshakeIncompatible struct{ Version string }

func (*NativeHandshakeIncompatible) Error() string { return "incompatible Codex handshake" }

var codexVersionPattern = regexp.MustCompile(`^[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}([-+][a-zA-Z0-9.-]{1,40})?$`)

func compatibilityFeature(method string) string {
	switch method {
	case "initialize":
		return "共享连接"
	case "thread/resume", "thread/read", "thread/turns/list", "thread/items/list":
		return "会话同步"
	case "thread/list":
		return "会话发现"
	case "threadSection/list":
		return "置顶状态同步"
	case "thread/queue/list":
		return "任务队列读取"
	case "thread/queue/add":
		return "飞书消息提交"
	case "thread/queue/delete":
		return "移除排队消息"
	case "thread/queue/start":
		return "继续执行队列"
	case "turn/steer":
		return "追加引导"
	case "turn/interrupt":
		return "停止任务"
	case "model/list", "thread/settings/update":
		return "会话模型设置"
	case "project/list":
		return "创建时选择 Codex 项目"
	case "thread/loaded/list":
		return "新建任务结果核验"
	case "thread/start":
		return "飞书新建任务"
	case "turn/start":
		return "失败轮次重试"
	}
	return ""
}

func nativeVersion(native RuntimeNative) (string, bool) {
	versioned, ok := native.(interface{ AppServerVersion() string })
	if !ok {
		return "", false
	}
	return versioned.AppServerVersion(), true
}

type compatibilityNoticeKey struct{ version, feature string }

var legacyVersionNoticeFeatures = []string{"飞书新建任务", "失败轮次重试", "托管工作树恢复"}

// In-flight notices from earlier builds used this body for both version
// advisories and observed failures. These three features require fresh evidence.
func legacyVersionNotice(card string) bool {
	for _, feature := range legacyVersionNoticeFeatures {
		if strings.Contains(card, "「"+feature+"」所需协议尚未通过检查") {
			return true
		}
	}
	return false
}

func (s *Service) stageCompatibilityNotice(ctx context.Context, version, feature string) {
	if feature == "" || ctx.Err() != nil {
		return
	}
	if !codexVersionPattern.MatchString(version) {
		version = "未知版本"
	}
	key := compatibilityNoticeKey{version, feature}
	s.compatibilityMu.Lock()
	defer s.compatibilityMu.Unlock()
	if s.compatibilityNotices[key] {
		return
	}
	if s.compatibilityNotices == nil {
		s.compatibilityNotices = make(map[compatibilityNoticeKey]bool)
	}
	card := cards.Message("Codex 兼容性提醒", "功能暂不可用", "当前 Codex App Server 为 "+version+"。\n\n飞书桥已检测到「"+feature+"」的协议不兼容。你可以先在 Codex App 里继续操作。")
	identity := feature
	if slices.Contains(legacyVersionNoticeFeatures, feature) {
		// Earlier builds also used these keys for unverified versions. An
		// advisory must not suppress a later, observed incompatibility.
		identity = "observed:" + feature
	}
	if err := s.Store.StageCompatibilityNotice(ctx, s.Config.Host.ID, s.Config.Feishu.AppID, s.Config.Access.OwnerOpenID, version, identity, card, s.now().UnixMilli()); err != nil {
		s.compatibilityNotices[key] = false
		s.notice("codex.compatibility_notice_pending")
		return
	}
	s.compatibilityNotices[key] = true
}

func (s *Service) retryCompatibilityNotices(ctx context.Context) {
	s.compatibilityMu.Lock()
	var pending []compatibilityNoticeKey
	for key, staged := range s.compatibilityNotices {
		if !staged {
			pending = append(pending, key)
		}
	}
	s.compatibilityMu.Unlock()
	for _, key := range pending {
		s.stageCompatibilityNotice(ctx, key.version, key.feature)
	}
}

func (s *Service) observeCompatibility(ctx context.Context, native ServiceNative) {
	version, _ := nativeVersion(native)
	if observer, ok := native.(interface{ SetCompatibilityObserver(func(string)) }); ok {
		observer.SetCompatibilityObserver(func(method string) {
			s.stageCompatibilityNotice(ctx, version, compatibilityFeature(method))
		})
	}
}
