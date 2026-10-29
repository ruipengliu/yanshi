package store

import "persona/domain"

// DomainChange 把存储隔离键与无正文的领域变化一起交给消费者。
// 去重键是 (Owner, Fact.ID, Fact.Version)，不能跨 owner 复用确认位置。
type DomainChange struct {
	Owner
	domain.DomainChange
}
