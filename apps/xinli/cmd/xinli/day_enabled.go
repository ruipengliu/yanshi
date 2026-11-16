//go:build daytest

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"persona/domain"
	"persona/mcpserver"
	"persona/store"
	"xinli/capture"
	"xinli/capture/sender"
	"xinli/delivery"
	"xinli/kernel"
	"xinli/recovery"
	"xinli/schedule"
	"xinli/task"
)

// 钩子只存在于专用验收制品；还须显式开关、回环监听与父进程随机凭据。
func testFlags(flags *flag.FlagSet, cfg *config) func() error {
	enabled := flags.Bool("test-mode", false, "仅隔离验收：启用父进程控制")
	endpoint := flags.String("test-control-url", "", "仅隔离验收：父进程回环地址")
	return func() error {
		if !*enabled && *endpoint == "" {
			return nil
		}
		u, err := url.Parse(*endpoint)
		if cfg.mode != "serve" || !*enabled || err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !loopback(u.Hostname()) || len(os.Getenv("XINLI_DAY_CONTROL_TOKEN")) < 32 {
			return errors.New("xinli: invalid test control configuration")
		}
		for _, addr := range []string{cfg.addr, cfg.healthAddr} {
			host, _, err := net.SplitHostPort(addr)
			if err != nil || !loopback(host) {
				return errors.New("xinli: test control requires loopback listeners")
			}
		}
		cfg.test = &dayControl{base: strings.TrimRight(*endpoint, "/"), token: os.Getenv("XINLI_DAY_CONTROL_TOKEN"), http: &http.Client{Timeout: 45 * time.Second}, role: flags.Lookup("roles").Value.String(), autoSchedule: cfg.autoSchedule, exitOnEOF: cfg.exitOnEOF}
		return nil
	}
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type dayControl struct {
	base, token             string
	http                    *http.Client
	mu                      sync.Mutex
	offset                  time.Duration
	role                    string
	autoSchedule, exitOnEOF bool
	reads                   uint64
	firstOffset             time.Duration
	books                   store.Store
}

func (c *dayControl) request(ctx context.Context, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Xinli-Day-Pid", strconv.Itoa(os.Getpid()))
	req.Header.Set("X-Xinli-Day-Role", c.role)
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("xinli: test controller unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("xinli: injected failure")
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (c *dayControl) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		Offset int64 `json:"offset"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c.request(ctx, "/clock", nil, &out) != nil {
		// 无法同步时不能悄悄使用旧日期写入；仅测试制品退出，由父进程判失败。
		panic("xinli: test clock unavailable")
	}
	if time.Duration(out.Offset) < c.offset {
		panic("xinli: test clock moved backwards")
	}
	c.offset = time.Duration(out.Offset)
	if c.reads == 0 {
		c.firstOffset = c.offset
	}
	c.reads++
	return time.Now().Add(c.offset)
}

// 只挂在显式隔离的测试制品健康端口；父进程可在杀进程前确认该进程真正读到的时钟。
func (c *dayControl) Diagnostics() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+c.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		now := c.Now()
		c.mu.Lock()
		out := map[string]any{"pid": os.Getpid(), "role": c.role, "now": now.UTC(),
			"offset_ns": int64(c.offset), "first_offset_ns": int64(c.firstOffset), "clock_reads": c.reads,
			"auto_schedule": c.autoSchedule, "exit_on_stdin_eof": c.exitOnEOF, "scan_interval_ms": 100}
		c.mu.Unlock()
		if r.URL.Query().Get("schedule") == "1" && c.books != nil {
			// 正常时刻由元数据重算，并没有另一张 next_check 表；只展示真实规则给出的到点时刻。
			owners, err := c.books.SchedulingOwners(r.Context())
			out["owner_query_ok"] = err == nil
			var days []map[string]any
			for _, owner := range owners {
				if len(days) >= 200 {
					break
				}
				err := c.books.View(r.Context(), owner.BusinessLine, owner.EndUser, func(b *domain.Book) error {
					for _, day := range b.SchedulingDays() {
						if len(days) >= 200 {
							break
						}
						zone, err := time.LoadLocation(day.TimeZone)
						if err != nil {
							return err
						}
						daytime, final, err := schedule.DayTimes(day.Date, zone, day.LastVisibleBatchAt, schedule.Jitter(owner.BusinessLine, owner.EndUser, day.Date))
						if err != nil {
							return err
						}
						days = append(days, map[string]any{"business_line": owner.BusinessLine, "end_user": owner.EndUser, "date": day.Date,
							"last_batch_at": day.LastBatchAt, "time_zone": day.TimeZone, "daytime_due_at": daytime, "final_due_at": final,
							"last_visible_batch_at": day.LastVisibleBatchAt,
							"processed":             day.Processed, "progress": b.CaptureProgress(day.Date), "capture_revision": day.Revision,
							"expires_at": day.ExpiresAt, "session": day.Session})
					}
					return nil
				})
				if err != nil {
					out["metadata_query_ok"] = false
				}
			}
			out["scheduling_days"] = days
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

func (c *dayControl) hit(ctx context.Context, point string, value any) error {
	return c.request(ctx, "/hook", map[string]any{"point": point, "value": value}, nil)
}

func (c *dayControl) Ticks(ctx context.Context) <-chan time.Time {
	out := make(chan time.Time)
	go func() {
		defer close(out)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case at := <-ticker.C:
				select {
				case out <- at:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

type dayBooks struct {
	store.Store
	control *dayControl
}

func (c *dayControl) Books(s store.Store) store.Store { c.books = s; return dayBooks{s, c} }
func (s dayBooks) SchedulingOwners(ctx context.Context) ([]store.Owner, error) {
	if err := s.control.hit(ctx, "scheduler.discovery_started", map[string]any{"pid": os.Getpid(), "at": s.control.Now().UTC()}); err != nil {
		return nil, err
	}
	owners, err := s.Store.SchedulingOwners(ctx)
	s.control.executionError(ctx, "store.scheduling_owners", err)
	status := "ok"
	if err != nil {
		status = "store_error"
	}
	if reportErr := s.control.hit(ctx, "scheduler.discovery_completed", map[string]any{"pid": os.Getpid(), "at": s.control.Now().UTC(), "status": status, "owners": owners}); reportErr != nil {
		return nil, reportErr
	}
	return owners, err
}
func (s dayBooks) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) (err error) {
	defer func() { s.control.executionError(ctx, "store.update", err) }()
	return s.Store.Update(ctx, bl, eu, func(b *domain.Book) error {
		if err := fn(b); err != nil {
			return err
		}
		// 只携带日期与版本，让父测试识别目标采集事务，避免被后台更新抢先命中。
		days := make([]map[string]any, 0)
		for _, day := range b.SchedulingDays() {
			days = append(days, map[string]any{"date": day.Date, "revision": day.Revision})
		}
		return s.control.hit(ctx, "persona.before_commit", days)
	})
}

func (s dayBooks) PendingChangeOwners(ctx context.Context) ([]store.Owner, error) {
	if err := s.control.hit(ctx, "persona.discover_changes", nil); err != nil {
		return nil, err
	}
	return s.Store.PendingChangeOwners(ctx)
}

type dayNotifier struct {
	next    mcpserver.Notifier
	control *dayControl
}

func (c *dayControl) Notifier(n mcpserver.Notifier) mcpserver.Notifier { return dayNotifier{n, c} }
func (n dayNotifier) DayFiled(ctx context.Context, bl, eu, sid, date string, projects []string) {
	if n.control.hit(ctx, "persona.file_day_committed", map[string]string{"session_id": sid}) != nil {
		return
	}
	if n.next != nil {
		n.next.DayFiled(ctx, bl, eu, sid, date, projects)
	}
}
func (n dayNotifier) WeekFiled(ctx context.Context, bl, eu string) {
	if next, ok := n.next.(mcpserver.WeekNotifier); ok {
		next.WeekFiled(ctx, bl, eu)
	}
}
func (n dayNotifier) DeletionPending(ctx context.Context, bl, eu, id string) {
	if next, ok := n.next.(mcpserver.DeletionNotifier); ok {
		next.DeletionPending(ctx, bl, eu, id)
	}
}

type dayConsumer struct {
	recovery.Consumer
	control *dayControl
}

func (c *dayControl) Consumer(s recovery.Consumer) recovery.Consumer { return dayConsumer{s, c} }
func (s dayConsumer) Commit(ctx context.Context, expected uint64, progress recovery.Progress, batch recovery.Batch) error {
	if len(batch.Envelopes) > 0 {
		if err := s.control.hit(ctx, "consumer.before_commit", batch.Envelopes); err != nil {
			return err
		}
	}
	return s.Consumer.Commit(ctx, expected, progress, batch)
}

type dayDelivery struct {
	delivery.Store
	control *dayControl
}

func (c *dayControl) Delivery(s delivery.Store) delivery.Store { return dayDelivery{s, c} }
func (s dayDelivery) Process(ctx context.Context, e kernel.Envelope, now time.Time) error {
	if err := s.control.hit(ctx, "consumer.before_process", e); err != nil {
		return err
	}
	return s.Store.Process(ctx, e, now)
}

type dayPush struct {
	next    delivery.Push
	control *dayControl
}

func (c *dayControl) Push(p delivery.Push) delivery.Push { return dayPush{p, c} }
func (p dayPush) Push(ctx context.Context, owner task.Owner, message delivery.PushMessage) error {
	if err := p.next.Push(ctx, owner, message); err != nil {
		return err
	}
	// 父进程持有跨重启观察记录；只传 ID 与种类，不传正文或深链。
	return p.control.hit(ctx, "push.sent", map[string]string{"id": message.ID, "kind": message.Kind, "session_id": message.SessionID, "run_id": message.RunID, "call_id": message.CallID})
}

// 每条命令都重新打开同一发送端文件，故障恢复不能依赖进程内批次副本。
func testCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "day-sender" {
		return false, nil
	}
	var in struct {
		Path       string            `json:"path"`
		Identity   sender.Identity   `json:"identity"`
		Operation  string            `json:"operation"`
		URL        string            `json:"url"`
		Token      string            `json:"token"`
		Day        string            `json:"day"`
		Generation uint64            `json:"generation"`
		Segments   []capture.Segment `json:"segments"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 2<<20)).Decode(&in); err != nil {
		return true, err
	}
	s, err := sender.Open(in.Path, in.Identity)
	if err != nil {
		return true, err
	}
	defer s.Close()
	var batch capture.Batch
	switch in.Operation {
	case "enqueue":
		batch, err = s.Enqueue(in.Day, "Asia/Shanghai", in.Generation, in.Segments)
	case "flush":
		u, parseErr := url.Parse(in.URL)
		if parseErr != nil || !loopback(u.Hostname()) {
			return true, errors.New("sender requires loopback")
		}
		client := &capture.Client{BaseURL: in.URL, HTTP: &http.Client{Timeout: 5 * time.Second}, Token: func(context.Context) (string, error) { return in.Token, nil }}
		err = s.Flush(ctx, client)
	case "state":
	default:
		return true, errors.New("unknown sender command")
	}
	code := ""
	if err != nil {
		code = "unavailable"
		if errors.Is(err, capture.ErrResync) {
			code = "resync_required"
		}
		if errors.Is(err, sender.ErrGap) {
			code = "gap"
		}
		if errors.Is(err, capture.ErrRejected) {
			code = "rejected"
		}
	}
	return true, json.NewEncoder(stdout).Encode(map[string]any{"error": code, "batch": batch, "state": s.State()})
}

