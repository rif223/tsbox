package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
	"github.com/rif223/tsbox/internal/fsutil"
)

// update [flags] <box>: rebuild a box from its (changed) files or with a new specification, while the old
// one keeps running until the new build passed.
func updateCmd(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	specFile := fs.String("spec", "", "replace the whole specification with this JSON file (BoxSpec, as in tsbox inspect .spec)")
	mem := fs.String("mem", "", "new memory limit of the box, e.g. 256Mi")
	cpu := fs.String("cpu", "", "new CPU limit of the box, e.g. 1")
	pids := fs.String("pids", "", "new PID limit of the box")
	script := fs.String("script", "", "script for --entry and --params (default: the only script)")
	entry := fs.String("entry", "", "new entry file of the script")
	params := fs.String("params", "", "new parameters of the script (JSON)")
	grace := new(time.Duration)
	fs.Var((*seconds)(grace), "t", "deadline for the old scripts, e.g. 10s (default 5s)")
	usageFor(fs, "update [--spec box.json | --mem 256Mi --cpu 1 --entry bot.ts ...] [-t 10s] <box>")
	name, ok := parseArgs(fs, args, true)
	if !ok {
		return 2
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		var spec tsbox.BoxSpec
		if *specFile != "" {
			b, err := fsutil.ReadText(*specFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "tsbox update:", err)
				return 2
			}
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&spec); err != nil {
				fmt.Fprintf(os.Stderr, "tsbox update: %s: %v\n", *specFile, err)
				return 2
			}
			if spec.Name == "" {
				spec.Name = name
			}
		} else {
			info, err := c.Inspect(ctx, name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tsbox update %s: %v\n", name, err)
				return 1
			}
			spec = info.Spec
		}
		if err := applyUpdateFlags(&spec, *mem, *cpu, *pids, *script, *entry, *params); err != nil {
			fmt.Fprintln(os.Stderr, "tsbox update:", err)
			return 2
		}
		absRefs(spec.SDKs)
		req := client.UpdateRequest{Spec: spec, Env: client.LookupEnv(spec)}
		if *grace > 0 {
			req.Grace = grace.String()
		}
		b, err := c.Update(ctx, name, req)
		if err != nil {
			if ae, ok := err.(*client.Error); ok && ae.Code == "build" {
				fmt.Fprintln(os.Stderr, ae.Message)
				return exitTsboxError
			}
			fmt.Fprintf(os.Stderr, "tsbox update %s: %v\n", name, err)
			return 1
		}
		fmt.Printf("%s %s\n", b.Name, b.State)
		return 0
	})
}

func applyUpdateFlags(spec *tsbox.BoxSpec, mem, cpu, pids, script, entry, params string) error {
	if mem != "" {
		v, err := parseBytes(mem)
		if err != nil {
			return fmt.Errorf("--mem: %w", err)
		}
		spec.Limits.Memory = v
	}
	if cpu != "" {
		v, err := strconv.ParseFloat(cpu, 64)
		if err != nil || v <= 0 {
			return fmt.Errorf("--cpu %q: expected a positive number of cores", cpu)
		}
		spec.Limits.CPU = v
	}
	if pids != "" {
		v, err := strconv.Atoi(pids)
		if err != nil || v <= 0 {
			return fmt.Errorf("--pids %q: expected a positive number", pids)
		}
		spec.Limits.PIDs = v
	}
	if entry == "" && params == "" {
		if script != "" {
			return fmt.Errorf("--script needs --entry or --params")
		}
		return nil
	}
	idx := -1
	for i, sc := range spec.Scripts {
		if sc.Name == script || (script == "" && len(spec.Scripts) == 1) {
			idx = i
		}
	}
	if idx < 0 {
		if script == "" {
			return fmt.Errorf("the box has %d scripts: name one with --script", len(spec.Scripts))
		}
		return fmt.Errorf("the box has no script %q", script)
	}
	if entry != "" {
		if _, err := os.Stat(entry); err != nil {
			return fmt.Errorf("--entry: %s", notFound(entry))
		}
		a, err := filepath.Abs(entry)
		if err != nil {
			return err
		}
		spec.Scripts[idx].Entry = a
	}
	if params != "" {
		if !jsonValid(params) {
			return fmt.Errorf("--params is not valid JSON")
		}
		spec.Scripts[idx].Params = json.RawMessage(params)
	}
	return nil
}

// emit [--script s] <box> <topic> [<json>] | emit --event sdk:event <box> [<json>]
func emitCmd(args []string) int {
	fs := flag.NewFlagSet("emit", flag.ContinueOnError)
	script := fs.String("script", "", "only this script (default: all scripts that subscribed)")
	event := fs.String("event", "", "send a raw event of an SDK instead of an inbox message, e.g. echo:tick")
	usageFor(fs, "emit [--script name] <box> <topic> [<json data>]   |   emit --event <sdk>:<event> <box> [<json payload>]")
	pos, ok := parsePos(fs, args, 1, 3)
	if !ok {
		return 2
	}
	req := client.EmitRequest{Script: *script}
	box := pos[0]
	if *event != "" {
		sdkName, ev, ok := strings.Cut(*event, ":")
		if !ok || sdkName == "" || ev == "" {
			usageErr(fs, fmt.Sprintf("--event %q: expected <sdk>:<event>", *event))
			return 2
		}
		if len(pos) > 2 {
			usageErr(fs, "with --event only <box> and the payload are expected")
			return 2
		}
		req.SDK, req.Event = sdkName, ev
		if len(pos) == 2 {
			if !jsonValid(pos[1]) {
				usageErr(fs, "the payload is not valid JSON")
				return 2
			}
			req.Payload = json.RawMessage(pos[1])
		}
	} else {
		if len(pos) < 2 {
			usageErr(fs, "missing topic")
			return 2
		}
		msg := map[string]any{"topic": pos[1]}
		if len(pos) == 3 {
			if !jsonValid(pos[2]) {
				usageErr(fs, "the data is not valid JSON (strings need quotes: '\"text\"')")
				return 2
			}
			msg["data"] = json.RawMessage(pos[2])
		}
		req.Payload, _ = json.Marshal(msg)
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		n, err := c.Emit(ctx, box, req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tsbox emit %s: %v\n", box, err)
			return 1
		}
		fmt.Printf("delivered to %d script(s)\n", n)
		if n == 0 {
			fmt.Fprintln(os.Stderr, "no script has subscribed (inbox.on / on(sdk, event, ...))")
		}
		return 0
	})
}
