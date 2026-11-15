package shard

import "testing"

func TestHashVectors(t *testing.T) {
	for key, want := range map[string]int{"": 20, "u": 455, "用户": 119, "ses_x-1": 935, "end-user-123": 476} {
		if got := Hash(key); got != want {
			t.Errorf("Hash(%q) = %d, want %d", key, got, want)
		}
	}
}

func TestTable(t *testing.T) {
	c := Config{Shards: []int{0, 7}, Ranges: []Range{{First: 0, Last: 511, Shard: 7}, {First: 512, Last: 1023, Shard: 0}}}
	table, err := NewTable(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Ranges[0].Shard = 0
	for slot := 0; slot < SlotCount; slot++ {
		want := 0
		if slot < 512 {
			want = 7
		}
		if got, err := table.Shard(slot); err != nil || got != want {
			t.Fatalf("slot %d -> %d, %v; want %d", slot, got, err, want)
		}
	}
	for _, slot := range []int{-1, SlotCount} {
		if _, err := table.Shard(slot); err == nil {
			t.Errorf("accepted slot %d", slot)
		}
	}
	if _, err := (Table{}).Shard(0); err == nil {
		t.Fatal("zero table accepted")
	}
}

func TestParseTable(t *testing.T) {
	table, err := ParseTable([]byte(`{"shards":[0,1],"ranges":[{"first":0,"last":511,"shard":0},{"first":512,"last":1023,"shard":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := table.Shard(1023); err != nil || got != 1 {
		t.Fatalf("last slot = %d, %v", got, err)
	}
	for _, raw := range []string{
		``, `null`, `{}`, `{"shards":[0],"ranges":[]}`,
		`{"shards":[1],"ranges":[{"first":0,"last":1023,"shard":1}]}`,
		`{"shards":[0,0],"ranges":[{"first":0,"last":1023,"shard":0}]}`,
		`{"shards":[0,-1],"ranges":[{"first":0,"last":1023,"shard":0}]}`,
		`{"shards":[0],"ranges":[{"first":0,"last":1023,"shard":1}]}`,
		`{"shards":[0],"ranges":[{"first":1,"last":1023,"shard":0}]}`,
		`{"shards":[0],"ranges":[{"first":0,"last":1022,"shard":0}]}`,
		`{"shards":[0],"ranges":[{"first":-1,"last":1023,"shard":0}]}`,
		`{"shards":[0],"ranges":[{"first":0,"last":1024,"shard":0}]}`,
		`{"shards":[0],"ranges":[{"first":10,"last":9,"shard":0}]}`,
		`{"shards":[0],"ranges":[{"first":0,"last":1023,"shard":0},{"first":0,"last":0,"shard":0}]}`,
		`{"shards":[0],"ranges":[{"first":0,"last":511,"shard":0},{"first":511,"last":1023,"shard":0}]}`,
		`{"shards":[0],"ranges":[{"first":0,"last":1023,"shard":0,"typo":1}]}`,
		`{"shards":[0],"ranges":[{"first":0,"last":1023,"shard":0}],"typo":1}`,
		`{"shards":[0],"ranges":[{"first":0,"last":1023,"shard":0}]} {}`,
		`{"shards":[0],"ranges":[{"first":0,"last":1023,"shard":0}]} garbage`,
	} {
		if _, err := ParseTable([]byte(raw)); err == nil {
			t.Errorf("accepted invalid config: %s", raw)
		}
	}
}

func TestRemapDoesNotChangeSlots(t *testing.T) {
	one, err := NewTable(Config{Shards: []int{0}, Ranges: []Range{{First: 0, Last: 1023, Shard: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewTable(Config{Shards: []int{0, 1}, Ranges: []Range{{First: 0, Last: 1023, Shard: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	slot := Hash("u")
	a, errA := one.Shard(slot)
	b, errB := two.Shard(slot)
	if slot != 455 || a != 0 || b != 1 || errA != nil || errB != nil {
		t.Fatalf("remap = slot %d, shards %d/%d, errors %v/%v", slot, a, b, errA, errB)
	}
}
