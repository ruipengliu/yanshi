package domain

import (
	"encoding/json"
	"slices"
)

// FoldVersion 是叠加规则与物化行形状的版本（Persona ADR-0002）。存储每次提交都把它与物化的交谈、承诺一起写入；
// 按需读取只信任版本相同的物化行，不同时退回整体装载并等待重建。改动 fold、State 的交谈与承诺，
// 或 Exchange、Commitment 的 JSON 形状时递增（foldversion_test 的摘要随之更新）。
const FoldVersion = 1

// 按需读取（Persona ADR-0002 第一期）：只读事务先装载小表（项目、业务日、绑定等），再由读取计划按条件装载物化的
// 交谈、承诺、变更集与日总览。领域代码照常在 Book 上运行，前提是计划装载了它会看的全部行；
// 计划与整体装载的结果必须逐项相同，由 storetest 的差分测试裁判。

// CommitmentRows 选择物化的承诺；各条件之间是"或"。
type CommitmentRows struct {
	IDs      []string
	Sessions []string
	// Unmerged 选择未被合并、状态属于 Statuses 的承诺；Statuses 为空时不限状态。
	Unmerged bool
	Statuses []string
}

// Match 是选择条件的定义；存储可以先用索引粗选，再用它精确过滤。
func (f CommitmentRows) Match(m *Commitment) bool {
	return slices.Contains(f.IDs, m.ID) || slices.Contains(f.Sessions, m.Session) ||
		f.Unmerged && m.MergedInto == "" && (len(f.Statuses) == 0 || slices.Contains(f.Statuses, m.Status))
}

// ExchangeRows 选择物化的交谈；各条件之间是"或"。
type ExchangeRows struct {
	Sessions []string
	Dates    []string
}

func (f ExchangeRows) Match(e *Exchange) bool {
	return slices.Contains(f.Sessions, e.Session) || slices.Contains(f.Dates, e.Date)
}

// ChangesetRows 选择变更集；各条件之间是"或"。
type ChangesetRows struct {
	IDs      []string
	Sessions []string
}

func (f ChangesetRows) Match(cs *Changeset) bool {
	return slices.Contains(f.IDs, cs.ID) || slices.Contains(f.Sessions, cs.Session)
}

// DayRows 选择日总览。
type DayRows struct {
	Dates []string
}

func (f DayRows) Match(d *Day) bool { return slices.Contains(f.Dates, d.Date) }

// LedgerRows 按条件读取同一快照中的行，结果保持账本顺序。
type LedgerRows interface {
	Commitments(CommitmentRows) ([]Commitment, error)
	Exchanges(ExchangeRows) ([]Exchange, error)
	Changesets(ChangesetRows) ([]Changeset, error)
	Days(DayRows) ([]Day, error)
}

// ReadPlan 在只含小表的 Book 上装载一次只读查询需要的行。返回 false 表示这次无法限定范围，存储改为整体装载。
type ReadPlan func(u *Book, rows LedgerRows) (bool, error)

// MarkPartial 把只装载了小表的 Book 标为按需读取：交谈、承诺、变更集、日总览、周回顾、合并与领域变化都为空，
// 等待计划装载；它不能叠加或提交。
func (u *Book) MarkPartial() {
	u.partial = true
	u.changesets, u.days, u.weeks, u.merges, u.domainChanges = nil, nil, nil, nil, nil
	u.ledger = ledger{sessionDays: u.sessionDays()}
}

// Partial 报告 Book 是否只装载了部分账本。
func (u *Book) Partial() bool { return u.partial }

// PartialCopy 从整体装载的 Book 得到按需读取的 Book 与按条件读取它的 LedgerRows，供内存实现与测试使用。
// 两者共享小表；调用方应传入快照。
func (u *Book) PartialCopy() (*Book, LedgerRows) {
	p := *u
	p.MarkPartial()
	return &p, memoryRows{u}
}

type memoryRows struct{ u *Book }

func pick[T any](items []T, match func(*T) bool) []T {
	var out []T
	for i := range items {
		if match(&items[i]) {
			out = append(out, items[i])
		}
	}
	return out
}

// 返回副本，避免按需读取的 Book 与源快照共享嵌套的切片。
func copyRows[T any](items []T) ([]T, error) {
	data, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	var out []T
	err = json.Unmarshal(data, &out)
	return out, err
}

func (r memoryRows) Commitments(f CommitmentRows) ([]Commitment, error) {
	return copyRows(pick(r.u.commitments, f.Match))
}
func (r memoryRows) Exchanges(f ExchangeRows) ([]Exchange, error) {
	return copyRows(pick(r.u.exchanges, f.Match))
}
func (r memoryRows) Changesets(f ChangesetRows) ([]Changeset, error) {
	return copyRows(pick(r.u.changesets, f.Match))
}
func (r memoryRows) Days(f DayRows) ([]Day, error) { return copyRows(pick(r.u.days, f.Match)) }

