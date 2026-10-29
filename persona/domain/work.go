package domain

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrWorkTargetGone = errors.New("工作 Session 目标已失效")
	ErrWorkConflict   = errors.New("工作 Session 绑定已变化")
	ErrWorkRetired    = errors.New("这个 Session 已退役，不能读写")
	ErrWorkCommand    = errors.New("工作 Session 未登记此命令")
	ErrWorkBusy       = errors.New("工作 Session 的前一命令尚未结束")
)

// WorkLease 由编排任务提供；期限也参与校验，旧执行者即使尚未看到接管也不能迟到绑定。
type WorkLease struct {
	Task       string
	Sequence   int64
	Generation int64
	Until      time.Time
}

type WorkTarget struct {
	Session  string
	DeleteAt time.Time
	Version  uint64
	Revision uint64
}

type workBinding struct {
	Version  uint64
	Lease    WorkLease
	DeleteAt time.Time
}

type workSession struct {
	Target    string
	Retired   bool
	Command   string
	CommandID string
	Finished  bool
}

// WorkTarget 仍以领域对象的当前绑定为准，擦除停用与 CAS 不会产生两份当前值。
func (u *Book) WorkTarget(target string) (WorkTarget, error) {
	binding := u.workBindings[target]
	out := WorkTarget{Version: binding.Version, DeleteAt: binding.DeleteAt}
	kind, key, ok := strings.Cut(target, "/")
	if !ok || key == "" {
		return out, ErrWorkConflict
	}
	switch kind {
	case "day":
		if err := u.CheckCaptures(key); err != nil {
			return out, err
		}
		day := u.BusinessDay(key)
		if day.Deleted {
			return out, ErrDayDeleted
		}
		if day.ExpiresAt.IsZero() {
			return out, ErrWorkConflict
		}
		for _, d := range u.deletionTasks {
			if d.Date == key && d.Status == "pending" {
				return out, ErrWorkBusy
			}
		}
		out.Session, out.DeleteAt, out.Revision = day.Session, day.ExpiresAt, day.CaptureRevision
	case "project":
		p := u.project(key)
		if p == nil || !p.Active() {
			return out, ErrWorkTargetGone
		}
		out.Session = p.Session
	case "week":
		if _, err := time.Parse(time.DateOnly, key); err != nil {
			return out, ErrWorkConflict
		}
		out.Session = u.weekSessions[key]
	default:
		return out, ErrWorkConflict
	}
	return out, nil
}

func validWorkLease(lease WorkLease, now time.Time) bool {
	return lease.Task != "" && lease.Sequence > 0 && lease.Generation > 0 && lease.Until.After(now)
}

func (u *Book) ReserveWork(target string, lease WorkLease, now time.Time) (WorkTarget, error) {
	out, err := u.WorkTarget(target)
	if err != nil {
		return out, err
	}
	old := u.workBindings[target]
	if !validWorkLease(lease, now) || old.Lease.Sequence > lease.Sequence ||
		(old.Lease.Sequence == lease.Sequence && (old.Lease.Task != lease.Task || old.Lease.Generation > lease.Generation)) {
		return out, ErrWorkConflict
	}
	if u.workBindings == nil {
		u.workBindings = map[string]workBinding{}
	}
	old.Lease, old.DeleteAt = lease, out.DeleteAt
	u.workBindings[target] = old
	return out, nil
}

func (u *Book) checkWorkLease(target string, lease WorkLease, now time.Time) error {
	if !validWorkLease(lease, now) || !sameWorkLease(u.workBindings[target].Lease, lease) {
		return ErrWorkConflict
	}
	return nil
}

// BindWork 是生产绑定的唯一写入口。期望值、业务世代与任务租约在同一 owner 事务内比较。
func (u *Book) BindWork(target string, expected WorkTarget, lease WorkLease, session string, now time.Time) error {
	if err := u.checkWorkLease(target, lease, now); err != nil {
		return err
	}
	current, err := u.WorkTarget(target)
	if err != nil {
		return err
	}
	if current.Session != expected.Session || current.Version != expected.Version || current.Revision != expected.Revision || !current.DeleteAt.Equal(expected.DeleteAt) || session == "" || u.SessionKnown(session) {
		return ErrWorkConflict
	}
	if err := u.setWorkSession(target, session, current.DeleteAt); err != nil {
		return err
	}
	if u.workSessions == nil {
		u.workSessions = map[string]workSession{}
	}
	if current.Session != "" {
		old := u.workSessions[current.Session]
		old.Target, old.Retired = target, true
		u.workSessions[current.Session] = old
	}
	u.workSessions[session] = workSession{Target: target}
	binding := u.workBindings[target]
	binding.Version++
	u.workBindings[target] = binding
	return nil
}

func (u *Book) setWorkSession(target, session string, deadline time.Time) error {
	kind, key, _ := strings.Cut(target, "/")
	switch kind {
	case "day":
		if session == "" {
			u.captureDay(key).Session = ""
			u.captureDay(key).ReadThrough = Ref{}
			return nil
		}
		return u.BindDay(key, session, deadline)
	case "project":
		if p := u.project(key); p != nil {
			p.Session = session
			return nil
		}
	case "week":
		u.weekSessions[key] = session
		return nil
	}
	return ErrWorkConflict
}

