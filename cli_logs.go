package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"
)

// cmdLogs queries the log store directly (read-only), so it works while
// fluid-config is running and without an admin token.
func cmdLogs(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.json", "static config file (to find the database)")
	dbPath := fs.String("db", "", "database path (overrides config)")
	kind := fs.String("kind", "", "comma-separated kinds: api, access, admin, config, deploy, system, app")
	level := fs.String("level", "", "minimum level: debug, info, warn, error")
	client := fs.String("client", "", "API credential or console user")
	route := fs.String("route", "", "api, e.g. v1/holidays")
	sourceIP := fs.String("source-ip", "", "caller IP")
	status := fs.String("status", "", "status code, 2xx..5xx, or error")
	since := fs.String("since", "1h", "RFC3339 or duration (15m, 24h, 7d); empty for all")
	until := fs.String("until", "", "RFC3339 upper bound")
	text := fs.String("q", "", "text to search in message, URL and attributes")
	limit := fs.Int("limit", 50, "max rows (1-1000)")
	by := fs.String("by", "", "summarize by: kind, level, client, route, status, source_ip, method")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *dbPath
	if path == "" {
		cfg, err := loadStatic(*cfgPath)
		if err != nil {
			return err
		}
		path = cfg.Database.Path
	}
	db, err := OpenDBReadOnly(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()

	q := LogQuery{Level: *level, Client: *client, Route: *route,
		SourceIP: *sourceIP, Status: *status, Text: *text, Limit: *limit}
	if *kind != "" {
		q.Kinds = strings.Split(*kind, ",")
	}
	if q.Since, err = parseTimeArg(*since); err != nil {
		return err
	}
	if *until != "" {
		if q.Until, err = time.Parse(time.RFC3339, *until); err != nil {
			return fmt.Errorf("until must be RFC3339")
		}
	}
	ctx := context.Background()
	if *by != "" {
		rows, err := SummarizeLogs(ctx, db, q, *by)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(out).Encode(rows)
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\tCOUNT\tERRORS\tAVG_MS\tBYTES_OUT\tLAST_SEEN\n", strings.ToUpper(*by))
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%.0f\t%d\t%s\n", orDash(r.Key), r.Count, r.Errors, r.AvgMs, r.BytesOut, r.LastSeenISO)
		}
		return tw.Flush()
	}
	rows, _, err := QueryLogs(ctx, db, q)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		for _, r := range rows {
			enc.Encode(r)
		}
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tLEVEL\tKIND\tDIR\tCLIENT/ROUTE\tMETHOD\tSTATUS\tMS\tURL / MESSAGE")
	for i := len(rows) - 1; i >= 0; i-- { // oldest first, like tail
		r := rows[i]
		who := r.Client
		if who == "" {
			who = r.Route
		}
		what := r.URL
		if what == "" {
			what = r.Msg
		} else if r.Level != "info" {
			what += "  (" + r.Msg + ")"
		}
		st := "-"
		if r.Status > 0 {
			st = fmt.Sprint(r.Status)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", r.TS.Local().Format("01-02 15:04:05"), r.Level, r.Kind,
			orDash(r.Direction), orDash(who), orDash(r.Method), st, r.DurationMs, what)
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// insecureLocal is for the healthcheck only: it probes 127.0.0.1, which the
// listener's certificate does not name.
func insecureLocal() *http.Transport {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // loopback probe
}
