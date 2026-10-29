package domain

// PurgeBatch 只供存储在永久注销屏障下使用；中间状态不再是可读取的 Book，不能 refold 复活已删物化。
// 每项对应一个账本实体或业务日，原文由存储另行限量删除；不序列化或复制整个账本。
func (u *Book) PurgeBatch(limit int) int {
	if limit <= 0 {
		return 0
	}
	left := limit
	purgeSlice(&u.changesets, &left)
	purgeSlice(&u.exchanges, &left)
	purgeSlice(&u.commitments, &left)
	purgeSlice(&u.projects, &left)
	purgeSlice(&u.days, &left)
	purgeSlice(&u.weeks, &left)
	purgeSlice(&u.merges, &left)
	purgeSlice(&u.deletionTasks, &left)
	purgeSlice(&u.domainChanges, &left)
	purgeMap(u.acceptedFacts, &left)
	purgeMap(u.captureDays, &left)
	purgeMap(u.captureStreams, &left)
	purgeMap(u.weekSessions, &left)
	purgeMap(u.submitted, &left)
	purgeMap(u.workBindings, &left)
	purgeMap(u.workSessions, &left)
	if left > 0 {
		*u = *NewBook()
	}
	return limit - left
}

func purgeSlice[T any](items *[]T, left *int) {
	n := min(len(*items), *left)
	clear((*items)[:n])
	*items = (*items)[n:]
	*left -= n
}

func purgeMap[K comparable, V any](items map[K]V, left *int) {
	for key := range items {
		if *left == 0 {
			break
		}
		delete(items, key)
		*left -= 1
	}
}
