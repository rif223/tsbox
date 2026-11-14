package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rif223/tsbox/client"
	"github.com/rif223/tsbox/internal/project"
)

func loadProject(file string) (*project.Project, int) {
	p, err := project.Load(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsbox:", err)
		return nil, 2
	}
	return p, 0
}

// projectBoxes returns the boxes of a project at the daemon (by label).
func projectBoxes(ctx context.Context, c *client.Client, name string) (map[string]client.Box, error) {
	all, err := c.List(ctx, true)
	if err != nil {
		return nil, err
	}
	out := map[string]client.Box{}
	for _, b := range all {
		if b.Labels[client.LabelProject] == name {
			out[b.Name] = b
		}
	}
	return out, nil
}

// up [-d] [-f tsbox.json]: create boxes, or recreate them when changed, and start them.
func upCmd(args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	file := fs.String("f", project.FileName, "project file")
	detach := fs.Bool("d", false, "in the background (otherwise follow the logs; Ctrl+C stops the boxes)")
	usageFor(fs, "up [-d] [-f tsbox.json]")
	if _, ok := parsePos(fs, args, 0, 0); !ok {
		return 2
	}
	p, code := loadProject(*file)
	if p == nil {
		return code
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		info, _ := c.Info(ctx)
		if !info.Isolated {
			fmt.Fprintln(os.Stderr, "WARNING: driver \"dev\": NO isolation. Only for your own, trusted code.")
		}
		existing, err := projectBoxes(ctx, c, p.Name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tsbox:", err)
			return 1
		}
		started := time.Now()
		want := map[string]bool{}
		for _, r := range p.Boxes {
			spec := r.Spec
			want[spec.Name] = true
			spec.Labels[client.LabelProject] = p.Name
			spec.Labels[client.LabelService] = r.Key
			spec.Labels[client.LabelSpecHash] = r.Hash
			env := client.LookupEnv(spec)
			if ex, ok := existing[spec.Name]; ok {
				if ex.Labels[client.LabelSpecHash] == r.Hash {
					if running(ex.State) {
						fmt.Printf("%s: up to date\n", spec.Name)
						continue
					}
					if _, err := c.Start(ctx, spec.Name, env); err != nil {
						fmt.Fprintf(os.Stderr, "tsbox: %s: %v\n", spec.Name, err)
						return 1
					}
					fmt.Printf("%s: started\n", spec.Name)
					continue
				}
				if err := c.Remove(ctx, spec.Name, true); err != nil {
					fmt.Fprintf(os.Stderr, "tsbox: %s: %v\n", spec.Name, err)
					return 1
				}
				fmt.Printf("%s: changed, recreating\n", spec.Name)
			}
			if _, err := c.Create(ctx, client.CreateRequest{Spec: spec, Start: true, Env: env}); err != nil {
				fmt.Fprintf(os.Stderr, "tsbox: %s: ", spec.Name)
				return printAPIErr(err)
			}
			fmt.Printf("%s: created and started\n", spec.Name)
		}
		for name := range existing {
			if !want[name] {
				if err := c.Remove(ctx, name, true); err == nil {
					fmt.Printf("%s: removed (no longer in %s)\n", name, *file)
				}
			}
		}
		if *detach {
			return 0
		}
		// Follow the logs of all boxes until all end or Ctrl+C.
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, r := range p.Boxes {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				for l, err := range c.Logs(ctx, name, client.LogQuery{Follow: true, Since: started}) {
					if err != nil {
						return
					}
					mu.Lock()
					l.Msg = name + "/" + l.Script + " | " + l.Msg
					printLogLine(l, false, false)
					mu.Unlock()
				}
			}(r.Spec.Name)
		}
		wg.Wait()
		if ctx.Err() != nil {
			sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			fmt.Fprintln(os.Stderr, "tsbox: stopping boxes ...")
			for _, r := range p.Boxes {
				_, _ = c.Stop(sctx, r.Spec.Name, 0)
			}
			return exitInterrupt
		}
		return 0
	})
}

// down [-f tsbox.json]: stop and remove all boxes of the project (kv data stays).
func downCmd(args []string) int {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	file := fs.String("f", project.FileName, "project file")
	usageFor(fs, "down [-f tsbox.json]")
	if _, ok := parsePos(fs, args, 0, 0); !ok {
		return 2
	}
	p, code := loadProject(*file)
	if p == nil {
		return code
	}
	return withClient(func(ctx context.Context, c *client.Client) int {
		existing, err := projectBoxes(ctx, c, p.Name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tsbox:", err)
			return 1
		}
		if len(existing) == 0 {
			fmt.Printf("%s: no boxes to remove\n", p.Name)
			return 0
		}
		code := 0
		for _, name := range sortedKeys(existing) {
			if err := c.Remove(ctx, name, true); err != nil {
				fmt.Fprintf(os.Stderr, "tsbox: %s: %v\n", name, err)
				code = 1
				continue
			}
			fmt.Printf("%s: removed\n", name)
		}
		return code
	})
}