// ToolReadPlan 返回只读工具的读取计划；没有计划的工具整体装载。
func ToolReadPlan(tool, session string, args json.RawMessage) ReadPlan {
	switch tool {
	case "list_commitments":
		var in CommitmentFilter
		if json.Unmarshal(args, &in) != nil {
			return nil
		}
		return commitmentsPlan(in.Status)
	case "find_related":
		// 候选项目在小表中；未完成承诺的数量与召回只看未完成、未合并的承诺。
		return commitmentsPlan("open")
	case "day_ledger":
		return dayLedgerPlan(session)
	}
	return nil
}

// QueryReadPlan 返回应用查询的读取计划；没有计划的资源整体装载。
func QueryReadPlan(resource, id, date string, filter CommitmentFilter) ReadPlan {
	switch resource {
	case "commitments":
		return commitmentsPlan(filter.Status)
	case "history":
		return historyPlan(id)
	case "day":
		return func(u *Book, rows LedgerRows) (bool, error) {
			var err error
			if u.exchanges, err = rows.Exchanges(ExchangeRows{Dates: []string{date}}); err != nil {
				return false, err
			}
			u.days, err = rows.Days(DayRows{Dates: []string{date}})
			return err == nil, err
		}
	}
	return nil
}

// commitmentsPlan 装载 QueryCommitments 会列出的状态：被合并的从不列出，默认只列未完成的。
func commitmentsPlan(status string) ReadPlan {
	var statuses []string
	switch status {
	case "":
		statuses = []string{"open"}
	case "all":
	default:
		statuses = []string{status}
	}
	return func(u *Book, rows LedgerRows) (bool, error) {
		var err error
		u.commitments, err = rows.Commitments(CommitmentRows{Unmerged: true, Statuses: statuses})
		return err == nil, err
	}
}

// historyPlan 装载一条承诺与它的修订所在的变更集（创建项的初值取自其中）。
func historyPlan(id string) ReadPlan {
	return func(u *Book, rows LedgerRows) (bool, error) {
		var err error
		if u.commitments, err = rows.Commitments(CommitmentRows{IDs: []string{id}}); err != nil {
			return false, err
		}
		var ids []string
		for _, m := range u.commitments {
			for _, r := range m.Revisions {
				ids = append(ids, r.Changeset)
			}
		}
		if len(ids) > 0 {
			u.changesets, err = rows.Changesets(ChangesetRows{IDs: ids})
		}
		return err == nil, err
	}
}

// dayLedgerPlan 装载与 session 同一天的变更集与交谈、它们涉及或在当天建立的承诺，以及全部未完成的承诺
// （"之前未完成的相关承诺"与重复提示只看未完成的）。同一天按 sameDay 判断：绑定了业务日的是该日的全部 Session，
// 未绑定的只有它自己。
func dayLedgerPlan(session string) ReadPlan {
	return func(u *Book, rows LedgerRows) (bool, error) {
		if session == "" {
			return false, nil
		}
		sessions := []string{session}
		if day := u.ledger.sessionDays[session]; day != "" {
			sessions = nil
			for s, d := range u.ledger.sessionDays {
				if d == day {
					sessions = append(sessions, s)
				}
			}
			slices.Sort(sessions)
		}
		var err error
		if u.changesets, err = rows.Changesets(ChangesetRows{Sessions: sessions}); err != nil {
			return false, err
		}
		var ids []string
		for i := range u.changesets {
			ids = append(ids, u.changesets[i].commitmentIDs()...)
		}
		if u.commitments, err = rows.Commitments(CommitmentRows{IDs: ids, Sessions: sessions, Unmerged: true, Statuses: []string{"open"}}); err != nil {
			return false, err
		}
		u.exchanges, err = rows.Exchanges(ExchangeRows{Sessions: sessions})
		return err == nil, err
	}
}

// commitmentIDs 是变更集提到的全部承诺 ID（多出的承诺由读取方按原条件再过滤，不影响结果）。
func (cs *Changeset) commitmentIDs() []string {
	var ids []string
	for _, m := range cs.CreateCommitments {
		ids = append(ids, m.ID)
	}
	for _, c := range slices.Concat(cs.ReviseCommitments, cs.CloseCommitments) {
		ids = append(ids, c.ID)
	}
	for _, m := range cs.MergeCommitments {
		ids = append(ids, m.Into)
		ids = append(ids, m.From...)
	}
	return ids
}
