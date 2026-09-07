package agent

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// Run accepts --config before or after validate, status, or apply.
func Run(args []string, out io.Writer, resolve InterfaceResolver) error {
	configPath := "config.yaml"
	command := ""
	dryRun := false
	policyMap, pathMap := "", ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--help" || arg == "-h":
			_, err := fmt.Fprintln(out, "usage: dualsteer-agent [--config config.yaml] [validate|status|apply|delete] [--dry-run] [--policy-map PATH|id:N --path-map PATH|id:N] (default: validate)")
			return err
		case arg == "--policy-map" || arg == "--path-map":
			i++
			if i == len(args) || args[i] == "" {
				return fmt.Errorf("%s requires a map selector", arg)
			}
			if arg == "--policy-map" {
				policyMap = args[i]
			} else {
				pathMap = args[i]
			}
		case arg == "--config":
			i++
			if i == len(args) || args[i] == "" {
				return errors.New("--config requires a path")
			}
			configPath = args[i]
		case strings.HasPrefix(arg, "--config="):
			configPath = strings.TrimPrefix(arg, "--config=")
			if configPath == "" {
				return errors.New("--config requires a path")
			}
		case arg == "--dry-run":
			dryRun = true
		case arg == "validate" || arg == "status" || arg == "apply" || arg == "delete":
			if command != "" {
				return errors.New("exactly one command is required")
			}
			command = arg
		default:
			return fmt.Errorf("unknown argument %q", arg)
		}
	}
	if command == "" {
		command = "validate"
	}
	if dryRun && command != "apply" {
		return errors.New("--dry-run is only valid with apply")
	}
	file, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	cfg, err := Decode(file)
	if err != nil {
		return err
	}
	if command == "validate" {
		_, err = fmt.Fprintln(out, "configuration valid (syntax and policy only; interfaces and kernel state not checked)")
		return err
	}
	if !dryRun {
		return runLive(command, cfg, policyMap, pathMap, out, resolve)
	}
	plan, err := Prepare(cfg, resolve)
	if err != nil {
		return err
	}
	label := "dry-run: planned configuration; no kernel changes"
	p := plan.Policy
	m := plan.MapPolicy()
	_, err = fmt.Fprintf(out, "%s\nenabled=%t generation=%d mode=%s rttDeltaUs=%d\nleg A: ifname=%s ifindex=%d weight=%d\nleg B: ifname=%s ifindex=%d weight=%d\nplanned ds_policy: generation=%d enabled=%d mode=%d weight_a=%d weight_b=%d rtt_delta_us=%d\nifindex values are diagnostics, not MPTCP endpoint/subflow mappings.\nPer-connection netns/token and generation-matched endpoint IDs are required before live apply.\n", label, *p.Enabled, p.Generation, p.Mode, p.RTTDeltaUS, p.Legs.A.IfName, plan.IfIndexA, *p.Legs.A.Weight, p.Legs.B.IfName, plan.IfIndexB, *p.Legs.B.Weight, m.Generation, m.Enabled, m.Mode, m.WeightA, m.WeightB, m.RTTDeltaUS)
	if err != nil {
		return err
	}
	if p.Connection != nil {
		key, keyErr := p.ConnectionKey()
		if keyErr != nil {
			return keyErr
		}
		desired, pathErr := DesiredPaths(p, key)
		if pathErr != nil {
			return pathErr
		}
		if _, err = fmt.Fprintf(out, "planned connection: netns=%d token=%d\n", key.NetNSInode, key.Token); err != nil {
			return err
		}
		return printPaths(out, desired, p.Generation, true)
	}
	return nil
}

func printPaths(out io.Writer, paths map[PathKey]MapPath, generation uint32, exists bool) error {
	keys := make([]PathKey, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b PathKey) int {
		return cmp.Or(cmp.Compare(a.LocalID, b.LocalID), cmp.Compare(a.RemoteID, b.RemoteID))
	})
	for _, k := range keys {
		v := paths[k]
		if _, err := fmt.Fprintf(out, "path localId=%d remoteId=%d generation=%d access=%d matchesPolicy=%t\n", k.LocalID, k.RemoteID, v.Generation, v.Access, exists && v.Generation == generation); err != nil {
			return err
		}
	}
	return nil
}

func runLive(command string, cfg Config, policyMap, pathMap string, out io.Writer, resolve InterfaceResolver) error {
	if policyMap == "" || pathMap == "" {
		return errors.New("live apply/status/delete require --policy-map and --path-map (pinned paths or id:NUMBER)")
	}
	key, err := cfg.DualSteer.ConnectionKey()
	if err != nil {
		return err
	}
	var plan Plan
	if command == "apply" {
		plan, err = Prepare(cfg, resolve)
		if err != nil {
			return err
		}
		if _, err = DesiredPaths(plan.Policy, key); err != nil {
			return err
		}
	}
	maps, err := OpenKernelMaps(policyMap, pathMap)
	if err != nil {
		return err
	}
	defer maps.Close()
	unlock, err := LockWriters()
	if err != nil {
		return err
	}
	defer unlock()
	switch command {
	case "apply":
		if err := ApplyLive(plan, key, maps); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "applied kernel policy: netns=%d token=%d generation=%d mode=%s; endpoint mappings committed\n", key.NetNSInode, key.Token, plan.Policy.Generation, plan.Policy.Mode)
	case "delete":
		if err := DeleteLive(key, maps); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "deleted kernel policy and paths: netns=%d token=%d (default scheduler fallback)\n", key.NetNSInode, key.Token)
	case "status":
		p, lookupErr := maps.LookupPolicy(key)
		if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
			return lookupErr
		}
		paths, pathErr := maps.Paths(key)
		if pathErr != nil {
			return pathErr
		}
		if errors.Is(lookupErr, ErrNotFound) {
			_, err = fmt.Fprintf(out, "kernel policy absent: netns=%d token=%d (default scheduler fallback); orphan paths=%d\n", key.NetNSInode, key.Token, len(paths))
		} else {
			_, err = fmt.Fprintf(out, "kernel policy: netns=%d token=%d generation=%d enabled=%d mode=%d weight_a=%d weight_b=%d rtt_delta_us=%d\n", key.NetNSInode, key.Token, p.Generation, p.Enabled, p.Mode, p.WeightA, p.WeightB, p.RTTDeltaUS)
		}
		if err != nil {
			return err
		}
		if err := printPaths(out, paths, p.Generation, lookupErr == nil); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "Map contents do not prove scheduler attachment or traffic use; verify kernel/data-plane separately.")
	}
	return err
}
