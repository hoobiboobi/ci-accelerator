package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
)

// hubCommand runs a command of the hub inside its pod, on its data
// directory: authorised by Kubernetes (exec into the pod), audited as
// "local".
//
//	hub grant <role> gitlab:<host>/<id>   break-glass: a role without signing in
//	hub backup <file>                     a copy of fleet.db, while the hub runs
//	hub restore <file>                    applied at the hub's next start
func hubCommand(cmd string, args []string) error {
	fs := flag.NewFlagSet("hub "+cmd, flag.ContinueOnError)
	data := fs.String("data", "/data", "the hub's data directory")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), hubCommandsUsage)
		fs.PrintDefaults()
	}
	if cmd == "help" {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}
	pos, err := parseInterspersed(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch cmd {
	case "grant":
		if len(pos) != 2 {
			return errors.New("usage: ci-accelerator hub grant [-data dir] <viewer|operator|owner> gitlab:<host>/<user id>")
		}
		role, err := fleet.ParseRole(pos[0])
		if err != nil {
			return err
		}
		reg, err := fleet.OpenRegistry(ctx, *data)
		if err != nil {
			return err
		}
		defer reg.Close()
		if _, err := reg.Grant(ctx, fleet.OriginLocal, fleet.OriginLocal, pos[1], role); err != nil {
			return err
		}
		fmt.Printf("%s is %s now; the running hub sees it within 15 seconds.\n", pos[1], role)
		return nil
	case "backup":
		if len(pos) != 1 {
			return errors.New("usage: ci-accelerator hub backup [-data dir] <file> (then kubectl cp it out)")
		}
		reg, err := fleet.OpenRegistry(ctx, *data)
		if err != nil {
			return err
		}
		defer reg.Close()
		if err := reg.Backup(ctx, pos[0]); err != nil {
			return err
		}
		if err := reg.Record(ctx, fleet.OriginLocal, "hub.backup", "hub", pos[0]); err != nil {
			return err
		}
		fmt.Printf("%s written (readable by the hub's user only). It holds the members, roles, token hashes, the clusters' settings with their sealed secrets, and the audit; keep it like a secret, and delete it here once copied out.\n", pos[0])
		return nil
	case "restore":
		if len(pos) != 1 {
			return errors.New("usage: ci-accelerator hub restore [-data dir] <file> (kubectl cp it into the pod first)")
		}
		if err := fleet.StageRestore(ctx, *data, pos[0]); err != nil {
			return err
		}
		fmt.Println("Staged. Restart the hub (kubectl rollout restart deployment/<hub>): it applies the backup at its start. Members, roles and the clusters' settings and secrets return as they were in the backup (review Members and Access afterwards); every API and join token is voided.")
		return nil
	}
	return fmt.Errorf("unknown hub command %q: grant, backup, restore (ci-accelerator hub help)", cmd)
}

// hubCommandsUsage is the commands' help (ci-accelerator hub help, or -h
// after a command).
const hubCommandsUsage = `Commands of a hub, run in its pod on its data directory:
  ci-accelerator hub grant [-data dir] <viewer|operator|owner> gitlab:<host>/<user id>
      a role without signing in (break-glass)
  ci-accelerator hub backup [-data dir] <file>
      a copy of fleet.db, while the hub runs
  ci-accelerator hub restore [-data dir] <file>
      applied at the hub's next start, which voids every API token
Flags:
`

// parseInterspersed parses flags given before, between or after the
// positional arguments ("hub grant owner gitlab:g/1 -data /x"), and
// returns the positional ones. "--" ends the flags.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		// Parse stops at the first argument that is not a flag, or after "--".
		if used := args[:len(args)-len(rest)]; len(used) > 0 && used[len(used)-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			break
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
	return pos, nil
}

// chartVersion is the chart version the install commands a hub generates
// name: its own chart's (set by the chart), else the binary's version
// (without the commit versionString adds).
func chartVersion() string {
	if v := os.Getenv("CI_ACCELERATOR_CHART_VERSION"); v != "" {
		return v
	}
	return strings.TrimPrefix(version, "v")
}
