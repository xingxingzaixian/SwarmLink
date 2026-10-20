package wails

import (
	"fmt"
	"time"

	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/version"
)

// EventUpdateState 是局域网更新的状态事件。
//
// 只有这一个事件，载荷是【完整快照】（UpdateStateDTO）。理由：更新是一个
// 8 阶段的流程，用「进度」「就绪」「排期」多个增量事件拼装，前端迟早会
// 出现「显示已下载完、其实后端已经进入安装」这种自相矛盾的画面。
// 快照每帧只有十几K的序列化成本，而更新事件一天也发不了几次（下载中
// 每 256 KiB 一次），不值得为它引入增量协议。
const EventUpdateState = "update:state"

// UpdateService 把更新应用暴露给前端。
//
// 它同时是「状态事件」的唯一出口：app.UpdateApp 只知道回调，
// 由本服务把回调翻译成前端事件 —— 这样 app 层不必认识 Wails，
// 也不需要在事件总线里新增一个只有自己用的主题。
type UpdateService struct {
	d    Deps
	emit func(name string, data any)
}

// SetEmitter 由外壳在 Wails 应用创建之后注入（创建之前拿不到 Event API）。
func (s *UpdateService) SetEmitter(fn func(name string, data any)) { s.emit = fn }

// Publish 把一份状态快照推给前端（app 层的 OnChange 回调指向这里）。
func (s *UpdateService) Publish(st app.UpdateState) {
	if s.emit == nil {
		return
	}
	s.emit(EventUpdateState, s.toDTO(st))
}

// GetState 返回当前更新状态（设置页打开时拉一次）。
func (s *UpdateService) GetState() UpdateStateDTO {
	if s.d.Update == nil {
		return UpdateStateDTO{
			Phase:          string(app.UpdateUnsupported),
			CurrentVersion: version.Version,
			DataDir:        s.d.ConfigDir,
			LogPath:        s.d.LogPath,
		}
	}
	return s.toDTO(s.d.Update.State())
}

// CheckNow 触发一次后台检查（不等结果，结果通过事件回来）。
func (s *UpdateService) CheckNow() UpdateStateDTO {
	if s.d.Update == nil {
		return s.GetState()
	}
	s.d.Update.CheckAsync()
	return s.GetState()
}

// ApplyNow 立即安装并重启。返回后进程通常会在几百毫秒内退出。
func (s *UpdateService) ApplyNow() (UpdateStateDTO, error) {
	if s.d.Update == nil {
		return s.GetState(), fmt.Errorf("更新服务不可用")
	}
	if err := s.d.Update.ApplyNow(); err != nil {
		return s.toDTO(s.d.Update.State()), err
	}
	return s.toDTO(s.d.Update.State()), nil
}

// Schedule 把安装安排到 atMillis（毫秒时间戳）。
//
// 时间从界面传过来是本地墙钟的毫秒值：跨时区/手动改系统时间都会让
// 这个数变得不可预期，但它的语义本来就是「用户当时看到的那个时刻」，
// 因此不做额外换算（改成 UTC 反而会让「今晚 22:00」在改时区后错位）。
func (s *UpdateService) Schedule(atMillis int64) (UpdateStateDTO, error) {
	if s.d.Update == nil {
		return s.GetState(), fmt.Errorf("更新服务不可用")
	}
	if atMillis <= 0 {
		return s.toDTO(s.d.Update.State()), fmt.Errorf("请选择安装时间")
	}
	if err := s.d.Update.Schedule(time.UnixMilli(atMillis)); err != nil {
		return s.toDTO(s.d.Update.State()), err
	}
	return s.toDTO(s.d.Update.State()), nil
}

// Snooze 稍后提醒（30 分钟）。
func (s *UpdateService) Snooze() UpdateStateDTO {
	if s.d.Update == nil {
		return s.GetState()
	}
	return s.toDTO(s.d.Update.Snooze())
}

// Cancel 放弃本次更新（删除已下载的包，回到 idle）。
func (s *UpdateService) Cancel() (UpdateStateDTO, error) {
	if s.d.Update == nil {
		return s.GetState(), fmt.Errorf("更新服务不可用")
	}
	if err := s.d.Update.Cancel(); err != nil {
		return s.toDTO(s.d.Update.State()), err
	}
	return s.toDTO(s.d.Update.State()), nil
}

func (s *UpdateService) toDTO(st app.UpdateState) UpdateStateDTO {
	out := UpdateStateDTO{
		Phase:             string(st.Phase),
		CurrentVersion:    st.CurrentVersion,
		TargetVersion:     st.TargetVersion,
		SourcePeerID:      st.SourcePeerID,
		SourceName:        st.SourceName,
		SourceAddr:        st.SourceAddr,
		Size:              st.Size,
		Downloaded:        st.Downloaded,
		Percent:           st.Percent,
		SHA256:            st.SHA256,
		ScheduledAt:       st.ScheduledAt,
		AutoInstallAt:     st.AutoInstallAt,
		PromptPending:     st.PromptPending,
		Error:             st.Error,
		PlatformSupported: st.PlatformSupported,
		CanDistribute:     st.CanDistribute,
		InstallerPath:     st.InstallerPath,
		LastCheckAt:       st.LastCheckAt,
		DataDir:           s.d.ConfigDir,
		LogPath:           s.d.LogPath,
	}
	if len(st.HigherPeers) > 0 {
		out.HigherPeers = make([]UpdatePeerDTO, 0, len(st.HigherPeers))
		for _, p := range st.HigherPeers {
			out.HigherPeers = append(out.HigherPeers, UpdatePeerDTO{
				PeerID:      p.PeerID,
				DisplayName: p.DisplayName,
				Addr:        p.Addr,
				Version:     p.Version,
			})
		}
	}
	return out
}
