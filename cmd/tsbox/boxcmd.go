package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// withClient connects (with autostart) and runs fn; Ctrl+C cancels ctx.
func withClient(fn func(ctx context.Context, c *client.Client) int) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c, _, err := connect(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsbox:", err)
		return 1
	}
	defer c.Close()
	return fn(ctx, c)
}

// parseSince understands durations (10m, 2h) and points in time (RFC 3339 or 2006-01-02).
func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since %q: duration (10m) or point in time (RFC 3339) expected", s)
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMi", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKi", float64(n)/(1<<10))
	}
	return strconv.FormatInt(n, 10) + "B"
}

func since(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return d.String()
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func running(s tsbox.BoxState) bool {
	switch s {
	case tsbox.BoxStarting, tsbox.BoxRunning, tsbox.BoxStopping:
		return true
	}
	return false
}

// ps [-a] [--json]
func psCmd(args []string) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	all := fs.Bool("a", false, "also ended boxes")
	asJSON := fs.Bool("json", false, "as JSON")
	usageFor(fs, "ps [-a] [--json]")
	if _, ok := parsePos(fs, args, 0, 0); !ok {
		return 2
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		boxes, err := c.List(ctx, *all)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tsbox:", err)
			return 1
		}
		if *asJSON {
			printJSON(boxes)
			return 0
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "BOX\tSTATE\tSCRIPTS\tDRIVER\tUPTIME\tPROJECT")
		for _, b := range boxes {
			ready := 0
			for _, s := range b.Scripts {
				if s.State == tsbox.ScriptReady || s.State == tsbox.ScriptRunning {
					ready++
				}
			}
			up := "-"
			if running(b.State) {
				up = since(b.Started)
			}
			state := string(b.State)
			if b.Error != "" && !running(b.State) {
				state += " (" + trunc(b.Error, 40) + ")"
			} else if !running(b.State) && len(b.Scripts) == 1 && b.Scripts[0].LastExit != nil {
				// like docker ps: "exited (3)", or the reason of a limit ("exited (timeout)")
				ex := b.Scripts[0].LastExit
				if ex.Reason == tsbox.ReasonExit || ex.Reason == tsbox.ReasonCrash {
					state += fmt.Sprintf(" (%d)", ex.Code)
				} else {
					state += " (" + string(ex.Reason) + ")"
				}
			}
			proj := b.Labels[client.LabelProject]
			if proj == "" {
				proj = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\t%s\n", b.Name, state, ready, len(b.Scripts), b.Driver, up, proj)
		}
		w.Flush()
		return 0
	})
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:max(n-3, 0)]) + "..."
	}
	return s
}

// logs [-f] [--since] [--script] [-n] [-t] <box>
func logsCmd(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow new lines until the box ends")
	sinceS := fs.String("since", "", "from a point in time (RFC 3339) or duration (e.g. 10m)")
	script := fs.String("script", "", "only this script")
	tail := fs.Int("n", 0, "only the last n lines")
	ts := fs.Bool("t", false, "print timestamps")
	usageFor(fs, "logs [-f] [--since 10m] [--script name] [-n 100] [-t] <box>")
	names, ok := parsePos(fs, args, 1, 1)
	if !ok {
		return 2
	}
	if len(names) != 1 {
		fmt.Fprintln(os.Stderr, "tsbox logs: exactly one box")
		return 2
	}
	if *tail < 0 {
		usageErr(fs, fmt.Sprintf("-n %d: must not be negative", *tail))
		return 2
	}
	from, err := parseSince(*sinceS)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsbox:", err)
		return 2
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		info, err := c.Inspect(ctx, names[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "tsbox:", err)
			return 1
		}
		if *script != "" {
			var known []string
			for _, s := range info.Spec.Scripts {
				known = append(known, s.Name)
			}
			if !slices.Contains(known, *script) {
				fmt.Fprintf(os.Stderr, "tsbox: box %q has no script %q (scripts: %s)\n", names[0], *script, strings.Join(known, ", "))
				return 1
			}
		}
		prefix := len(info.Spec.Scripts) > 1 && *script == ""
		for l, err := range c.Logs(ctx, names[0], client.LogQuery{Follow: *follow, Since: from, Script: *script, Tail: *tail}) {
			if err != nil {
				if ctx.Err() != nil {
					return exitInterrupt // Ctrl+C, like run
				}
				fmt.Fprintln(os.Stderr, "tsbox:", err)
				return 1
			}
			printLogLine(l, prefix, *ts)
		}
		return interrupted(ctx)
	})
}

func printLogLine(l tsbox.LogLine, prefix, ts bool) {
	w := os.Stdout
	head := ""
	if ts {
		head = l.At.Local().Format("2006-01-02T15:04:05.000") + " "
	}
	if prefix {
		head += l.Script + " | "
	}
	switch l.Level {
	case "warn", "error":
		w = os.Stderr
		head += strings.ToUpper(l.Level) + " "
	case "stderr":
		w = os.Stderr
	}
	fmt.Fprintln(w, head+l.Msg)
}

// stats [--json] [--no-stream] [box]
func statsCmd(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "as JSON (with stream: one line per second)")
	noStream := fs.Bool("no-stream", false, "print only once")
	usageFor(fs, "stats [--json] [--no-stream] [box]")
	names, ok := parsePos(fs, args, 0, 1)
	if !ok {
		return 2
	}
	if len(names) > 1 {
		fmt.Fprintln(os.Stderr, "tsbox stats: at most one box")
		return 2
	}
	box := ""
	if len(names) == 1 {
		box = names[0]
	}
	live := !*noStream && !*asJSON && isTerminal(os.Stdout)
	if live {
		enableANSI()
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		for entries, err := range c.Stats(ctx, box, !*noStream) {
			if err != nil {
				if ctx.Err() != nil {
					return exitInterrupt // Ctrl+C, like run
				}
				fmt.Fprintln(os.Stderr, "tsbox:", err)
				return 1
			}
			if *asJSON {
				if *noStream {
					printJSON(entries)
				} else {
					b, _ := json.Marshal(entries)
					fmt.Println(string(b))
				}
				continue
			}
			if live {
				fmt.Print("\x1b[H\x1b[2J")
			}
			statsTable(os.Stdout, entries)
		}
		return interrupted(ctx)
	})
}

