package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// helpShown is set when a command printed its help (-h/--help); main then exits with 0.
var helpShown bool

// usageLines holds the usage line of each command (for help and error messages).
var usageLines = map[*flag.FlagSet]string{}

// usageFor sets the usage of a command: the line and all flags, written as in the docs (--name, -x).
func usageFor(fs *flag.FlagSet, line string) {
	usageLines[fs] = "tsbox " + line
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintln(w, "Usage: tsbox "+line)
		printFlags(w, fs)
	}
}

func dashes(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

func printFlags(w io.Writer, fs *flag.FlagSet) {
	n := 0
	fs.VisitAll(func(*flag.Flag) { n++ })
	if n == 0 {
		return
	}
	fmt.Fprintln(w, "\nFlags:")
	fs.VisitAll(func(f *flag.Flag) {
		typ, usage := flag.UnquoteUsage(f)
		head := "  " + dashes(f.Name)
		if typ != "" {
			head += " " + typ
		}
		if len(head) < 24 {
			head += strings.Repeat(" ", 24-len(head))
		} else {
			head += "\n" + strings.Repeat(" ", 24)
		}
		if def := f.DefValue; def != "" && def != "0" && def != "false" && def != "0s" {
			if typ == "string" {
				def = fmt.Sprintf("%q", def)
			}
			usage += " (default " + def + ")"
		}
		fmt.Fprintln(w, head+usage)
	})
}

// parsePos parses flags before, between and after the positional arguments and checks their number
// (max < 0: unlimited). Help goes to stdout; errors print one line, the usage line and a hint.
func parsePos(fs *flag.FlagSet, args []string, min, max int) ([]string, bool) {
	fs.SetOutput(io.Discard)
	var pos []string
	rest := args
	for {
		err := fs.Parse(rest)
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			helpShown = true
			return nil, false
		}
		if err != nil {
			usageErr(fs, flagErr(fs, err))
			return nil, false
		}
		if fs.NArg() == 0 {
			break
		}
		if consumed := len(rest) - fs.NArg(); consumed > 0 && rest[consumed-1] == "--" {
			pos = append(pos, fs.Args()...) // everything after "--" is positional
			break
		}
		pos, rest = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	switch {
	case len(pos) < min:
		usageErr(fs, "missing argument")
		return nil, false
	case max >= 0 && len(pos) > max:
		extra := pos[max:]
		if max == 0 {
			usageErr(fs, fmt.Sprintf("takes no arguments (got %q)", strings.Join(extra, " ")))
		} else {
			usageErr(fs, fmt.Sprintf("too many arguments (unexpected %q)", strings.Join(extra, " ")))
		}
		return nil, false
	}
	return pos, true
}

// parseNames: flags and any number (at least min) of names.
func parseNames(fs *flag.FlagSet, args []string, min int) ([]string, bool) {
	return parsePos(fs, args, min, -1)
}

// parseArgs: flags and at most one positional argument (required with need).
func parseArgs(fs *flag.FlagSet, args []string, need bool) (string, bool) {
	min := 0
	if need {
		min = 1
	}
	pos, ok := parsePos(fs, args, min, 1)
	if !ok || len(pos) == 0 {
		return "", ok
	}
	return pos[0], true
}

// usageErr prints "tsbox <cmd>: <msg>", the usage line and a hint to -h.
func usageErr(fs *flag.FlagSet, msg string) {
	fmt.Fprintf(os.Stderr, "tsbox %s: %s\n", fs.Name(), msg)
	if line := usageLines[fs]; line != "" {
		fmt.Fprintln(os.Stderr, "Usage: "+line)
	}
	fmt.Fprintf(os.Stderr, "Run \"tsbox %s -h\" for help.\n", fs.Name())
}

var (
	reUnknown = regexp.MustCompile(`^flag provided but not defined: -+(\S+)$`)
	reInvalid = regexp.MustCompile(`^invalid (?:boolean )?value "(.*)" for flag -+(\S+?): (.*)$`)
	reNeeds   = regexp.MustCompile(`^flag needs an argument: -+(\S+)$`)
)

// flagErr turns the messages of the flag package into readable ones.
func flagErr(fs *flag.FlagSet, err error) string {
	s := err.Error()
	if m := reUnknown.FindStringSubmatch(s); m != nil {
		msg := "unknown flag " + dashes(m[1])
		if sug := suggestFlag(fs, m[1]); sug != "" {
			msg += " (did you mean " + dashes(sug) + "?)"
		}
		return msg
	}
	if m := reNeeds.FindStringSubmatch(s); m != nil {
		return dashes(m[1]) + " needs a value"
	}
	if m := reInvalid.FindStringSubmatch(s); m != nil {
		val, name, why := m[1], m[2], m[3]
		msg := fmt.Sprintf("%s: invalid value %q", dashes(name), val)
		if why != "parse error" && why != "value out of range" {
			return msg + ": " + why
		}
		if f := fs.Lookup(name); f != nil {
			if g, ok := f.Value.(flag.Getter); ok {
				switch g.Get().(type) {
				case time.Duration:
					msg += " (a duration with a unit, e.g. 500ms, 30s, 5m, 1h)"
				case float64:
					msg += " (a number with a dot as decimal separator, e.g. 0.5)"
				case int, int64, uint, uint64:
					msg += " (a whole number)"
				case bool:
					msg += " (true or false)"
				}
			}
		}
		return msg
	}
	return s
}

// suggestFlag finds a defined flag that is close to the unknown name.
func suggestFlag(fs *flag.FlagSet, name string) string {
	best, bestD := "", 3
	fs.VisitAll(func(f *flag.Flag) {
		d := editDistance(name, f.Name)
		if strings.HasPrefix(f.Name, name) || strings.HasPrefix(name, f.Name) {
			d = min(d, 1)
		}
		if d < bestD {
			best, bestD = f.Name, d
		}
	})
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

var commands = []string{"run", "ps", "logs", "stats", "stop", "start", "restart", "rm", "inspect", "events",
	"up", "down", "daemon", "doctor", "setup", "check", "types", "sdk", "version", "help"}

// suggestCmd finds a command close to an unknown one.
func suggestCmd(name string) string {
	best, bestD := "", 3
	for _, c := range commands {
		if d := editDistance(name, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}
