package costtest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"persona/domain"
	"persona/pg/pgtest"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
)

const defaultSamples = 200

type sqlCounter struct {
	active atomic.Bool
	count  atomic.Int64
}

// 每个 pgx Query/Exec 计一次，包括 BEGIN/COMMIT/ROLLBACK；不把协议 prepare 当 SQL。
// 专属池串行测量，按调用区间启用，才能包含脱离请求上下文的清理 ROLLBACK。
func (c *sqlCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if c.active.Load() {
		c.count.Add(1)
	}
	return ctx
}

func (*sqlCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type sample struct {
	latency   time.Duration
	load      store.LoadStats
	sql       int64
	heapPeak  uint64
	heapRise  uint64
	allocated uint64
	wal       int64
}

type backend struct {
	store   store.Store
	monitor *pgxpool.Pool
	counter *sqlCounter
}

func TestLargeWorkingSetCost(t *testing.T) {
	if os.Getenv("PERSONA_BENCH") != "1" {
		t.Skip("set PERSONA_BENCH=1 to run the A14 cost report")
	}
	// 显式开启后缺 PG 是失败，不能输出只有内存结果的完整报告。
	if os.Getenv("YANSHI_TEST_PG") == "" {
		t.Fatal("PERSONA_BENCH requires YANSHI_TEST_PG")
	}
	n := defaultSamples
	if value := os.Getenv("PERSONA_BENCH_N"); value != "" {
		var err error
		n, err = strconv.Atoi(value)
		if err != nil || n < 1 {
			t.Fatal("PERSONA_BENCH_N must be a positive integer")
		}
	}
	fmt.Printf("\nPersona A14: seed=%d, N=%d, date=%s, UTC\n", recipeSeed, n, fixtureNow.Format(time.DateOnly))
	fmt.Printf("Go=%s OS=%s arch=%s CPU=%d GOMAXPROCS=%d\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0))
	fmt.Printf("recipe: days=%d exchanges/day=%d commitments/day=%d projects=%d weeks=%d captures=%d*%d*%d*%d chars\n", ledgerDays, exchangesPerDay, commitmentsPerDay, projectCount, weekCount, captureDays, batchesPerDay, segmentsPerBatch, charsPerSegment)
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/jackc/pgx/v5" {
				fmt.Printf("pgx=%s\n", dep.Version)
			}
		}
	}
	var expectedDigest [32]byte
	for _, name := range []string{"memory", "postgres"} {
		t.Run(name, func(t *testing.T) {
			b := backend{store: memstore.New(func() time.Time { return fixtureNow })}
			if name == "postgres" {
				monitor, dsn := pgtest.NewWithDSN(t)
				config, err := pgxpool.ParseConfig(dsn)
				must(t, err)
				counter := &sqlCounter{}
				config.ConnConfig.Tracer = counter
				config.MaxConns, config.MinConns = 1, 0
				pool, err := pgxpool.NewWithConfig(context.Background(), config)
				must(t, err)
				t.Cleanup(pool.Close)
				must(t, pool.Ping(context.Background()))
				b = backend{store: pgstore.New(pool, func() time.Time { return fixtureNow }), monitor: monitor, counter: counter}
				var version, walLevel, syncCommit, fullPageWrites string
				must(t, monitor.QueryRow(context.Background(), "SELECT version(),current_setting('wal_level'),current_setting('synchronous_commit'),current_setting('full_page_writes')").Scan(&version, &walLevel, &syncCommit, &fullPageWrites))
				fmt.Printf("PG=%s; wal_level=%s synchronous_commit=%s full_page_writes=%s; measured pool max=1\n", version, walLevel, syncCommit, fullPageWrites)
			}
			results := map[string][]sample{}
			order := []string{"captures_middle", "captures_tail", "append", "day_ledger", "list_commitments", "find_related", "file_changes", "replace_source", "erase", "expire_day"}
			started := time.Now()
			var setup time.Duration
			for i := range n {
				prepare := time.Now()
				generate(t, b.store)
				digest := validateFixture(t, b.store)
				if expectedDigest == ([32]byte{}) {
					expectedDigest = digest
					fmt.Printf("fixture SHA256=%x\n", digest)
				}
				if digest != expectedDigest {
					t.Fatal("fixture differs between samples/backends")
				}
				setup += time.Since(prepare)
				runSample(t, b, results)
				if i == 0 || (i+1)%10 == 0 || i+1 == n {
					t.Logf("%s completed %d/%d; elapsed=%s setup=%s", name, i+1, n, time.Since(started).Round(time.Second), setup.Round(time.Second))
				}
			}
			fmt.Printf("\n%s: setup=%s total=%s (setup and verification excluded from samples)\n", name, setup, time.Since(started))
			fmt.Println("| backend | operation | N | p50 ms | p99 ms | ledger B/tx p50 / max | segments/tx p50 / max | SQL/tx p50 / max | heap peak MiB | heap rise MiB | allocated MiB/op p50 | WAL B/op p50 / p99 |")
			fmt.Println("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
			for _, operation := range order {
				printRow(name, operation, results[operation])
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func view(t *testing.T, s store.Store, fn func(*domain.Book)) {
	t.Helper()
	must(t, s.View(context.Background(), ownerLine, ownerUser, func(book *domain.Book) error {
		fn(book)
		return nil
	}))
}

func validateFixture(t *testing.T, s store.Store) [32]byte {
	t.Helper()
	var digest [32]byte
	stats := &store.LoadStats{}
	must(t, s.View(store.WithLoadStats(context.Background(), stats), ownerLine, ownerUser, func(book *domain.Book) error {
		state := book.State()
		if len(state.Days) != ledgerDays || len(state.Exchanges) != ledgerDays*exchangesPerDay || len(state.Commitments) != ledgerDays*commitmentsPerDay || len(state.Projects) != projectCount || len(state.Weeks) != weekCount || len(state.Changesets) != changesetCount {
			return fmt.Errorf("fixture ledger counts differ: days=%d exchanges=%d commitments=%d projects=%d weeks=%d changesets=%d", len(state.Days), len(state.Exchanges), len(state.Commitments), len(state.Projects), len(state.Weeks), len(state.Changesets))
		}
		merged, revisions, done, cancelled := 0, 0, 0, 0
		for _, commitment := range state.Commitments {
			if commitment.Unverified != "" {
				return fmt.Errorf("fixture contains unverified commitment")
			}
			if commitment.MergedInto != "" {
				merged++
			}
			for _, revision := range commitment.Revisions {
				if revision.Op == "revise" {
					revisions++
				}
			}
			if commitment.Status == "done" {
				done++
			}
			if commitment.Status == "cancelled" {
				cancelled++
			}
		}
		if merged != mergeCount || revisions != revisionCount || done != ledgerDays || cancelled != ledgerDays {
			return fmt.Errorf("fixture revision/status counts differ: merged=%d revisions=%d done=%d cancelled=%d", merged, revisions, done, cancelled)
		}
		if len(book.DeletionTasks()) != deletionCount || len(book.PendingDeletionTasks()) != 0 {
			return fmt.Errorf("fixture deletion task counts differ")
		}
		var stored struct {
			CaptureDays map[string]struct{ Tombstones []domain.Source }
		}
		data := encode(book)
		if err := json.Unmarshal(data, &stored); err != nil {
			return err
		}
		tombstones := 0
		for _, day := range stored.CaptureDays {
			tombstones += len(day.Tombstones)
		}
		if tombstones != deletionCount {
			return fmt.Errorf("fixture tombstone count differs")
		}
		for day := range ledgerDays {
			date := dayTime(day).Format(time.DateOnly)
			if day < ledgerDays-captureDays {
				if !book.BusinessDay(date).Expired {
					return fmt.Errorf("historical capture day is still live")
				}
				continue
			}
			segments, err := book.DayCaptures(date)
			if err != nil || len(segments) != batchesPerDay*segmentsPerBatch {
				return fmt.Errorf("fixture capture count differs: count=%d error=%v", len(segments), err)
			}
			for _, segment := range segments {
				if utf8.RuneCountInString(segment.Text) != charsPerSegment {
					return fmt.Errorf("fixture capture character count differs")
				}
			}
			if segments[0].Text != batch(day, 1)[0].Text || segments[len(segments)-1].Text != batch(day, batchesPerDay)[segmentsPerBatch-1].Text {
				return fmt.Errorf("fixture capture text differs")
			}
		}
		// 摘要只含确定性的人工配方；跨轮、跨存储比较包含墓碑和全部元数据。
		digest = sha256.Sum256(data)
		return nil
	}))
	if stats.LedgerBytes == 0 || stats.CaptureSegments != captureDays*batchesPerDay*segmentsPerBatch {
		t.Fatalf("load counters failed calibration: %+v", stats)
	}
	return digest
}

func (b backend) lsn(t *testing.T) string {
	var value string
	must(t, b.monitor.QueryRow(context.Background(), "SELECT pg_current_wal_lsn()::text").Scan(&value))
	return value
}

// 采样堆峰值包含存储与领域工作；ReadMemStats 会影响耗时，报告明确这是带探针的成本。
// 前后 TotalAlloc 差同时提供采样可能漏掉的短命分配量。
func measure(t *testing.T, b backend, wal bool, fn func(context.Context) error) sample {
	t.Helper()
	stats := &store.LoadStats{}
	ctx := store.WithLoadStats(context.Background(), stats)
	beforeLSN := ""
	if wal && b.monitor != nil {
		beforeLSN = b.lsn(t)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	peak := before.HeapAlloc
	stop, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var current runtime.MemStats
				runtime.ReadMemStats(&current)
				peak = max(peak, current.HeapAlloc)
			case <-stop:
				return
			}
		}
	}()
	if b.counter != nil {
		b.counter.count.Store(0)
		b.counter.active.Store(true)
	}
	start := time.Now()
	err := fn(ctx)
	elapsed := time.Since(start)
	if b.counter != nil {
		b.counter.active.Store(false)
	}
	close(stop)
	<-finished
	runtime.ReadMemStats(&after)
	peak = max(peak, after.HeapAlloc)
	must(t, err)
	out := sample{latency: elapsed, load: *stats, heapPeak: peak, heapRise: peak - before.HeapAlloc, allocated: after.TotalAlloc - before.TotalAlloc, wal: -1}
	if b.counter != nil {
		out.sql = b.counter.count.Load()
	}
	if beforeLSN != "" {
		afterLSN := b.lsn(t)
		must(t, b.monitor.QueryRow(context.Background(), "SELECT pg_wal_lsn_diff($1::pg_lsn,$2::pg_lsn)::bigint", afterLSN, beforeLSN).Scan(&out.wal))
	}
	return out
}

func runSample(t *testing.T, b backend, results map[string][]sample) {
	t.Helper()
	day := ledgerDays - 1
	date, session := dayTime(day).Format(time.DateOnly), daySession(day)
	run := func(name string, write, wal bool, bound int64, fn func(*domain.Book) error) {
		t.Helper()
		op := b.store.View
		if write {
			op = b.store.Update
		}
		result := measure(t, b, wal, func(ctx context.Context) error { return op(ctx, ownerLine, ownerUser, fn) })
		if result.load.LedgerBytes <= 0 {
			t.Fatal("missing ledger load observation")
		}
		if bound >= 0 && (result.load.CaptureSegments > bound || result.load.CaptureSegments >= captureDays*batchesPerDay*segmentsPerBatch/10) {
			t.Fatalf("%s loaded %d capture segments, bound=%d", name, result.load.CaptureSegments, bound)
		}
		if b.monitor != nil && result.sql == 0 || b.monitor == nil && result.sql != 0 {
			t.Fatal("SQL counter differs from backend")
		}
		results[name] = append(results[name], result)
	}
	for _, position := range []struct {
		name, after, first, last string
		batch, count             int
		done                     bool
	}{
		{"captures_middle", "600.2~1.0", "601.0", "667.1", 601, 200, false},
		{"captures_tail", "1190.2~1.0", "1191.0", "1200.2", 1191, 30, true},
	} {
		var page domain.StructuredPage
		args := encode(object{"after": position.after, "all": true})
		run(position.name, true, false, 512, func(book *domain.Book) error {
			var err error
			page, err = domain.NewCall(book, session, args, nil, fixtureNow).Captures(pageOptions)
			return err
		})
		if page.Kind != "page" || len(page.Segments) != position.count || page.Done != position.done || page.Segments[0].Ref != position.first || page.Segments[len(page.Segments)-1].Ref != position.last || page.Segments[0].Text != batch(day, position.batch)[0].Text {
			t.Fatalf("%s returned unexpected page", position.name)
		}
	}
	var refs []string
	appended := batch(day, batchesPerDay+1)
	run("append", true, true, 16, func(book *domain.Book) error {
		var err error
		refs, err = book.AppendCaptures(date, "measured-append", appended)
		return err
	})
	if !slices.Equal(refs, []string{"1201.0", "1201.1", "1201.2"}) {
		t.Fatal("append allocated unexpected refs")
	}
	view(t, b.store, func(book *domain.Book) {
		segment, found, err := book.CaptureAccess().Lookup(date, domain.Ref{Seq: 1201})
		must(t, err)
		if !found || segment != batch(day, batchesPerDay+1)[0] || book.BusinessDay(date).CaptureRevision != batchesPerDay+1 {
			t.Fatal("append did not persist batch and revision")
		}
	})
	for _, query := range []struct {
		name     string
		args     object
		method   func(*domain.Call) string
		contains string
	}{
		{"day_ledger", object{}, (*domain.Call).DayLedger, "x1.0"},
		{"list_commitments", object{"status": "open"}, (*domain.Call).ListCommitments, fmt.Sprintf("共 %d 条", openCommitmentCount)},
		{"find_related", object{"query": "交付材料", "counterparts": []string{"同事"}}, (*domain.Call).FindRelated, "C-"},
	} {
		var out string
		args := encode(query.args)
		run(query.name, false, false, 0, func(book *domain.Book) error {
			call := domain.NewCall(book, session, args, nil, fixtureNow)
			out = query.method(call)
			return call.Err()
		})
		if !strings.Contains(out, query.contains) {
			t.Fatalf("%s returned unexpected query result", query.name)
		}
	}
	args := object{"source": source(21, 21), "exchanges": object{"create": []object{{"from": "#21.0", "to": "#21.2", "title": "追加交付讨论", "summary": "确认新的材料清单", "projects": []object{{"match": "existing", "id": "P-0101"}}}}}, "commitments": object{"create": []object{{"exchange": "x21.0", "kind": "mine", "content": "确认新的材料清单", "refs": []string{"#21.0"}}}}}
	fileArgs, fileCites := encode(args), citations(day, 21, 21)
	var receipt string
	file := func(book *domain.Book) error {
		var err error
		receipt, err = checkedCall(domain.NewCall(book, session, fileArgs, fileCites, fixtureNow), (*domain.Call).FileChanges, "已收下变更集")
		return err
	}
	run("file_changes", true, false, -1, file)
	ids := commitmentID.FindAllStringSubmatch(receipt, -1)
	if len(ids) != 1 {
		t.Fatal("file_changes did not return one commitment")
	}
	var oldID string
	view(t, b.store, func(book *domain.Book) {
		state := book.State()
		if len(state.Changesets) != changesetCount+1 || len(state.Exchanges) != ledgerDays*exchangesPerDay+1 || len(state.Commitments) != ledgerDays*commitmentsPerDay+1 {
			t.Fatal("file_changes did not persist its objects")
		}
		oldID = state.Changesets[len(state.Changesets)-1].ID
	})
	// 留一个后续来源，验证取代确实原位发生；准备不计入重交成本。
	var followingID string
	must(t, b.store.Update(context.Background(), ownerLine, ownerUser, func(book *domain.Book) error {
		_, err := invoke(book, session, object{"source": source(22, 22)}, citations(day, 22, 22), fixtureNow, (*domain.Call).FileChanges, "已收下变更集")
		return err
	}))
	view(t, b.store, func(book *domain.Book) {
		state := book.State()
		followingID = state.Changesets[len(state.Changesets)-1].ID
	})
	args["exchanges"].(object)["create"].([]object)[0]["summary"] = "确认修正后的材料清单"
	args["commitments"].(object)["create"].([]object)[0]["id"] = ids[0][1]
	fileArgs = encode(args)
	run("replace_source", true, true, -1, file)
	view(t, b.store, func(book *domain.Book) {
		state := book.State()
		if len(state.Changesets) != changesetCount+2 {
			t.Fatal("source replacement changed changeset count")
		}
		replaced := state.Changesets[len(state.Changesets)-2]
		if replaced.ID == oldID || len(replaced.CreateExchanges) != 1 || len(replaced.CreateCommitments) != 1 || replaced.CreateExchanges[0].Summary != "确认修正后的材料清单" || replaced.CreateCommitments[0].ID != ids[0][1] || len(state.Commitments) != ledgerDays*commitmentsPerDay+1 || state.Changesets[len(state.Changesets)-1].ID != followingID {
			t.Fatal("source replacement did not preserve position and commitment identity")
		}
	})
	var task domain.DeletionTask
	run("erase", true, true, -1, func(book *domain.Book) error {
		var err error
		task, err = book.Erase(date, "21.0", "21.0", fixtureNow)
		return err
	})
	view(t, b.store, func(book *domain.Book) {
		_, found, err := book.CaptureAccess().Lookup(date, domain.Ref{Seq: 21})
		must(t, err)
		_, neighbor, err := book.CaptureAccess().Lookup(date, domain.Ref{Seq: 21, Index: 1})
		must(t, err)
		state := book.State()
		if found || !neighbor || task.Status != "pending" || len(book.PendingDeletionTasks()) != 1 || book.BusinessDay(date).CaptureRevision != batchesPerDay+2 || len(state.Exchanges) != ledgerDays*exchangesPerDay || len(state.Commitments) != ledgerDays*commitmentsPerDay {
			t.Fatal("erase did not remove dependent facts and preserve neighbor")
		}
	})
	oldest := dayTime(ledgerDays - captureDays).Format(time.DateOnly)
	var before [32]byte
	var beforeDay domain.BusinessDay
	view(t, b.store, func(book *domain.Book) {
		before = sha256.Sum256(encode(book.State()))
		beforeDay = book.BusinessDay(oldest)
	})
	run("expire_day", true, false, -1, func(book *domain.Book) error { return book.ExpireCaptures(oldest, beforeDay.ExpiresAt) })
	view(t, b.store, func(book *domain.Book) {
		afterDay := book.BusinessDay(oldest)
		if sha256.Sum256(encode(book.State())) != before || !afterDay.Expired || afterDay.Deleted || afterDay.Session != beforeDay.Session || afterDay.CaptureRevision != beforeDay.CaptureRevision+1 || book.CheckCaptures(oldest) != domain.ErrCapturesExpired {
			t.Fatal("natural expiration changed ledger/binding or left access open")
		}
	})
	// PG 另查物理行，避免仅靠到期访问闸门误判清理完成；该 SQL 不参与样本。
	if b.monitor != nil {
		var count int
		must(t, b.monitor.QueryRow(context.Background(), "SELECT count(*) FROM capture_segments WHERE business_line=$1 AND end_user=$2 AND business_day=$3", []byte(ownerLine), []byte(ownerUser), oldest).Scan(&count))
		if count != 0 {
			t.Fatal("expired PG capture payload remains")
		}
	}
}

func percentile(values []int64, fraction float64) int64 {
	values = slices.Clone(values)
	slices.Sort(values)
	return values[max(0, int(math.Ceil(float64(len(values))*fraction))-1)]
}

func printRow(name, operation string, samples []sample) {
	var latency, ledger, segments, sql, allocations, wal []int64
	var heapPeak, heapRise uint64
	for _, sample := range samples {
		latency = append(latency, int64(sample.latency))
		ledger = append(ledger, sample.load.LedgerBytes)
		segments = append(segments, sample.load.CaptureSegments)
		sql = append(sql, sample.sql)
		allocations = append(allocations, int64(sample.allocated))
		wal = append(wal, sample.wal)
		heapPeak, heapRise = max(heapPeak, sample.heapPeak), max(heapRise, sample.heapRise)
	}
	walText := "—"
	if wal[0] >= 0 {
		walText = fmt.Sprintf("%d / %d", percentile(wal, .5), percentile(wal, .99))
	}
	fmt.Printf("| %s | %s | %d | %.3f | %.3f | %d / %d | %d / %d | %d / %d | %.2f | %.2f | %.2f | %s |\n", name, operation, len(samples), float64(percentile(latency, .5))/1e6, float64(percentile(latency, .99))/1e6, percentile(ledger, .5), slices.Max(ledger), percentile(segments, .5), slices.Max(segments), percentile(sql, .5), slices.Max(sql), float64(heapPeak)/(1<<20), float64(heapRise)/(1<<20), float64(percentile(allocations, .5))/(1<<20), walText)
}