// RetireWork 先封住 Persona 的读写，再允许应用释放槽位。重复恢复不能停用后来换绑的 Session。
func (u *Book) RetireWork(target, session string, now time.Time) error {
	if session == "" {
		return ErrWorkConflict
	}
	old := u.workSessions[session]
	if old.Retired {
		return nil
	}
	current, err := u.WorkTarget(target)
	if err != nil {
		// 擦除、到期已停用的工作 Session 仍须留下永久拒收标记。
		if old.Target != target {
			return err
		}
	} else if current.Session == session {
		if err := u.setWorkSession(target, "", current.DeleteAt); err != nil {
			return err
		}
		if u.workBindings == nil {
			u.workBindings = map[string]workBinding{}
		}
		binding := u.workBindings[target]
		binding.Version++
		binding.DeleteAt = current.DeleteAt
		u.workBindings[target] = binding
	} else if old.Target != target {
		return ErrWorkConflict
	}
	if u.workSessions == nil {
		u.workSessions = map[string]workSession{}
	}
	old.Target, old.Retired = target, true
	u.workSessions[session] = old
	return nil
}

func (u *Book) SessionKnown(session string) bool {
	if _, ok := u.workSessions[session]; ok {
		return true
	}
	if _, d := u.sessionDay(session); d != nil {
		return true
	}
	if u.projectOfSession(session) != nil {
		return true
	}
	for _, sid := range u.weekSessions {
		if sid == session {
			return true
		}
	}
	return false
}

// FenceOrphan 与绑定互斥；清理者获得资格后，晚到的创建者也无法再绑定这个 ID。
func (u *Book) FenceOrphan(session string) bool {
	if old, ok := u.workSessions[session]; ok {
		return old.Retired && old.Target == ""
	}
	if u.SessionKnown(session) {
		return false
	}
	if u.workSessions == nil {
		u.workSessions = map[string]workSession{}
	}
	u.workSessions[session] = workSession{Retired: true}
	return true
}

func (u *Book) CheckWorkSession(session string) error {
	old, managed := u.workSessions[session]
	if managed && old.Retired {
		return ErrWorkRetired
	}
	// 擦除已清空当前绑定；删除待办的 busy 状态不能遮住旧 Session 永久失去资格的原因。
	if err := u.checkWorkingSession(session); err != nil {
		return err
	}
	if managed {
		current, err := u.WorkTarget(old.Target)
		if err != nil {
			return err
		}
		if current.Session != session {
			return ErrWorkRetired
		}
	}
	return nil
}

func (u *Book) RegisterWorkCommand(target, session, id, command string, lease WorkLease, now time.Time) error {
	if err := u.checkWorkLease(target, lease, now); err != nil {
		return err
	}
	if err := u.CheckWorkSession(session); err != nil {
		return err
	}
	current, err := u.WorkTarget(target)
	if err != nil {
		return err
	}
	if current.Session != session || id == "" || !workCommandFor(target, command) {
		return ErrWorkCommand
	}
	old := u.workSessions[session]
	if old.CommandID != "" && old.CommandID != id && !old.Finished {
		return ErrWorkBusy
	}
	if old.CommandID == id {
		if old.Command != command {
			return ErrWorkCommand
		}
		return nil
	}
	if u.workSessions == nil {
		u.workSessions = map[string]workSession{}
	}
	old.Target, old.CommandID, old.Command, old.Finished = target, id, command, false
	u.workSessions[session] = old
	return nil
}

func workCommandFor(target, command string) bool {
	kind, _, _ := strings.Cut(target, "/")
	switch kind {
	case "day":
		return command == "exchange" || command == "day"
	case "project":
		return command == "collect" || command == "rewrite"
	case "week":
		return command == "week"
	}
	return false
}

func (u *Book) FinishWorkCommand(session, id string) error {
	old, ok := u.workSessions[session]
	if !ok || old.CommandID != id {
		return ErrWorkCommand
	}
	old.Finished = true
	u.workSessions[session] = old
	return nil
}

// CheckWorkTool 也供 MCP 在事务内守住所有只读工具；夹具的宽松路径必须显式开启。
func (c *Call) CheckWorkTool(write bool, commands ...string) (err error) {
	defer func() { c.workErr = err }()
	if c.u == nil {
		if c.RequireWork {
			return ErrWorkCommand
		}
		return nil
	}
	if err := c.u.CheckWorkSession(c.session); err != nil {
		return err
	}
	old, managed := c.u.workSessions[c.session]
	if !managed {
		if write && c.RequireWork {
			return ErrWorkCommand
		}
		return nil
	}
	if !write {
		return nil
	}
	if old.CommandID == "" || old.Finished {
		return ErrWorkCommand
	}
	for _, command := range commands {
		if old.Command == command {
			return nil
		}
	}
	return ErrWorkCommand
}

func sameWorkLease(a, b WorkLease) bool {
	return a.Task == b.Task && a.Sequence == b.Sequence && a.Generation == b.Generation && a.Until.Equal(b.Until)
}

// DeletionSessionIDs 把异常退役的历史工作 Session 纳入擦除；旧版本历史已由此前待办清理。
func (u *Book) DeletionSessionIDs(task DeletionTask) []string {
	var out []string
	if task.Session != "" {
		out = append(out, task.Session)
	}
	for _, sid := range task.Sessions {
		if sid != task.Session && u.workSessions[sid].Target == "day/"+task.Date {
			out = append(out, sid)
		}
	}
	return out
}

// 项目归集的取代身份跨退役保持稳定，未知结果重做不能叠加两份同日修订。
func (u *Book) sameProjectSession(a, b string) bool {
	if a == b {
		return true
	}
	target := func(sid string) string {
		if binding := u.workSessions[sid]; strings.HasPrefix(binding.Target, "project/") {
			return binding.Target
		}
		if project := u.projectOfSession(sid); project != nil {
			return "project/" + project.ID
		}
		return ""
	}
	x, y := target(a), target(b)
	return x != "" && x == y
}
