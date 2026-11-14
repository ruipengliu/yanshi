package domain

type captureRefKey struct {
	date string
	ref  Ref
}

type captureRefValue struct {
	segment CaptureSegment
	found   bool
}

// captureRef 与引用存在性检查共用缓存；同一调用内重复引用不重复读原文，读失败必须交回事务回滚。
func (c *Call) captureRef(date, raw string) (CaptureSegment, bool) {
	if c.err != nil || c.u.captureDays[date] == nil {
		return CaptureSegment{}, false
	}
	ref, ok := parseRef(raw)
	if !ok {
		return CaptureSegment{}, false
	}
	key := captureRefKey{date: date, ref: ref}
	access, err := c.u.captureAccess()
	if err != nil {
		c.err = err
		return CaptureSegment{}, false
	}
	if cached, ok := c.captureRefs[key]; ok {
		// 缓存只能省去重复读原文，不能延长原文期限或事务寿命。
		if err := (captureGuard{CaptureAccess: access, book: c.u}).check(date); err != nil {
			c.err = err
			return CaptureSegment{}, false
		}
		return cached.segment.Clone(), cached.found
	}
	g, found, err := access.Lookup(date, ref)
	if err != nil {
		c.err = err
		return CaptureSegment{}, false
	}
	if c.captureRefs == nil {
		c.captureRefs = make(map[captureRefKey]captureRefValue)
	}
	c.captureRefs[key] = captureRefValue{segment: g.Clone(), found: found}
	return g, found
}

func (g CaptureSegment) commitmentEvidence() bool {
	return g.Visible() && (g.Speaker == "other" || g.Speaker == "wearer" && g.EffectiveSpeakerGrade() == "evidence")
}

func (g CaptureSegment) criticalLowSupport() bool {
	for _, span := range g.LowSupportSpans {
		kinds := span.EffectiveCriticalKinds()
		if len(kinds) != 1 || kinds[0] != "none" {
			return true
		}
	}
	return false
}