func statsTable(out io.Writer, entries []client.StatsEntry) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BOX\tSCRIPT\tSTATE\tRAM/LIMIT\tCPU\tPIDS\tRESTARTS\tUPTIME")
	for _, e := range entries {
		restarts := 0
		for _, s := range e.Scripts {
			restarts += s.Restarts
		}
		fmt.Fprintf(w, "%s\t*\t%s\t%s / %s\t%.1f%%\t%d\t%d\t%s\n", e.Box, e.State, human(e.Stats.Memory), human(e.Limits.Memory),
			e.Stats.CPUCores*100, e.Stats.PIDs, restarts, since(e.Started))
		byName := map[string]tsbox.ScriptStats{}
		for _, s := range e.Stats.Scripts {
			byName[s.Name] = s
		}
		for _, s := range e.Scripts {
			st := byName[s.Name]
			limit := "-"
			if l := e.RSSLimits[s.Name]; l > 0 {
				limit = human(l)
			}
			up := "-"
			if s.State == tsbox.ScriptRunning || s.State == tsbox.ScriptReady {
				up = since(s.Started)
			}
			rss := "-" // not measurable, e.g. under gVisor (the processes live in the sandbox)
			if st.RSS > 0 {
				rss = human(st.RSS)
			}
			fmt.Fprintf(w, "\t%s\t%s\t%s / %s\t%.1f%%\t%d\t%d\t%s\n", s.Name, s.State, rss, limit, st.CPUCores*100,
				st.Threads, s.Restarts, up)
		}
	}
	w.Flush()
}

// stop/start/restart/rm for several boxes.
func lifecycleCmd(cmd string, args []string) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	grace := new(time.Duration)
	force := new(bool)
	switch cmd {
	case "stop", "restart":
		fs.DurationVar(grace, "t", 0, "deadline for the scripts (default 5s)")
	case "rm":
		fs.BoolVar(force, "f", false, "also running boxes (stop them first)")
	}
	usageFor(fs, cmd+" [flags] <box> [<box> ...]")
	names, ok := parseNames(fs, args, 1)
	if !ok {
		return 2
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		code := 0
		for _, n := range names {
			var err error
			switch cmd {
			case "stop":
				_, err = c.Stop(ctx, n, *grace)
			case "start":
				var info client.Inspect
				if info, err = c.Inspect(ctx, n); err == nil {
					_, err = c.Start(ctx, n, client.LookupEnv(info.Spec))
				}
			case "restart":
				_, err = c.Restart(ctx, n, *grace)
			case "rm":
				err = c.Remove(ctx, n, *force)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "tsbox %s %s: %v\n", cmd, n, err)
				code = 1
				continue
			}
			fmt.Println(n)
		}
		return code
	})
}

// inspect <box> … (JSON, without secret values)
func inspectCmd(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	usageFor(fs, "inspect <box> [<box> ...]")
	names, ok := parseNames(fs, args, 1)
	if !ok {
		return 2
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		var out []client.Inspect
		code := 0
		for _, n := range names {
			i, err := c.Inspect(ctx, n)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tsbox inspect %s: %v\n", n, err)
				code = 1
				continue
			}
			out = append(out, i)
		}
		if len(out) > 0 {
			printJSON(out)
		}
		return code
	})
}

// events [--since] [--box] [--json]
func eventsCmd(args []string) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	sinceS := fs.String("since", "", "also earlier events from a point in time or duration (buffer: 1000)")
	box := fs.String("box", "", "only this box")
	asJSON := fs.Bool("json", false, "as JSON lines")
	usageFor(fs, "events [--since 10m] [--box name] [--json]")
	if _, ok := parsePos(fs, args, 0, 0); !ok {
		return 2
	}
	from, err := parseSince(*sinceS)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsbox:", err)
		return 2
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		for ev, err := range c.Events(ctx, from, *box) {
			if err != nil {
				if ctx.Err() != nil {
					return exitInterrupt // Ctrl+C, like run
				}
				fmt.Fprintln(os.Stderr, "tsbox:", err)
				return 1
			}
			if *asJSON {
				b, _ := json.Marshal(ev)
				fmt.Println(string(b))
				continue
			}
			fmt.Println(formatEvent(ev))
		}
		return interrupted(ctx)
	})
}

func formatEvent(ev tsbox.Event) string {
	parts := []string{ev.At.Local().Format("2006-01-02T15:04:05.000"), string(ev.Kind), ev.Box}
	if ev.Script != "" {
		parts = append(parts, ev.Script)
	}
	if ev.Exit != nil {
		x := "reason=" + string(ev.Exit.Reason) + " code=" + strconv.Itoa(ev.Exit.Code)
		if ev.Exit.Detail != "" {
			x += " " + ev.Exit.Detail
		}
		parts = append(parts, x)
	}
	if ev.Detail != "" {
		parts = append(parts, ev.Detail)
	}
	return strings.Join(parts, " ")
}

// sortedKeys helps with stable output.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// interrupted is the exit code at the end of a following command: 130 after Ctrl+C, otherwise 0.
func interrupted(ctx context.Context) int {
	if ctx.Err() != nil {
		return exitInterrupt
	}
	return 0
}