// 只传固定类别和计数信号；跨子进程重启的诊断由父驱动保存，不写业务库或工具正文。
func (c *dayControl) executionError(ctx context.Context, operation string, err error) {
	if err == nil {
		return
	}
	class := "operation_failed"
	var network net.Error
	var database *pgconn.PgError
	var connect *pgconn.ConnectError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		class = "timeout"
	case errors.Is(err, context.Canceled):
		class = "canceled"
	case errors.As(err, &network) && network.Timeout():
		class = "timeout"
	case errors.As(err, &database) && database.Code == "57014":
		class = "sql_canceled"
	case errors.As(err, &database) && database.Code == "55P03":
		class = "lock_timeout"
	case errors.As(err, &connect), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		class = "unavailable"
	}
	c.executionClass(ctx, operation, class)
}
func (c *dayControl) executionClass(ctx context.Context, operation, class string) {
	report, stop := context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
	defer stop()
	_ = c.hit(report, "execution.error", map[string]string{"component": c.role, "operation": operation, "class": class})
}
func (s dayBooks) View(ctx context.Context, bl, eu string, fn func(*domain.Book) error) (err error) {
	defer func() { s.control.executionError(ctx, "store.view", err) }()
	return s.Store.View(ctx, bl, eu, fn)
}
func (s dayBooks) ViewPlanned(ctx context.Context, bl, eu string, plan domain.ReadPlan, fn func(*domain.Book) error) (err error) {
	defer func() { s.control.executionError(ctx, "store.view", err) }()
	return s.Store.ViewPlanned(ctx, bl, eu, plan, fn)
}

type dayLogHandler struct {
	slog.Handler
	control *dayControl
}

func (c *dayControl) LogHandler(handler slog.Handler) slog.Handler { return dayLogHandler{handler, c} }
func (h dayLogHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	if record.Level >= slog.LevelWarn && strings.HasPrefix(record.Message, "xinli: ") {
		class := "operation_failed"
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "error" {
				switch attr.Value.String() {
				case "deadline exceeded":
					class = "timeout"
				case "canceled":
					class = "canceled"
				}
			}
			return true
		})
		h.control.executionClass(ctx, "orchestrator."+strings.TrimPrefix(record.Message, "xinli: "), class)
	}
	return err
}
func (h dayLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return dayLogHandler{h.Handler.WithAttrs(attrs), h.control}
}
func (h dayLogHandler) WithGroup(name string) slog.Handler {
	return dayLogHandler{h.Handler.WithGroup(name), h.control}
}
