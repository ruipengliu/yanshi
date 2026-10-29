package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"yanshi/internal/pg"
	"yanshi/internal/pg/idcheck"
)

func idPreflightCmd(args []string) error {
	fs := flag.NewFlagSet("id-preflight", flag.ContinueOnError)
	dsn := fs.String("pg-dsn", envOr("YANSHI_PG_DSN", devPGDSN), "历史快照 PostgreSQL 地址（只读访问）")
	historical := fs.Bool("historical-snapshot", false, "确认源库全部属于历史盘点范围；新旧混合库须先提取历史快照")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*historical {
		return errors.New("id-preflight requires -historical-snapshot; an ID cannot establish its creation era")
	}
	if fs.NArg() != 0 {
		return errors.New("id-preflight: unexpected positional argument")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pg.Open(ctx, *dsn, "")
	if err != nil {
		return err
	}
	defer pool.Close()
	enc := json.NewEncoder(os.Stdout)
	count, err := idcheck.ScanHistorical(ctx, pool, func(f idcheck.Finding) error { return enc.Encode(f) })
	if err != nil {
		return err
	}
	return idcheck.RequireClean(count)
}
